package main

import (
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
			return name + " is already loaded", nil
		}
	}
	if w := gpuBusyExcept(port); w != "" {
		return "", errors.New("not switching: " + w + ". Stop that first or switch from the ContextSaver shortcut")
	}
	killPort(port)
	setAIScript(s)
	if _, err := startScript(s); err != nil {
		return "", fmt.Errorf("couldn't start %s: %v", s, err)
	}
	deadline := time.Now().Add(time.Duration(waitSecs) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if len(servedModels(host, port)) > 0 && !serverLoading(host, port) {
			return name + " loaded on port " + fmt.Sprint(port), nil
		}
	}
	return name + " is loading on port " + fmt.Sprint(port) + "; check with model{action:list}", nil
}

func stopModel() string {
	host, port := scriptAddr(firstScript())
	if len(servedModels(host, port)) == 0 && !serverLoading(host, port) && !portOpen(port) {
		return "nothing loaded"
	}
	killPort(port)
	return "model stopped, VRAM freed"
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
