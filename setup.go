package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Setup: what this PC needs for every ContextSaver tool, as steps. "contextsaver setup"
// asks before each one in a console; sys{action:setup,what} lets the local model run them
// itself (admin steps through the admin helper). Every step first checks whether it's
// needed. Output goes to a writer, never stdout while serving the tool.

type setupStep struct {
	name, ask string
	defYes    bool
	needed    func() bool
	run       func(out io.Writer) (string, error)
}

const setupStepNames = "all tools compilers office chromium llama whisper android ndk github"

func setupNeeded() bool {
	for _, st := range setupSteps() {
		if st.defYes && st.needed() {
			return true
		}
	}
	return false
}

// consoleSetup asks before each missing step; only a person at a real console is asked
// (a pipe or NUL must never answer the questions with their defaults).
func consoleSetup() {
	if !isTerminal(os.Stdin) {
		fmt.Println("contextsaver setup asks before each step, so run it in a console window (or ask the local AI: sys{action:setup}).")
		return
	}
	fmt.Printf("ContextSaver %s setup\n\n", appVersion)
	did := 0
	for _, st := range setupSteps() {
		if !st.needed() {
			continue
		}
		q := "[Y/n]"
		if !st.defYes {
			q = "[y/N]"
		}
		if !askYes("\nInstall "+st.ask+"? "+q+" ", st.defYes) {
			continue
		}
		if msg, err := st.run(os.Stdout); err != nil {
			fmt.Println(st.name, "not done:", err)
		} else {
			fmt.Println(msg)
			did++
		}
	}
	if did == 0 {
		fmt.Println("Everything is set up already.")
	}
	if !helperInstalled() && exists(installPath()) {
		if askYes("\nInstall the admin helper, so your local AI can run admin commands without a prompt each time (destructive ones still ask you)? [Y/n] ", true) {
			consoleInstallHelper()
		}
	}
	if did > 0 && isWin {
		fmt.Println("\nNew programs are on the PATH in windows opened from now on.")
	}
	fmt.Println("\nSetup done.")
}

// toolSetup: sys{action:setup,what}: the steps named in what (all = every missing one
// but the NDK), done without questions; the full output goes to cache\setup.log.
func toolSetup(a map[string]any) (string, error) {
	what := strings.ToLower(strings.Join(strings.FieldsFunc(str(a, "what", "name", "step"), func(r rune) bool { return r == ',' || r == ' ' }), " "))
	want := map[string]bool{}
	for _, w := range strings.Fields(what) {
		want[w] = true
	}
	known := map[string]bool{"all": true}
	steps := setupSteps()
	for _, st := range steps {
		known[st.name] = true
	}
	for w := range want {
		if !known[w] {
			return "", fmt.Errorf("unknown setup step %q; steps: %s", w, setupStepNames)
		}
	}
	all := len(want) == 0 || want["all"]
	logPath := filepath.Join(appHome, "cache", "setup.log")
	os.MkdirAll(filepath.Dir(logPath), 0o755)
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer lf.Close()
	var res []string
	for _, st := range steps {
		if !(want[st.name] || all && st.name != "ndk") {
			continue
		}
		if !st.needed() {
			if want[st.name] {
				res = append(res, st.name+": already there")
			}
			continue
		}
		fmt.Fprintf(lf, "==== %s %s\n", time.Now().Format("2006-01-02 15:04:05"), st.name)
		if msg, err := st.run(lf); err != nil {
			res = append(res, "✗ "+st.name+": "+err.Error())
		} else {
			res = append(res, "✓ "+st.name+": "+msg)
		}
	}
	if !helperInstalled() {
		res = append(res, "⚠ "+helperHowTo())
	}
	if len(res) == 0 {
		return "everything is set up already", nil
	}
	return strings.Join(res, "\n") + "\nlog: " + logPath, nil
}

// githubStep: the GitHub sign-in (gh), in the console or with a code the local model
// shows the user.
func githubStep() setupStep {
	return setupStep{"github", "a GitHub sign-in, so ContextSaver can push, release and check Actions", true,
		func() bool { _, err := ghPath(); return err == nil && !ghSignedIn() }, func(out io.Writer) (string, error) {
			gh, err := ghPath()
			if err != nil {
				return "", err
			}
			if !isTerminal(os.Stdin) {
				return gitLogin()
			}
			c := exec.Command(gh, ghLoginArgs()...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, out
			if err := c.Run(); err != nil {
				return "", err
			}
			exec.Command(gh, "auth", "setup-git").Run()
			return "signed in to GitHub" + ensureGitIdentity(), nil
		}}
}

// githubAsset: the download URL and tag of the newest release asset matching pick.
func githubAsset(ctx context.Context, repo string, pick func(string) bool) (string, string, error) {
	names, tag, err := githubAssets(ctx, repo)
	if err != nil {
		return "", "", err
	}
	for n, u := range names {
		if pick(n) {
			return u, tag, nil
		}
	}
	return "", "", fmt.Errorf("no matching download in %s %s", repo, tag)
}

// githubAssets: name -> download URL of the newest release's files.
func githubAssets(ctx context.Context, repo string) (map[string]string, string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var r struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, "", err
	}
	m := map[string]string{}
	for _, a := range r.Assets {
		m[a.Name] = a.URL
	}
	return m, r.Tag, nil
}

// unzipFlat extracts every file of a zip into dir without its folders (llama.cpp's
// zips keep the programs and their libraries in one place).
func unzipFlat(zp, dir string) (int, error) {
	return unzipTo(zp, dir, true)
}

// unzipTo extracts a zip into dir, keeping its folders unless flat.
func unzipTo(zp, dir string, flat bool) (int, error) {
	zr, err := zip.OpenReader(zp)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	n := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel := filepath.FromSlash(f.Name)
		if flat {
			rel = filepath.Base(rel)
		}
		dst := filepath.Join(dir, rel)
		if !strings.HasPrefix(dst, filepath.Clean(dir)+string(filepath.Separator)) {
			continue // a name like ../x
		}
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			rc, _ := f.Open()
			target, _ := io.ReadAll(rc)
			rc.Close()
			os.Remove(dst)
			os.Symlink(filepath.Base(string(target)), dst)
			n++
			continue
		}
		os.MkdirAll(filepath.Dir(dst), 0o755)
		rc, err := f.Open()
		if err != nil {
			return n, err
		}
		mode := os.FileMode(0o644)
		if f.FileInfo().Mode()&0o111 != 0 || !strings.Contains(filepath.Base(f.Name), ".") {
			mode = 0o755
		}
		out, err := os.OpenFile(dst+".part", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			rc.Close()
			return n, err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return n, err
		}
		os.Remove(dst)
		if err := os.Rename(dst+".part", dst); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func runTo(out io.Writer, dir, name string, args ...string) error {
	os.MkdirAll(dir, 0o755)
	c := exec.Command(name, args...)
	c.Dir, c.Stdout, c.Stderr = dir, out, out
	return c.Run()
}

// androidSDKHome: where setup puts the Android SDK (Android Studio's place too).
func androidSDKHome() string {
	if isWin {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "Android", "Sdk")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Android", "Sdk")
}

// androidSDK: ANDROID_HOME, else the usual place when it has platform-tools ("" = none).
func androidSDK() string {
	for _, d := range []string{os.Getenv("ANDROID_HOME"), os.Getenv("ANDROID_SDK_ROOT"), androidSDKHome()} {
		if d != "" && exists(filepath.Join(d, "platform-tools")) {
			return d
		}
	}
	return ""
}

// installAndroidSDK installs Google's command-line tools, then the packages (platform-tools,
// a platform, build-tools, the NDK) with sdkmanager, accepting its licenses.
func installAndroidSDK(out io.Writer, pkgs ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	sdk := androidSDKHome()
	tools := filepath.Join(sdk, "cmdline-tools", "latest")
	sm := filepath.Join(tools, "bin", "sdkmanager")
	url := "https://dl.google.com/android/repository/commandlinetools-linux-13114758_latest.zip"
	if isWin {
		sm += ".bat"
		url = "https://dl.google.com/android/repository/commandlinetools-win-13114758_latest.zip"
	}
	if !exists(sm) {
		zp := filepath.Join(os.TempDir(), "android-cmdline-tools.zip")
		fmt.Fprintln(out, "Downloading Android command-line tools...")
		if err := download(ctx, url, zp); err != nil {
			return "", err
		}
		defer os.Remove(zp)
		tmp := filepath.Join(sdk, "cmdline-tools", "tmp")
		os.RemoveAll(tmp)
		if _, err := unzipTo(zp, tmp, false); err != nil {
			return "", err
		}
		os.RemoveAll(tools)
		if err := os.Rename(filepath.Join(tmp, "cmdline-tools"), tools); err != nil {
			return "", err
		}
		os.RemoveAll(tmp)
	}
	env := os.Environ()
	if jh := javaHome(); jh != "" {
		env = append(env, "JAVA_HOME="+jh)
	}
	yes := exec.Command(sm, "--sdk_root="+sdk, "--licenses")
	yes.Env, yes.Stdin = env, strings.NewReader(strings.Repeat("y\n", 20))
	yes.Run()
	c := exec.CommandContext(ctx, sm, append([]string{"--sdk_root=" + sdk}, pkgs...)...)
	c.Dir, c.Env, c.Stdout, c.Stderr = sdk, env, out, out
	if err := c.Run(); err != nil {
		return "", err
	}
	return strings.Join(pkgs, ", ") + " in " + sdk, nil
}

// javaHome: JAVA_HOME, else a JDK 17+ in the usual install folders ("" = java on PATH or none).
func javaHome() string {
	if j := os.Getenv("JAVA_HOME"); j != "" && exists(j) {
		return j
	}
	if !isWin {
		return ""
	}
	for _, pat := range []string{`Eclipse Adoptium\jdk-*`, `Microsoft\jdk-*`, `Java\jdk-*`, `Zulu\zulu-*`} {
		ms, _ := filepath.Glob(filepath.Join(os.Getenv("ProgramFiles"), pat))
		for i := len(ms) - 1; i >= 0; i-- {
			if exists(filepath.Join(ms[i], "bin", "java.exe")) {
				return ms[i]
			}
		}
	}
	return ""
}

func javaOK() bool { return lookOK("java") || javaHome() != "" }
