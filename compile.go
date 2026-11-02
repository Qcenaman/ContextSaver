package main

// check action:compile — a real build of the project (or one source file) at path for
// linux, windows or android, into out (default <project>/dist/<target>). It answers with
// the files made and their sizes, or only the failing lines.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type compileArgs struct {
	Path    string `json:"path"`
	Target  string `json:"target"`
	Out     string `json:"out"`
	Only    string `json:"only"`
	Release bool   `json:"release"`
	Max     int    `json:"max"`
}

// compileJob: the steps, and where to pick up what they made (copied into out).
type compileJob struct {
	kind    string
	steps   []buildStep
	collect []string // globs relative to root, copied into out; empty = what landed in out
	report  []string // folders (relative to root) that are the result as they are (web, desktop bundles)
}

// isArtifact: a program, package or library worth handing over (not .o or .d files).
func isArtifact(fi os.FileInfo) bool {
	switch strings.ToLower(filepath.Ext(fi.Name())) {
	case ".exe", ".apk", ".aab", ".so", ".dll", ".appimage", ".deb", ".zip", ".msi", ".wasm":
		return true
	case ".o", ".d", ".obj", ".rlib", ".rmeta", ".a", ".lib", ".pdb", ".c", ".h", ".cpp", ".go", ".rs", ".txt", ".log", ".json", ".make", ".cmake":
		return false
	}
	return fi.Mode()&0o111 != 0
}

var targetNames = map[string]string{"linux": "linux", "lin": "linux", "windows": "windows", "win": "windows",
	"exe": "windows", "android": "android", "apk": "android", "phone": "android"}

func toolCompile(raw json.RawMessage) (string, error) {
	var a compileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("bad arguments: %v", err)
	}
	t := strings.ToLower(strings.TrimSpace(a.Target))
	if t == "" {
		t = hostTarget()
	}
	target, ok := targetNames[t]
	if !ok {
		return "", errors.New("target must be linux, windows or android")
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", errors.New("compile needs path (project folder or one source file)")
	}
	p, err := expandPath(a.Path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("path not found: %s", p)
	}
	root, file := p, ""
	if !st.IsDir() {
		root, file = filepath.Dir(p), p
	}
	out := filepath.Join(root, "dist", target)
	if a.Out != "" {
		if out, err = expandPath(a.Out); err != nil {
			return "", err
		}
	}
	if err := checkAllowed(out); err != nil {
		return "", err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	if a.Max <= 0 {
		a.Max = 30
	}
	job, err := compilePlan(root, file, target, out, strings.ToLower(a.Only), a.Release)
	if err != nil {
		return "", err
	}
	start := time.Now()
	logPath := filepath.Join(appHome, "cache", "build", slug(filepath.Base(root))+"-"+target+".log")
	os.MkdirAll(filepath.Dir(logPath), 0o755)
	var full strings.Builder
	for _, s := range job.steps {
		so, se, code, err := ckRunEnv(s.env, s.dir, 30*time.Minute, s.name, s.args...)
		o := so + "\n" + se
		fmt.Fprintf(&full, "==== %s %s (exit %d)\n%s\n", s.name, strings.Join(s.args, " "), code, o)
		if err != nil || code != 0 {
			os.WriteFile(logPath, []byte(full.String()), 0o644)
			why := fmt.Sprintf("exit %d", code)
			if err != nil {
				why = err.Error()
				if strings.Contains(why, "executable file not found") {
					why = s.name + " isn't installed" + toolHint(s.name)
				}
			}
			lines := buildErrLines(o, a.Max)
			if len(lines) == 0 {
				lines = strings.Split(ckFirstLines(strings.TrimSpace(o), a.Max), "\n")
			}
			return fmt.Sprintf("✗ %s %s: %s\n  %s\nFull log: %s", job.kind, s.label, why, strings.Join(lines, "\n  "), logPath), nil
		}
	}
	os.WriteFile(logPath, []byte(full.String()), 0o644)
	for _, g := range job.collect {
		ms, _ := filepath.Glob(filepath.Join(root, g))
		for _, m := range ms {
			if fi, err := os.Stat(m); err == nil && !fi.IsDir() && fi.ModTime().After(start.Add(-2*time.Second)) && (isArtifact(fi) || isELF(m)) {
				copyPlain(m, filepath.Join(out, filepath.Base(m)))
				os.Chmod(filepath.Join(out, filepath.Base(m)), fi.Mode().Perm())
			}
		}
	}
	made := newFiles(out, start)
	for _, r := range job.report {
		if d := filepath.Join(root, r); exists(d) {
			if fs := newFiles(d, start); len(fs) > 0 {
				made = append(made, d+"/ (as built):")
				made = append(made, fs...)
			}
		}
	}
	if len(made) == 0 {
		return fmt.Sprintf("%s build for %s finished, but no new files are in %s. Full log: %s", job.kind, target, out, logPath), nil
	}
	return fmt.Sprintf("✓ %s for %s in %s:\n%s", job.kind, target, time.Since(start).Round(time.Second), strings.Join(made, "\n")), nil
}

// newFiles: files in dir written since start, with sizes (largest first, 15 at most).
func newFiles(dir string, start time.Time) []string {
	type f struct {
		p string
		n int64
	}
	var fs []f
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && fi.ModTime().After(start.Add(-2*time.Second)) {
			fs = append(fs, f{p, fi.Size()})
		}
		return nil
	})
	sort.Slice(fs, func(i, j int) bool { return fs[i].n > fs[j].n })
	var out []string
	for i, x := range fs {
		if i == 15 {
			out = append(out, fmt.Sprintf("…and %d more", len(fs)-15))
			break
		}
		out = append(out, fmt.Sprintf("%s (%s)", x.p, hb(x.n)))
	}
	return out
}

func toolHint(name string) string {
	if isWin {
		switch {
		case strings.Contains(name, "android") || strings.Contains(name, "ndk"):
			return " (contextsaver setup: Android SDK + NDK)"
		case name == "cargo" || name == "rustc":
			return " (contextsaver setup installs Rust; then open a new window)"
		case name == "pyinstaller":
			return " (pip install pyinstaller)"
		case name == "wsl":
			return " (wsl --install)"
		case name == "make" || name == "mingw32-make" || name == "cmake" || name == "gcc" || name == "g++" || name == "go" || name == "npm" || name == "npx":
			return " (contextsaver setup installs the compilers; then open a new window)"
		}
		return ""
	}
	switch {
	case strings.Contains(name, "mingw"):
		return " (sudo apt install mingw-w64)"
	case strings.Contains(name, "android") || strings.Contains(name, "ndk"):
		return " (contextsaver setup: Android SDK + NDK)"
	case name == "cargo" || name == "rustc":
		return " (sudo apt install cargo, or rustup)"
	case name == "go", name == "gcc", name == "g++", name == "cmake", name == "make", name == "npm", name == "npx":
		return " (contextsaver setup installs the compilers)"
	case name == "pyinstaller":
		return " (pip install pyinstaller)"
	}
	return ""
}

func exeName(base, target string) string {
	if target == "windows" {
		return base + ".exe"
	}
	return base
}

// androidNDK: ANDROID_NDK_HOME, else the newest NDK in the SDK ("" = none).
func androidNDK() string {
	if d := os.Getenv("ANDROID_NDK_HOME"); d != "" && exists(d) {
		return d
	}
	if sdk := androidSDK(); sdk != "" {
		ms, _ := filepath.Glob(filepath.Join(sdk, "ndk", "*"))
		sort.Strings(ms)
		if len(ms) > 0 {
			return ms[len(ms)-1]
		}
	}
	return ""
}

func ndkClang(cxx bool) string {
	n := "aarch64-linux-android24-clang"
	if cxx {
		n += "++"
	}
	host := "linux-x86_64"
	if isWin {
		host, n = "windows-x86_64", n+".cmd"
	}
	if ndk := androidNDK(); ndk != "" {
		return filepath.Join(ndk, "toolchains", "llvm", "prebuilt", host, "bin", n)
	}
	return n
}

func cCompiler(target string, cxx bool) string {
	switch target {
	case hostTarget():
		// the system's own compiler (MinGW gcc on Windows)
	case "windows":
		if cxx {
			return "x86_64-w64-mingw32-g++"
		}
		return "x86_64-w64-mingw32-gcc"
	case "android":
		return ndkClang(cxx)
	}
	if cxx {
		return "g++"
	}
	return "gcc"
}

func compilePlan(root, file, target, out, only string, release bool) (compileJob, error) {
	has := func(names ...string) bool {
		for _, n := range names {
			if exists(filepath.Join(root, n)) {
				return true
			}
		}
		return false
	}
	want := func(k string) bool { return only == "" || only == k }
	name := slug(filepath.Base(root))
	if file != "" {
		name = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		switch strings.ToLower(filepath.Ext(file)) {
		case ".go":
			return goJob(root, file, target, filepath.Join(out, exeName(name, target))), nil
		case ".c", ".cpp", ".cc", ".cxx":
			if isWin && target == "linux" {
				return wslCJob(root, []string{file}, name, out)
			}
			return cJob(root, []string{file}, target, filepath.Join(out, exeName(name, target))), nil
		case ".rs":
			return rustcJob(root, file, target, filepath.Join(out, exeName(name, target))), nil
		}
	}
	if isWin && target == "linux" && file == "" && (want("c") || only == "") && !has("go.mod", "Cargo.toml", "gradlew", "gradlew.bat", "package.json", "pubspec.yaml") && !hasGlob(root, "*.csproj") && !hasGlob(root, "*.sln") {
		if has("CMakeLists.txt", "Makefile", "makefile") || hasGlob(root, "*.c") || hasGlob(root, "*.cpp") || hasGlob(root, "*.cc") {
			return wslCJob(root, nil, name, out)
		}
	}
	switch {
	case want("android") && has("gradlew", "gradlew.bat") && has("app/build.gradle", "app/build.gradle.kts", "settings.gradle", "settings.gradle.kts"):
		if target != "android" {
			return compileJob{}, errors.New("this is an Android (Gradle) project: use target android")
		}
		gw := gradlew(root)
		task := "assembleDebug"
		if release {
			task = "assembleRelease"
		}
		var env []string
		if sdk := androidSDK(); sdk != "" && os.Getenv("ANDROID_HOME") == "" {
			env = append(env, "ANDROID_HOME="+sdk)
		}
		return compileJob{"gradle", []buildStep{{"gradle " + task, root, env, gw, []string{task, "--console=plain", "-q"}}},
			[]string{"app/build/outputs/apk/*/*.apk", "*/build/outputs/apk/*/*.apk"}, nil}, nil
	case want("flutter") && has("pubspec.yaml"):
		args := map[string][]string{"android": {"build", "apk"}, "linux": {"build", "linux"}, "windows": {"build", "windows"}}[target]
		if !release {
			args = append(args, "--debug")
		}
		return compileJob{"flutter", []buildStep{{"flutter build", root, nil, "flutter", args}},
			[]string{"build/app/outputs/flutter-apk/*.apk"}, []string{"build/linux/x64/release/bundle", "build/linux/x64/debug/bundle",
				"build/windows/x64/runner/Release", "build/windows/x64/runner/Debug"}}, nil
	case want("go") && has("go.mod"):
		pkg := "."
		if !hasGlob(root, "*.go") {
			pkg = "./..."
		}
		if pkg == "." {
			return goJob(root, pkg, target, filepath.Join(out, exeName(name, target))), nil
		}
		return goJob(root, pkg, target, out+string(filepath.Separator)), nil
	case want("rust") && has("Cargo.toml"):
		return cargoJob(root, target), nil
	case want("dotnet") && (hasGlob(root, "*.csproj") || hasGlob(root, "*.sln")):
		rid := map[string]string{"linux": "linux-x64", "windows": "win-x64", "android": "android-arm64"}[target]
		return compileJob{"dotnet", []buildStep{{"dotnet publish", root, nil, "dotnet", []string{"publish", "-c", "Release", "-r", rid,
			"--self-contained", "-p:PublishSingleFile=true", "-o", out}}}, nil, nil}, nil
	case want("c") && has("CMakeLists.txt"):
		bd := filepath.Join(root, "build", target)
		cfgArgs := []string{"-S", root, "-B", bd, "-DCMAKE_BUILD_TYPE=Release", "-DCMAKE_RUNTIME_OUTPUT_DIRECTORY=" + out}
		switch target {
		case hostTarget():
			if isWin && !exists(filepath.Join(bd, "CMakeCache.txt")) { // MinGW unless Visual Studio is what's there
				if lookOK("ninja") {
					cfgArgs = append(cfgArgs, "-G", "Ninja")
				} else if lookOK("mingw32-make") {
					cfgArgs = append(cfgArgs, "-G", "MinGW Makefiles")
				}
			}
		case "windows":
			cfgArgs = append(cfgArgs, "-DCMAKE_SYSTEM_NAME=Windows", "-DCMAKE_C_COMPILER="+cCompiler(target, false),
				"-DCMAKE_CXX_COMPILER="+cCompiler(target, true), "-DCMAKE_RC_COMPILER=x86_64-w64-mingw32-windres")
		case "android":
			ndk := androidNDK()
			if ndk == "" {
				return compileJob{}, errors.New("android C/C++ needs the Android NDK (contextsaver setup)")
			}
			cfgArgs = append(cfgArgs, "-DCMAKE_TOOLCHAIN_FILE="+filepath.Join(ndk, "build", "cmake", "android.toolchain.cmake"),
				"-DANDROID_ABI=arm64-v8a", "-DANDROID_PLATFORM=android-24")
		}
		return compileJob{"cmake", []buildStep{{"cmake configure", root, nil, "cmake", cfgArgs},
			{"cmake build", root, nil, "cmake", []string{"--build", bd, "-j", "--config", "Release"}}}, nil, nil}, nil
	case want("c") && has("Makefile", "makefile"):
		env := []string{}
		if target != "linux" {
			env = append(env, "CC="+cCompiler(target, false), "CXX="+cCompiler(target, true))
		}
		return compileJob{"make", []buildStep{{"make", root, env, makeExe(), append([]string{"-j"}, env...)}},
			[]string{"*", "bin/*", "build/*"}, nil}, nil
	case want("js") && has("package.json"):
		scripts := pkgScripts(filepath.Join(root, "package.json"))
		b, _ := os.ReadFile(filepath.Join(root, "package.json"))
		if strings.Contains(string(b), "electron-builder") {
			flag := map[string]string{"linux": "--linux", "windows": "--win", "android": ""}[target]
			if flag == "" {
				return compileJob{}, errors.New("an Electron app builds for linux or windows")
			}
			return compileJob{"electron", []buildStep{{"electron-builder", root, []string{"CI=1"}, "npx", []string{"--no-install", "electron-builder", flag,
				"--publish", "never", "-c.directories.output=" + out}}}, nil, nil}, nil
		}
		if _, ok := scripts["build"]; !ok {
			return compileJob{}, errors.New("package.json has no build script")
		}
		return compileJob{"npm", []buildStep{{"npm run build", root, []string{"CI=1"}, "npm", []string{"run", "build", "--silent"}}},
			nil, []string{"dist", "build", "out"}}, nil
	case want("python") && (has("main.py", "app.py", "__main__.py") || hasGlob(root, "*.py")):
		if target != hostTarget() {
			return compileJob{}, errors.New("Python programs are packed for the system they're built on: use target " + hostTarget())
		}
		entry := ""
		for _, n := range []string{"main.py", "app.py", "__main__.py", name + ".py"} {
			if has(n) {
				entry = n
				break
			}
		}
		if entry == "" {
			return compileJob{}, errors.New("no main.py or app.py; give path = the entry .py file")
		}
		return compileJob{"pyinstaller", []buildStep{{"pyinstaller", root, nil, "pyinstaller", []string{"--onefile", "--noconfirm",
			"--distpath", out, "--workpath", filepath.Join(root, "build", "pyinstaller"), "--specpath", filepath.Join(root, "build"), "-n", name, entry}}}, nil, nil}, nil
	}
	var srcs []string
	for _, pat := range []string{"*.c", "*.cpp", "*.cc"} {
		m, _ := filepath.Glob(filepath.Join(root, pat))
		srcs = append(srcs, m...)
	}
	if len(srcs) > 0 && want("c") {
		return cJob(root, srcs, target, filepath.Join(out, exeName(name, target))), nil
	}
	return compileJob{}, fmt.Errorf("don't know how to build %s (no go.mod, gradlew, Cargo.toml, CMakeLists.txt, Makefile, package.json, pubspec.yaml, .csproj, .py or C/C++ files)", root)
}

func goJob(root, pkg, target, outPath string) compileJob {
	env := []string{"CGO_ENABLED=0", "GOARCH=amd64", "GOOS=" + target}
	if target == "android" {
		env = []string{"CGO_ENABLED=0", "GOARCH=arm64", "GOOS=android"}
	}
	return compileJob{"go", []buildStep{{"go build", root, env, "go", []string{"build", "-trimpath", "-ldflags=-s -w", "-o", outPath, pkg}}}, nil, nil}
}

func cJob(root string, srcs []string, target, outPath string) compileJob {
	cxx := false
	for _, s := range srcs {
		if !strings.HasSuffix(s, ".c") {
			cxx = true
		}
	}
	args := append([]string{"-O2", "-o", outPath}, srcs...)
	switch target {
	case "windows":
		args = append(args, "-static")
	case "linux":
		args = append(args, "-lm")
	}
	cc := cCompiler(target, cxx)
	return compileJob{filepath.Base(cc), []buildStep{{filepath.Base(cc), root, nil, cc, args}}, nil, nil}
}

var rustTriples = map[string]string{"linux": "", "windows": "x86_64-pc-windows-gnu", "android": "aarch64-linux-android"}

// rustTriple: the --target for target ("" = this system's own).
func rustTriple(target string) string {
	if target == hostTarget() {
		return ""
	}
	if isWin && target == "linux" {
		return "x86_64-unknown-linux-musl" // static, linked with Rust's own lld
	}
	return rustTriples[target]
}

func rustEnv(target string) []string {
	if target == hostTarget() {
		return nil
	}
	switch target {
	case "linux":
		return []string{"CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER=rust-lld"}
	case "windows":
		return []string{"CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER=x86_64-w64-mingw32-gcc"}
	case "android":
		return []string{"CARGO_TARGET_AARCH64_LINUX_ANDROID_LINKER=" + ndkClang(false)}
	}
	return nil
}

func cargoJob(root, target string) compileJob {
	args := []string{"build", "--release"}
	dir := "target/release"
	if t := rustTriple(target); t != "" {
		args = append(args, "--target", t)
		dir = "target/" + t + "/release"
	}
	return compileJob{"cargo", []buildStep{{"cargo build", root, rustEnv(target), "cargo", args}},
		[]string{dir + "/*"}, nil}
}

func rustcJob(root, file, target, outPath string) compileJob {
	args := []string{"-O", "-o", outPath, file}
	if t := rustTriple(target); t != "" {
		args = append(args, "--target", t)
		switch target {
		case "windows":
			args = append(args, "-C", "linker=x86_64-w64-mingw32-gcc")
		case "linux":
			args = append(args, "-C", "linker=rust-lld")
		default:
			args = append(args, "-C", "linker="+ndkClang(false))
		}
	}
	return compileJob{"rustc", []buildStep{{"rustc", root, nil, "rustc", args}}, nil, nil}
}

// hostTarget: the system ContextSaver runs on, the default compile target.
func hostTarget() string {
	if isWin {
		return "windows"
	}
	return "linux"
}

func makeExe() string {
	if isWin && !lookOK("make") && lookOK("mingw32-make") {
		return "mingw32-make"
	}
	return "make"
}

// isELF: a Linux program (built for linux from Windows, where files have no x bit).
func isELF(p string) bool {
	if filepath.Ext(p) != "" {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 4)
	n, _ := f.Read(b)
	return n == 4 && string(b) == "\x7fELF"
}

// wslCJob builds C/C++ for linux on Windows inside WSL (gcc, CMake or make there); srcs
// nil = the project at root.
func wslCJob(root string, srcs []string, name, out string) (compileJob, error) {
	if !wslReady() {
		return compileJob{}, errors.New("C/C++ for linux needs WSL with a Linux distro on Windows (wsl --install), or build it on the Linux laptop")
	}
	o := wslPath(out)
	has := func(n string) bool { return exists(filepath.Join(root, n)) }
	var script string
	var collect []string
	switch {
	case srcs == nil && has("CMakeLists.txt"):
		script = "cmake -S . -B build/linux -DCMAKE_BUILD_TYPE=Release -DCMAKE_RUNTIME_OUTPUT_DIRECTORY=" + shq(o) + " && cmake --build build/linux -j"
	case srcs == nil && (has("Makefile") || has("makefile")):
		script = "make -j"
		collect = []string{"*", "bin/*", "build/*"}
	default:
		if srcs == nil {
			for _, pat := range []string{"*.c", "*.cpp", "*.cc"} {
				m, _ := filepath.Glob(filepath.Join(root, pat))
				srcs = append(srcs, m...)
			}
		}
		cc, rel := "gcc", ""
		for _, s := range srcs {
			if !strings.HasSuffix(s, ".c") {
				cc = "g++"
			}
			r, _ := filepath.Rel(root, s)
			rel += " " + shq(filepath.ToSlash(r))
		}
		script = cc + " -O2 -o " + shq(o+"/"+name) + rel + " -lm"
	}
	return compileJob{"wsl", []buildStep{{"wsl build", root, nil, "wsl", []string{"--cd", root, "-e", "bash", "-lc", script}}}, collect, nil}, nil
}

// wslReady: WSL is there with at least one Linux distro.
func wslReady() bool {
	if !isWin || !lookOK("wsl") {
		return false
	}
	out, err := quietCmd("wsl", "-l", "-q").Output()
	return err == nil && strings.TrimSpace(strings.ReplaceAll(string(out), "\x00", "")) != ""
}

// wslPath: D:\x\y as WSL sees it (/mnt/d/x/y).
func wslPath(p string) string {
	if len(p) >= 2 && p[1] == ':' {
		return "/mnt/" + strings.ToLower(p[:1]) + filepath.ToSlash(p[2:])
	}
	return filepath.ToSlash(p)
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
