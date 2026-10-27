package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Desktop icons: "Local AI" loads the last-used model and opens AnythingLLM;
// "Stop Local AI" stops every local model and closes AnythingLLM, which also ends
// ContextSaver and its admin helper.

func anythingLLMExe() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "AnythingLLM", "AnythingLLM.exe")
}

func consoleStartLocalAI() int {
	name := modelForScript(cfg.AIScript)
	if _, ok := models()[name]; !ok {
		name = "qwen"
	}
	if cur, st := currentModel(); st == "loaded" && cur != "" {
		fmt.Println("Using the loaded model:", cur)
	} else {
		fmt.Println("Loading", name, "…")
		out, err := useModel(name, 600)
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
		fmt.Println("AnythingLLM not found at", exe)
	}
	time.Sleep(2 * time.Second)
	return 0
}

func consoleStopLocalAI() int {
	fmt.Println(stopModel())
	if processRunning("anythingllm.exe") {
		quietCmd("taskkill", "/IM", "AnythingLLM.exe", "/T").Run() // ask it to close first
		for i := 0; i < 20 && processRunning("anythingllm.exe"); i++ {
			time.Sleep(500 * time.Millisecond)
		}
		if processRunning("anythingllm.exe") {
			quietCmd("taskkill", "/IM", "AnythingLLM.exe", "/T", "/F").Run()
		}
		fmt.Println("AnythingLLM closed")
	}
	time.Sleep(2 * time.Second)
	return 0
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

// writeDesktopShortcuts puts the two icons on the Desktop (with AnythingLLM's icon).
func writeDesktopShortcuts(exe string) {
	if !isWin {
		return
	}
	icon := anythingLLMExe()
	if !exists(icon) {
		icon = exe
	}
	ps := ""
	for _, s := range []struct{ name, arg, desc string }{
		{"Local AI", "start", "Load the local model and open AnythingLLM"},
		{"Stop Local AI", "stop", "Stop the local model and close AnythingLLM"},
	} {
		lnk := filepath.Join(desktopDir(), s.name+".lnk")
		ps += fmt.Sprintf("$s=$w.CreateShortcut('%s');$s.TargetPath='%s';$s.Arguments='%s';$s.WorkingDirectory='%s';$s.IconLocation='%s,0';$s.Description='%s';$s.WindowStyle=7;$s.Save();",
			psq(lnk), psq(exe), s.arg, psq(filepath.Dir(exe)), psq(icon), psq(s.desc))
	}
	if err := quietCmd("powershell", "-NoProfile", "-Command", "$w=New-Object -ComObject WScript.Shell;"+ps).Run(); err != nil {
		fmt.Println("Couldn't create the desktop icons:", err)
		return
	}
	fmt.Println("Desktop icons: Local AI, Stop Local AI")
}

func psq(s string) string { return strings.ReplaceAll(s, "'", "''") }
