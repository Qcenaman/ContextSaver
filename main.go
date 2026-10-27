package main

import (
	"fmt"
	"os"
)

const appVersion = "1.3.0"

func main() {
	initApp()
	args := os.Args[1:]
	if len(args) == 0 {
		consoleMenu()
		return
	}
	switch args[0] {
	case "model":
		os.Exit(consoleModel(args[1:]))
	case "mcp":
		// --local: AnythingLLM's local models (one small tool, confirmations).
		// Without it: Claude mode, every tool with its full description.
		localMode = os.Getenv("CONTEXTSAVER_LOCAL") == "1"
		for _, a := range args[1:] {
			if a == "--local" {
				localMode = true
			}
		}
		serveMCP(os.Stdin, os.Stdout)
	case "run":
		if len(args) < 2 {
			fmt.Println(`usage: contextsaver run "<saved job name>"`)
			os.Exit(2)
		}
		os.Exit(consoleRunSaved(args[1]))
	case "install":
		consoleInstall()
	case "uninstall":
		consoleUninstall()
	case "helper":
		os.Exit(serveElevatedHelper())
	case "install-helper":
		consoleInstallHelper()
	case "uninstall-helper":
		consoleUninstallHelper()
	case "version":
		fmt.Println("contextsaver", appVersion)
	case "terminal":
		terminal()
	case "macro":
		os.Exit(runMacroCLI(args[1:]))
	default:
		fmt.Println("unknown command:", args[0])
		os.Exit(2)
	}
}
