package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Desktop icons: "Local AI" loads the last-used model and opens AnythingLLM (or, without
// AnythingLLM, ContextSaver Chat in its own window); "Local AI - <model>" loads that model
// and opens its chat; "Stop Local AI" stops the local model ContextSaver started (when idle;
// servers started elsewhere stay up for RemoteAI and others), closes the chat windows and
// AnythingLLM, which also ends ContextSaver and its admin helper. "Local AI" starts the
// admin helper first (or says how to install it); "Stop Local AI" closes everything.

func anythingLLMExe() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "AnythingLLM", "AnythingLLM.exe")
}

func consoleStartLocalAI() int {
	enableVT()
	if msg := startHelper(); msg != "" {
		fmt.Println(msg)
	}
	chat := !useLMStudio() && !exists(anythingLLMExe())
	if !chat {
		spawnApprovals() // the chat asks in its own window
	}
	if useLMStudio() {
		out, err := startLMStudio()
		fmt.Println(out + syncFrontends())
		if err != nil {
			fmt.Println(err)
			waitEnter("Press Enter to close.", 0)
			return 1
		}
		time.Sleep(2 * time.Second)
		return 0
	}
	name := modelForScript(cfg.AIScript)
	if _, ok := models()[name]; !ok {
		name = "qwen"
	}
	if cur, st := currentModel(); st == "loaded" && cur != "" {
		fmt.Println("Using the loaded model:", cur)
	} else {
		fmt.Println("Loading", name, "…")
		out, err := useModel(name, 600, false)
		if err != nil {
			fmt.Println(err)
			waitEnter("Press Enter to close.", 0)
			return 1
		}
		fmt.Println(out)
	}
	if exe := anythingLLMExe(); exists(exe) {
		if !processRunning("anythingllm.exe") {
			c := exec.Command(exe)
			c.Dir = filepath.Dir(exe)
			if err := c.Start(); err != nil {
				fmt.Println("Couldn't open AnythingLLM:", err)
			}
		}
	} else {
		return consoleChat(nil) // no AnythingLLM here: ContextSaver's own chat, in this window
	}
	time.Sleep(2 * time.Second)
	return 0
}

func consoleStopLocalAI() int {
	if useLMStudio() {
		fmt.Println(stopLMStudio(false))
	} else {
		fmt.Println(stopModel(false))
	}
	if !useLMStudio() && processRunning("anythingllm.exe") {
		quietCmd("taskkill", "/IM", "AnythingLLM.exe", "/T").Run() // ask it to close first
		for i := 0; i < 20 && processRunning("anythingllm.exe"); i++ {
			time.Sleep(500 * time.Millisecond)
		}
		if processRunning("anythingllm.exe") {
			quietCmd("taskkill", "/IM", "AnythingLLM.exe", "/T", "/F").Run()
		}
		fmt.Println("AnythingLLM closed")
	}
	if n := killChats(); n > 0 {
		fmt.Println("Closed", n, "chat window(s)")
	}
	if stopOtherContextSavers() > 0 {
		fmt.Println("ContextSaver closed")
	}
	if msg := stopHelper(); msg != "" {
		fmt.Println(msg)
	}
	time.Sleep(2 * time.Second)
	return 0
}

// stopOtherContextSavers ends ContextSaver tool processes (not this one, not the
// elevated helper, which stopHelper handles).
func stopOtherContextSavers() int {
	ps, err := procs()
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range ps {
		if strings.EqualFold(p.name, "contextsaver.exe") && int(p.pid) != os.Getpid() && !strings.Contains(strings.ToLower(p.cmd), " helper") {
			if quietCmd("taskkill", "/PID", fmt.Sprint(p.pid), "/T", "/F").Run() == nil {
				n++
			}
		}
	}
	return n
}

func processRunning(exe string) bool {
	ps, err := procs()
	if err != nil {
		return false
	}
	for _, p := range ps {
		if strings.EqualFold(p.name, exe) {
			return true
		}
	}
	return false
}

// writeDesktopShortcuts puts "Local AI" (with AnythingLLM's icon) and "Stop Local AI" on
// the Desktop and "ContextSaver Chat" in the Start Menu; the "Local AI - <model>" icons
// come from writeModelShortcuts.
func writeDesktopShortcuts(exe string) {
	if !isWin {
		return
	}
	icon := anythingLLMExe()
	style := 7 // AnythingLLM opens its own window; this one only loads the model
	startDesc, stopDesc := "Load the local model and open AnythingLLM", "Stop the local model, close the chat windows and AnythingLLM"
	switch {
	case useLMStudio():
		icon = exe
		startDesc, stopDesc = "Start LM Studio's server and load the local model", "Unload the local model, stop LM Studio's server and close the chat windows"
	case !exists(icon):
		icon, style = exe, 1
		startDesc, stopDesc = "Load the local model and chat with it (ContextSaver Chat)", "Close the chat windows and stop the local model"
	}
	sm := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "ContextSaver Chat.lnk")
	err := writeShortcuts([]shortcut{
		{filepath.Join(desktopDir(), "Local AI.lnk"), exe, "start", startDesc, icon, style},
		{filepath.Join(desktopDir(), "Stop Local AI.lnk"), exe, "stop", stopDesc, icon, 7},
		{sm, exe, "chat", "Chat with the local model, with ContextSaver's tools", exe, 1},
	})
	if err != nil {
		fmt.Println("Couldn't create the desktop icons:", err)
		return
	}
	fmt.Println("Desktop icons: Local AI, Stop Local AI, Local AI - <model> for each model; Start Menu: ContextSaver Chat")
}

// shortcut: one Windows .lnk (style 1 = normal window, 7 = minimized).
type shortcut struct {
	lnk, target, args, desc, icon string
	style                         int
}

// writeShortcuts makes the .lnk files in one PowerShell run.
func writeShortcuts(ss []shortcut) error {
	if !isWin || len(ss) == 0 {
		return nil
	}
	ps := ""
	for _, s := range ss {
		os.MkdirAll(filepath.Dir(s.lnk), 0o755)
		ps += fmt.Sprintf("$s=$w.CreateShortcut('%s');$s.TargetPath='%s';$s.Arguments='%s';$s.WorkingDirectory='%s';$s.IconLocation='%s,0';$s.Description='%s';$s.WindowStyle=%d;$s.Save();",
			psq(s.lnk), psq(s.target), psq(s.args), psq(filepath.Dir(s.target)), psq(s.icon), psq(s.desc), s.style)
	}
	return quietCmd("powershell", "-NoProfile", "-Command", "$w=New-Object -ComObject WScript.Shell;"+ps).Run()
}

func psq(s string) string { return strings.ReplaceAll(s, "'", "''") }
