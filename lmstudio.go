package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LM Studio support. When LM Studio's server answers and no llama.cpp script server
// does, ContextSaver works with LM Studio instead of starting the llama.cpp scripts:
// the ai command uses the model LM Studio has loaded, and model{} switches with lms.

const lmsBase = "http://127.0.0.1:1234"

// lmsNames maps ContextSaver's model names to LM Studio model keys.
var lmsNames = map[string]string{
	"qwen": "qwen3.6-27b", "qwen-think": "qwen3.6-27b",
	"gemma": "gemma-4-31b-it",
	"oss":   "gpt-oss-120b", "oss-fast": "gpt-oss-120b",
	"oss-free": "gpt-oss-120b-derestricted",
}

type lmsModel struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	State string `json:"state"`
}

func lmsModels() ([]lmsModel, bool) {
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get(lmsBase + "/api/v0/models")
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var r struct {
		Data []lmsModel `json:"data"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&r) != nil {
		return nil, false
	}
	return r.Data, true
}

// lmsLoaded is the chat model LM Studio has loaded ("" if none).
func lmsLoaded() string {
	ms, _ := lmsModels()
	for _, m := range ms {
		if m.State == "loaded" && m.Type != "embeddings" {
			return m.ID
		}
	}
	return ""
}

// lmsMode: LM Studio is the model server in use (its server answers and the llama.cpp port doesn't).
func lmsMode() bool {
	if cfg.AIEndpoint != "" {
		return false
	}
	if _, ok := lmsModels(); !ok {
		return false
	}
	host, port := scriptAddr(firstScript())
	return len(servedModels(host, port)) == 0
}

func lmsExe() string {
	h, _ := os.UserHomeDir()
	if p := filepath.Join(h, ".lmstudio", "bin", "lms.exe"); exists(p) {
		return p
	}
	return "lms"
}

// lmsKey resolves a ContextSaver name or an LM Studio key to an LM Studio key.
func lmsKey(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if k, ok := lmsNames[name]; ok {
		return k
	}
	ms, _ := lmsModels()
	for _, m := range ms {
		if strings.EqualFold(m.ID, name) && m.Type != "embeddings" {
			return m.ID
		}
	}
	return ""
}

func lmsNameList() []string {
	var ns []string
	for n := range lmsNames {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

func lmsStatus() string {
	cur := lmsLoaded()
	var b strings.Builder
	b.WriteString("LM Studio models:\n")
	for _, n := range lmsNameList() {
		mark := ""
		if lmsNames[n] == cur && cur != "" {
			mark = " [loaded]"
		}
		fmt.Fprintf(&b, "%s = %s%s\n", n, lmsNames[n], mark)
	}
	if cur == "" {
		b.WriteString("nothing loaded")
	}
	return strings.TrimSpace(b.String())
}

// lmsUse loads a model in LM Studio after stopping every other local model.
func lmsUse(name string) (string, error) {
	key := lmsKey(name)
	if key == "" {
		return "", fmt.Errorf("unknown model %q; have: %s", name, strings.Join(lmsNameList(), ", "))
	}
	if lmsLoaded() == key {
		return name + " is already loaded in LM Studio", nil
	}
	_, port := scriptAddr(firstScript())
	if w := gpuBusyExcept(port); w != "" {
		return "", errors.New("not switching: " + w + ". Stop that first")
	}
	waitNote := waitForAIJobs(60 * time.Second)
	stopped := stopAllModels(port, false) // also unloads LM Studio's current model
	waitNote += vramWarning(name, key)
	err := runWithTimeout(time.Duration(modelLoadWait+30)*time.Second, lmsExe(), "load", key, "-y")
	if lmsLoaded() == key {
		return name + " loaded in LM Studio" + stoppedNote(stopped) + waitNote, nil
	}
	if err != nil {
		return "", fmt.Errorf("LM Studio couldn't load %s: %v", key, err)
	}
	return name + " is loading in LM Studio" + stoppedNote(stopped) + waitNote + "; check with model{action:list}", nil
}
