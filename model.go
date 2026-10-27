package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Model switching: every model has a llama.cpp start script on the same port, so
// AnythingLLM keeps talking to the same address whichever model is loaded.

var defaultModels = map[string]string{
	"qwen":     `D:\llama.cpp\start-qwen3.6-27b-nothink.bat`,
	"oss":      `D:\llama.cpp\start-gpt-oss-120b.bat`,
	"oss-fast": `D:\llama.cpp\start-gpt-oss-120b-SPEC.bat`,
	"oss-free": `D:\llama.cpp\start-gpt-oss-120b-derestricted.bat`,
}

func models() map[string]string {
	if len(cfg.Models) > 0 {
		return cfg.Models
	}
	return defaultModels
}

func modelNames() []string {
	var ns []string
	for n := range models() {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

var aliasRe = regexp.MustCompile(`(?i)--alias[ =]+"?([^\s"^]+)`)

func scriptAlias(script string) string {
	b, err := os.ReadFile(script)
	if err != nil {
		return ""
	}
	if m := aliasRe.FindStringSubmatch(string(b)); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// servedModels returns the model ids answering on port, or nil when nothing runs.
func servedModels(host string, port int) []string {
	c := &aiClient{kind: "openai", base: fmt.Sprintf("http://%s:%d", host, port), hc: &http.Client{Timeout: 3 * time.Second}}
	ms, err := c.models()
	if err != nil {
		return nil
	}
	return ms
}

func serverLoading(host string, port int) bool {
	qc := &http.Client{Timeout: 2 * time.Second}
	resp, err := qc.Get(fmt.Sprintf("http://%s:%d/health", host, port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 503
}

// currentModel names the configured model that is loaded right now ("" if none).
func currentModel() (name, state string) {
	for _, n := range modelNames() {
		s := models()[n]
		host, port := scriptAddr(s)
		ms := servedModels(host, port)
		al := scriptAlias(s)
		for _, m := range ms {
			if al != "" && strings.EqualFold(m, al) {
				if samePath(s, cfg.AIScript) || name == "" {
					name = n
				}
			}
		}
	}
	if name != "" {
		return name, "loaded"
	}
	host, port := scriptAddr(firstScript())
	if ms := servedModels(host, port); len(ms) > 0 {
		return strings.Join(ms, ","), "loaded"
	}
	if serverLoading(host, port) {
		return modelForScript(cfg.AIScript), "loading"
	}
	return "", "none"
}

func firstScript() string {
	if cfg.AIScript != "" {
		return cfg.AIScript
	}
	return models()[modelNames()[0]]
}

func modelForScript(s string) string {
	for n, p := range models() {
		if samePath(p, s) {
			return n
		}
	}
	return filepath.Base(s)
}

func toolModel(a map[string]any) (string, error) {
	act := strings.ToLower(str(a, "action"))
	name := strings.ToLower(str(a, "name", "model"))
	if act == "" {
		if name != "" {
			act = "use"
		} else {
			act = "list"
		}
	}
	switch act {
	case "list", "status":
		return modelStatus(), nil
	case "use", "load", "switch":
		return useModel(name, 45)
	case "stop", "unload":
		return stopModel(), nil
	}
	return "", errors.New("action must be list, use or stop")
}

func modelStatus() string {
	cur, st := currentModel()
	var b strings.Builder
	for _, n := range modelNames() {
		s := models()[n]
		mark := ""
		if n == cur {
			mark = " [" + st + "]"
		}
		if !exists(s) {
			mark += " (script missing)"
		}
		fmt.Fprintf(&b, "%s%s\n", n, mark)
	}
	if cur == "" {
		b.WriteString("nothing loaded")
	} else if st == "loading" {
		b.WriteString("loading " + cur)
	}
	return strings.TrimSpace(b.String())
}

func useModel(name string, waitSecs int) (string, error) {
	s, ok := models()[name]
	if !ok {
		return "", fmt.Errorf("unknown model %q; have: %s", name, strings.Join(modelNames(), ", "))
	}
	if !exists(s) {
		return "", errors.New("script not found: " + s)
	}
	host, port := scriptAddr(s)
	al := scriptAlias(s)
	for _, m := range servedModels(host, port) {
		if al != "" && strings.EqualFold(m, al) {
			setAIScript(s)
			return name + " is already loaded" + stoppedNote(stopAllModels(port, true)), nil
		}
	}
	if w := gpuBusyExcept(port); w != "" {
		return "", errors.New("not switching: " + w + ". Stop that first or switch from the ContextSaver shortcut")
	}
	waitNote := waitForAIJobs(60 * time.Second)
	stopped := stopAllModels(port, false)
	setAIScript(s)
	if _, err := startScript(s); err != nil {
		return "", fmt.Errorf("couldn't start %s: %v", s, err)
	}
	deadline := time.Now().Add(time.Duration(waitSecs) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if len(servedModels(host, port)) > 0 && !serverLoading(host, port) {
			return name + " loaded on port " + fmt.Sprint(port) + stoppedNote(stopped) + waitNote, nil
		}
	}
	return name + " is loading on port " + fmt.Sprint(port) + stoppedNote(stopped) + waitNote + "; check with model{action:list}", nil
}

func stopModel() string {
	_, port := scriptAddr(firstScript())
	waitNote := waitForAIJobs(60 * time.Second)
	stopped := stopAllModels(port, false)
	if len(stopped) == 0 {
		return "nothing loaded"
	}
	return "stopped " + strings.Join(stopped, ", ") + "; VRAM freed" + waitNote
}

func setAIScript(s string) {
	if samePath(cfg.AIScript, s) && cfg.AIEndpoint == "" {
		return
	}
	cfg.AIScript = s
	cfg.AIEndpoint = ""
	saveConfig()
}

func saveConfig() {
	p := filepath.Join(appHome, "config.json")
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &m)
	}
	m["ai_script"] = cfg.AIScript
	delete(m, "ai_endpoint")
	if len(cfg.Models) > 0 {
		m["models"] = cfg.Models
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(p, b, 0o644)
}

// gpuBusyExcept reports other GPU work (ComfyUI, training) but not the model server on port.
func gpuBusyExcept(port int) string {
	ps, err := procs()
	if err != nil {
		return ""
	}
	for _, p := range ps {
		n, c := strings.ToLower(p.name), strings.ToLower(p.cmd)
		if strings.Contains(c, "contextsaver") || strings.Contains(c, "llama-server") {
			continue
		}
		for _, a := range gpuApps {
			if a.match(n, c) {
				return a.what + " is running"
			}
		}
	}
	return ""
}

// aiServerUp reports a model already loaded, so using it needs no extra VRAM.
func aiServerUp() bool {
	if cfg.AIEndpoint != "" || cfg.AIScript == "" {
		return false
	}
	host, port := scriptAddr(cfg.AIScript)
	return len(servedModels(host, port)) > 0
}

func killPort(port int) {
	for i := 0; i < 8; i++ {
		pid := pidOnPort(port)
		if pid <= 0 {
			return
		}
		killTree(pid)
		time.Sleep(time.Second)
	}
}

func portOpen(port int) bool { return pidOnPort(port) > 0 }

// consoleModel: contextsaver model [qwen|oss|oss-fast|stop|list]
func consoleModel(args []string) int {
	if len(args) == 0 || args[0] == "list" || args[0] == "status" {
		fmt.Println(modelStatus())
		return 0
	}
	if args[0] == "stop" {
		fmt.Println(stopModel())
		return 0
	}
	fmt.Println("Loading", args[0], "…")
	out, err := useModel(args[0], 600)
	if err != nil {
		fmt.Println(err)
		waitEnter("Press Enter to close.", 0)
		return 1
	}
	fmt.Println(out)
	return 0
}

// writeModelShortcuts puts one double-click file per model in the app folder.
func writeModelShortcuts(exe string) {
	if !isWin {
		return
	}
	dir := appRoot()
	os.MkdirAll(dir, 0o755)
	for _, n := range modelNames() {
		os.WriteFile(filepath.Join(dir, "Use model - "+n+".cmd"), []byte("@\""+exe+"\" model "+n+"\r\n@timeout /t 4 >nul\r\n"), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "Stop model.cmd"), []byte("@\""+exe+"\" model stop\r\n@timeout /t 3 >nul\r\n"), 0o644)
}

func stoppedNote(stopped []string) string {
	if len(stopped) == 0 {
		return ""
	}
	return " (stopped " + strings.Join(stopped, ", ") + ")"
}

// localModelProcs are programs that serve a local model and hold VRAM while running.
var localModelProcs = []string{"llama-server", "llama-cli", "llama-run", "koboldcpp", "llamafile", "text-generation-launcher", "lemonade-server"}

// stopAllModels stops every local model, not only the one on port: other llama.cpp
// servers on any port, KoboldCpp, models loaded in Ollama and in LM Studio.
// It returns what it stopped.
func stopAllModels(port int, keepPort bool) []string {
	var stopped []string
	keep := 0
	if keepPort {
		keep = pidOnPort(port)
	}
	portPID := pidOnPort(port)
	if pid := portPID; pid > 0 && !keepPort {
		if m := uniq(servedModels("127.0.0.1", port)); len(m) > 0 {
			stopped = append(stopped, strings.Join(m, ","))
		} else {
			stopped = append(stopped, fmt.Sprintf("server on port %d", port))
		}
		killPort(port)
	}
	if ps, err := procs(); err == nil {
		self := os.Getpid()
		for _, p := range ps {
			if p.pid == self || p.pid == portPID || (keep > 0 && p.pid == keep) {
				continue
			}
			n := strings.ToLower(strings.TrimSuffix(p.name, ".exe"))
			n = strings.TrimSuffix(n, ".EXE")
			for _, w := range localModelProcs {
				if strings.HasPrefix(n, w) {
					killTree(p.pid)
					stopped = append(stopped, fmt.Sprintf("%s (pid %d)", p.name, p.pid))
					break
				}
			}
		}
	}
	stopped = append(stopped, unloadOllama()...)
	stopped = append(stopped, unloadLMStudio()...)
	return stopped
}

// unloadOllama asks Ollama to drop every loaded model; Ollama itself keeps running.
func unloadOllama() []string {
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get("http://127.0.0.1:11434/api/ps")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var r struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return nil
	}
	var out []string
	for _, m := range r.Models {
		b, _ := json.Marshal(map[string]any{"model": m.Name, "keep_alive": 0})
		hc2 := &http.Client{Timeout: 30 * time.Second}
		if rr, err := hc2.Post("http://127.0.0.1:11434/api/generate", "application/json", bytes.NewReader(b)); err == nil {
			rr.Body.Close()
			out = append(out, "Ollama "+m.Name)
		}
	}
	return out
}

// unloadLMStudio unloads LM Studio's models with its lms command, when LM Studio has one loaded.
func unloadLMStudio() []string {
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get("http://127.0.0.1:1234/api/v0/models")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var r struct {
		Data []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return nil
	}
	var loaded []string
	for _, m := range r.Data {
		if m.State == "loaded" {
			loaded = append(loaded, "LM Studio "+m.ID)
		}
	}
	if len(loaded) == 0 {
		return nil
	}
	h, _ := os.UserHomeDir()
	lms := filepath.Join(h, ".lmstudio", "bin", "lms.exe")
	if !exists(lms) {
		lms = "lms"
	}
	if err := runWithTimeout(30*time.Second, lms, "unload", "--all"); err != nil {
		return nil
	}
	return loaded
}

func runWithTimeout(d time.Duration, name string, args ...string) error {
	_, err := runQuiet(d, name, args...)
	return err
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// AI jobs of TokenSaver (Claude's tool) and ContextSaver read through the loaded model.
// Switching mid-job would make them fail, so a switch waits for them first.
func aiJobDirs() []string {
	dirs := []string{jobsDir()}
	if isWin {
		dirs = append(dirs, `D:\Claude Apps\TokenSaver\data\jobs`)
	}
	return dirs
}

// runningAIJobs names AI jobs that are running now (kind ai, or web for web ask).
func runningAIJobs() []string { return runningJobsOfKind(aiJobDirs(), "ai", "web") }

// otherJobDirs: TokenSaver's jobs folder.
func otherJobDirs() []string {
	if isWin {
		return []string{`D:\Claude Apps\TokenSaver\data\jobs`}
	}
	return nil
}

// runningJobsOfKind names jobs of these kinds that are running now in dirs.
func runningJobsOfKind(dirs []string, kinds ...string) []string {
	var out []string
	for _, d := range dirs {
		es, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		// newest jobs last; only recent ones can still be running
		for i := len(es) - 1; i >= 0 && i >= len(es)-40; i-- {
			b, err := os.ReadFile(filepath.Join(d, es[i].Name(), "state.json"))
			if err != nil {
				continue
			}
			var st struct {
				Name    string    `json:"name"`
				Kind    string    `json:"kind"`
				Status  string    `json:"status"`
				Updated time.Time `json:"updated"`
			}
			if json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &st) != nil {
				continue
			}
			if st.Status == "running" && containsFold(kinds, st.Kind) && time.Since(st.Updated) < 2*time.Minute {
				out = append(out, st.Name)
			}
		}
	}
	return out
}

// waitForAIJobs waits up to max for running AI jobs to finish. It returns a note
// for the result when it waited or gave up waiting.
func waitForAIJobs(max time.Duration) string {
	jobs := runningAIJobs()
	if len(jobs) == 0 {
		return ""
	}
	start := time.Now()
	for time.Since(start) < max {
		time.Sleep(2 * time.Second)
		if jobs = runningAIJobs(); len(jobs) == 0 {
			return fmt.Sprintf("; waited %ds for an AI job to finish first", int(time.Since(start).Seconds()))
		}
	}
	return "; switched anyway after waiting " + max.String() + " for " + strings.Join(jobs, ", ") + " (it may fail)"
}
