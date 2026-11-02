package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// One desktop icon per model: "contextsaver start <name>" warns when a model or a chat is
// already running and asks first; on yes it closes those chat windows, unloads what runs,
// loads <name> and opens its chat in the icon's own window.

type aiRunning struct {
	chats   []procInfo // ContextSaver chat windows (contextsaver chat / start)
	servers []procInfo // model servers (llama-server and friends), not the search index
	served  []string   // models answering on the chat port
	port    int
}

// procArgs: the arguments after the program in a command line ("C:\a b\x.exe" start qwen).
func procArgs(cmd string) []string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if i := strings.Index(cmd[1:], `"`); i >= 0 {
			return strings.Fields(cmd[i+2:])
		}
		return nil
	}
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return nil
	}
	return f[1:]
}

func isChatProc(p procInfo) bool {
	if !strings.HasPrefix(strings.ToLower(p.name), "contextsaver") {
		return false
	}
	a := procArgs(p.cmd)
	return len(a) > 0 && (a[0] == "chat" || a[0] == "start" && len(a) > 1 || a[0] == "start" && !exists(anythingLLMExe()) && !useLMStudio())
}

func findRunningAI() aiRunning {
	_, port := scriptAddr(firstScript())
	r := aiRunning{port: port, served: uniq(servedModels(guardHost, port))}
	ps, _ := procs()
	self, parent := os.Getpid(), os.Getppid()
	embed := guardPIDOnPort(ragPort)
	for _, p := range ps {
		if p.pid == self || p.pid == parent {
			continue
		}
		if isChatProc(p) {
			r.chats = append(r.chats, p)
			continue
		}
		if p.pid == embed {
			continue
		}
		n := strings.ToLower(p.name)
		for _, w := range localModelProcs {
			if strings.HasPrefix(n, w) {
				r.servers = append(r.servers, p)
				break
			}
		}
	}
	return r
}

func (r aiRunning) any() bool { return len(r.chats) > 0 || len(r.servers) > 0 || len(r.served) > 0 }

func (r aiRunning) describe() string {
	var b strings.Builder
	if len(r.served) > 0 {
		fmt.Fprintf(&b, "  model on port %d: %s\n", r.port, strings.Join(shortModelNames(r.served), ", "))
	}
	for _, p := range r.servers {
		m := ""
		if i := strings.Index(strings.ToLower(p.cmd), ".gguf"); i > 0 {
			m = " " + filepath.Base(strings.Trim(p.cmd[strings.LastIndexAny(p.cmd[:i], ` "/\`)+1:i+5], `"`))
		}
		fmt.Fprintf(&b, "  %s (pid %d)%s, %s RAM\n", p.name, p.pid, m, hb(p.mem))
	}
	for _, p := range r.chats {
		fmt.Fprintf(&b, "  chat window (pid %d)\n", p.pid)
	}
	return b.String()
}

func shortModelNames(ms []string) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = filepath.Base(m)
	}
	return out
}

// consoleStartModel: the per-model desktop icon.
func consoleStartModel(name string) int {
	enableVT()
	name = strings.ToLower(strings.TrimSpace(name))
	if err := modelFileCheck(name); err != nil {
		fmt.Println(err)
		waitEnter("Press Enter to close.", 0)
		return 1
	}
	if _, ok := models()[name]; !ok {
		fmt.Printf("No model called %s in %s (have: %s)\n", name, modelsPath(), strings.Join(modelNames(), ", "))
		waitEnter("Press Enter to close.", 0)
		return 1
	}
	if msg := startHelper(); strings.HasPrefix(msg, "⚠") {
		fmt.Println(msg) // the chat shows it again at the top
	}
	r := findRunningAI()
	if r.any() {
		fmt.Println("A local AI is running already:")
		fmt.Print(r.describe())
		if !askYes("Unload it, close its chat and load "+name+modelDesc(name)+"? [y/N] ", false) {
			fmt.Println("Left everything as it was.")
			time.Sleep(2 * time.Second)
			return 0
		}
		if err := stopRunningAI(r); err != nil {
			fmt.Println(err)
			waitEnter("Press Enter to close.", 0)
			return 1
		}
	}
	fmt.Println("Loading", name+modelDesc(name), "…")
	out, err := useModel(name, 600, true)
	if err != nil {
		fmt.Println(err)
		waitEnter("Press Enter to close.", 0)
		return 1
	}
	fmt.Println(out)
	return consoleChat(nil)
}

// stopRunningAI closes the chat windows and stops every model server, as the user
// just agreed to (busy or started elsewhere included); the search index model stays.
func stopRunningAI(r aiRunning) error {
	if n := closeChats(r.chats); n > 0 {
		fmt.Println("Closed", n, "chat window(s)")
	}
	if pid := guardPIDOnPort(r.port); pid > 0 || len(r.served) > 0 {
		_, own := inspectServer(r.port)
		killServer(r.port, own)
	}
	for _, p := range r.servers {
		if pidAlive(p.pid) {
			killTree(p.pid)
		}
	}
	editReg(func(g *guardReg) {
		for k, e := range g.Servers {
			if e.Port != ragPort {
				delete(g.Servers, k)
			}
		}
	})
	for i := 0; i < 20 && (guardPIDOnPort(r.port) > 0 || anyAlive(r.servers)); i++ {
		time.Sleep(500 * time.Millisecond)
	}
	if guardPIDOnPort(r.port) > 0 {
		return fmt.Errorf("port %d is still in use; nothing new was loaded", r.port)
	}
	fmt.Println("Unloaded; VRAM freed")
	return nil
}

// closeChats asks each chat window to save and close, and ends the ones still open
// after a few seconds.
func closeChats(ps []procInfo) int {
	for _, p := range ps {
		askChatClose(p.pid)
	}
	for i := 0; i < 25 && anyAlive(ps); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	for _, p := range ps {
		if pidAlive(p.pid) {
			killTree(p.pid)
		}
		os.Remove(chatCloseFile(p.pid))
	}
	return len(ps)
}

// iconDir: where the "Local AI - <model>" icons go (tests point it elsewhere).
var iconDir = desktopDir

// chatCloseFile: a chat that sees this file saves itself and closes (Windows has no
// signal for "close politely" to a console program in another window).
func chatCloseFile(pid int) string { return filepath.Join(chatDir(), fmt.Sprintf(".close-%d", pid)) }

func anyAlive(ps []procInfo) bool {
	for _, p := range ps {
		if pidAlive(p.pid) {
			return true
		}
	}
	return false
}

// killChats closes ContextSaver chat windows (Stop Local AI).
func killChats() int {
	return closeChats(findRunningAI().chats)
}

// staleModelIcons: "Local AI - <name>" icons in names that aren't models any more.
func staleModelIcons(files []string, names []string, ext string) []string {
	keep := map[string]bool{}
	for _, n := range names {
		keep[strings.ToLower("Local AI - "+n+ext)] = true
	}
	var out []string
	for _, f := range files {
		if b := strings.ToLower(filepath.Base(f)); strings.HasPrefix(b, "local ai - ") && strings.HasSuffix(b, ext) && !keep[b] {
			out = append(out, f)
		}
	}
	return out
}
