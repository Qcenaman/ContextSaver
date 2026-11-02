//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Windows setup: programs through winget (as admin through the admin helper when it's
// installed), the Android SDK through Google's sdkmanager. What Windows already has is
// skipped: Office converts documents, Edge/Chrome serve the browser tool, and llama.cpp
// and whisper.cpp are in D:\ already.

type wingetPkg struct {
	id, cmd string
	paths   []string // where it lands when it isn't on this process's PATH yet
}

var winGroups = map[string]struct {
	ask  string
	pkgs []wingetPkg
}{
	"tools": {"Tools ContextSaver uses (Git, GitHub CLI, ctags, 7-Zip, ffmpeg)", []wingetPkg{
		{"Git.Git", "git", []string{`%ProgramFiles%\Git\cmd\git.exe`}},
		{"GitHub.cli", "gh", []string{`%ProgramFiles%\GitHub CLI\gh.exe`}},
		{"UniversalCtags.Ctags", "ctags", []string{`%LOCALAPPDATA%\Microsoft\WinGet\Links\ctags.exe`}},
		{"7zip.7zip", "7z", []string{`%ProgramFiles%\7-Zip\7z.exe`}},
		{"Gyan.FFmpeg", "ffmpeg", []string{`%LOCALAPPDATA%\Microsoft\WinGet\Links\ffmpeg.exe`}},
	}},
	"compilers": {"Compilers for building apps (Go, Rust, Java 17 for Android, Python, Node.js, CMake, Ninja, MinGW gcc for C/C++)", []wingetPkg{
		{"GoLang.Go", "go", []string{`%ProgramFiles%\Go\bin\go.exe`}},
		{"Rustlang.Rustup", "cargo", []string{`%USERPROFILE%\.cargo\bin\cargo.exe`}},
		{"EclipseAdoptium.Temurin.17.JDK", "java", nil},
		{"Python.Python.3.12", "python", []string{`%LOCALAPPDATA%\Programs\Python\Python312\python.exe`}},
		{"OpenJS.NodeJS.LTS", "node", []string{`%ProgramFiles%\nodejs\node.exe`}},
		{"Kitware.CMake", "cmake", []string{`%ProgramFiles%\CMake\bin\cmake.exe`}},
		{"Ninja-build.Ninja", "ninja", []string{`%LOCALAPPDATA%\Microsoft\WinGet\Links\ninja.exe`}},
		{"BrechtSanders.WinLibs.POSIX.UCRT", "gcc", []string{`%LOCALAPPDATA%\Microsoft\WinGet\Links\gcc.exe`}},
	}},
}

// have: the program is there (on PATH, not just the Microsoft Store's python shortcut,
// or where its installer puts it).
func (p wingetPkg) have() bool {
	if p.cmd == "java" && javaHome() != "" {
		return true
	}
	if lp, err := lookPath(p.cmd); err == nil && !strings.Contains(strings.ToLower(lp), `\windowsapps\`) {
		return true
	}
	for _, x := range p.paths {
		if e, err := expandPath(x); err == nil && exists(e) {
			return true
		}
	}
	return false
}

func missingWinget(group string) []wingetPkg {
	var m []wingetPkg
	for _, p := range winGroups[group].pkgs {
		if !p.have() {
			m = append(m, p)
		}
	}
	return m
}

// asAdmin runs a cmd line through the admin helper when it's installed, else directly
// (an installer that needs admin then shows Windows' own prompt).
func asAdmin(out io.Writer, command string) error {
	if helperInstalled() {
		ep, err := startElevated(elevRequest{Command: command, Shell: "cmd"}, out)
		if err != nil {
			return err
		}
		<-ep.done
		if ep.err != nil {
			return ep.err
		}
		if ep.code != nil && *ep.code != 0 {
			return fmt.Errorf("exit %d", *ep.code)
		}
		return nil
	}
	c := quietCmd("cmd", "/c", command)
	c.Stdout, c.Stderr = out, out
	return c.Run()
}

func wingetStep(group string) setupStep {
	g := winGroups[group]
	return setupStep{group, g.ask, true, func() bool { return len(missingWinget(group)) > 0 }, func(out io.Writer) (string, error) {
		if !lookOK("winget") {
			return "", errors.New("winget isn't there (install \"App Installer\" from the Microsoft Store)")
		}
		var done, failed []string
		for _, p := range missingWinget(group) {
			fmt.Fprintln(out, "winget install", p.id)
			err := asAdmin(out, "winget install --id "+p.id+" --exact --silent --accept-package-agreements --accept-source-agreements --disable-interactivity")
			if err != nil && !p.have() {
				failed = append(failed, p.id)
				continue
			}
			done = append(done, p.id)
		}
		if p := filepath.Join(os.Getenv("USERPROFILE"), ".cargo", "bin", "rustup.exe"); exists(p) && strings.Contains(strings.Join(done, " "), "Rustup") {
			runTo(out, filepath.Dir(p), p, "default", "stable")
			runTo(out, filepath.Dir(p), p, "target", "add", "x86_64-unknown-linux-musl", "aarch64-linux-android")
		}
		if len(failed) > 0 {
			return "", fmt.Errorf("winget couldn't install %s (installed: %s)", strings.Join(failed, " "), strings.Join(done, " "))
		}
		return "installed " + strings.Join(done, " ") + " (on the PATH in new windows)", nil
	}}
}

// officeInstalled: Word is registered (convert drives Office).
func officeInstalled() bool {
	return quietCmd("reg", "query", `HKCR\Word.Application\CurVer`).Run() == nil
}

func setupSteps() []setupStep {
	return []setupStep{
		wingetStep("tools"), wingetStep("compilers"),
		{"office", "LibreOffice, because Microsoft Office isn't installed", false,
			func() bool { return !officeInstalled() }, func(out io.Writer) (string, error) {
				return "", errors.New("convert uses Microsoft Office, which isn't installed; office create/edit/read work without it")
			}},
		{"chromium", "Google Chrome for the browser tool (pages that need JavaScript or your sign-in)", true,
			func() bool { return browserExe() == "" }, func(out io.Writer) (string, error) {
				if err := asAdmin(out, "winget install --id Google.Chrome --exact --silent --accept-package-agreements --accept-source-agreements"); err != nil && browserExe() == "" {
					return "", err
				}
				return "Chrome installed", nil
			}},
		{"llama", "llama.cpp's Windows build in " + llamaDir(), true,
			func() bool { return !exists(filepath.Join(llamaDir(), "llama-server.exe")) }, installLlamaCpp},
		{"whisper", "whisper.cpp and its model for audio/video transcripts (about 1.6 GB)", true,
			func() bool { return !whisperReady() }, func(out io.Writer) (string, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
				defer cancel()
				fmt.Fprintln(out, "Downloading whisper.cpp and its model...")
				cli, _, err := ensureWhisper(ctx)
				if err != nil {
					return "", err
				}
				return "whisper ready: " + cli, nil
			}},
		{"android", "the Android SDK in " + androidSDKHome() + ", for building APKs (about 500 MB)", false,
			func() bool { return androidSDK() == "" && javaOK() }, func(out io.Writer) (string, error) {
				return installAndroidSDK(out, "platform-tools", "platforms;android-35", "build-tools;35.0.0")
			}},
		{"ndk", "the Android NDK, for C/C++ and Rust on Android (about 1 GB)", false,
			func() bool { return androidSDK() != "" && androidNDK() == "" }, func(out io.Writer) (string, error) {
				return installAndroidSDK(out, "ndk;27.2.12479018")
			}},
		githubStep(),
	}
}

// whisperReady: whisper-cli.exe and the model are in data\whisper.
func whisperReady() bool {
	cli := false
	filepath.Walk(whisperDir(), func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.EqualFold(fi.Name(), "whisper-cli.exe") {
			cli = true
		}
		return nil
	})
	return cli && exists(filepath.Join(whisperDir(), "ggml-large-v3-turbo.bin"))
}

// installLlamaCpp downloads the newest llama.cpp Windows build: CUDA (with its runtime)
// when there's an NVIDIA GPU, else Vulkan.
func installLlamaCpp(out io.Writer) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	assets, tag, err := githubAssets(ctx, "ggml-org/llama.cpp")
	if err != nil {
		return "", err
	}
	want := pickLlamaAssets(assets, lookOK("nvidia-smi"))
	if len(want) == 0 {
		return "", fmt.Errorf("no Windows build in llama.cpp %s", tag)
	}
	dir := llamaDir()
	n := 0
	for _, name := range want {
		zp := filepath.Join(os.TempDir(), name)
		fmt.Fprintln(out, "Downloading", name, "...")
		if err := download(ctx, assets[name], zp); err != nil {
			return "", err
		}
		k, err := unzipFlat(zp, dir)
		os.Remove(zp)
		if err != nil {
			return "", err
		}
		n += k
	}
	os.WriteFile(filepath.Join(dir, "VERSION"), []byte(tag+"\n"), 0o644)
	return fmt.Sprintf("llama.cpp %s (%s): %d files in %s", tag, strings.Join(want, ", "), n, dir), nil
}

// pickLlamaAssets: the newest CUDA build plus its runtime for NVIDIA, else Vulkan.
func pickLlamaAssets(assets map[string]string, nvidia bool) []string {
	var cuda []string
	vulkan := ""
	for n := range assets {
		switch {
		case !strings.HasSuffix(n, "-x64.zip") || strings.HasPrefix(n, "cudart"):
		case strings.Contains(n, "-bin-win-cuda-"):
			cuda = append(cuda, n)
		case strings.Contains(n, "-bin-win-vulkan-"):
			vulkan = n
		}
	}
	if nvidia && len(cuda) > 0 {
		sort.Slice(cuda, func(i, j int) bool { return cudaVer(cuda[i]) < cudaVer(cuda[j]) })
		best := cuda[len(cuda)-1]
		out := []string{best}
		if rt := "cudart-llama-bin-win-cuda-" + strings.TrimSuffix(best[strings.Index(best, "-bin-win-cuda-")+len("-bin-win-cuda-"):], "-x64.zip") + "-x64.zip"; assets[rt] != "" {
			out = append(out, rt)
		}
		return out
	}
	if vulkan != "" {
		return []string{vulkan}
	}
	return nil
}

// cudaVer makes "…-cuda-12.4-x64.zip" sortable (12004).
func cudaVer(n string) int {
	v := strings.TrimSuffix(n[strings.Index(n, "-cuda-")+6:], "-x64.zip")
	var major, minor int
	fmt.Sscanf(v, "%d.%d", &major, &minor)
	return major*1000 + minor
}
