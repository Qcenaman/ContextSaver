//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func askChatClose(pid int) {
	os.MkdirAll(chatDir(), 0o755)
	os.WriteFile(chatCloseFile(pid), nil, 0o644)
}

// writeModelIcons puts "Local AI - <name>" on the Desktop for every model in models.json
// and removes the icons of models no longer listed; the AnythingLLM "Local AI" icon stays.
func writeModelIcons(exe string, names []string) {
	dir := iconDir()
	var lnks []shortcut
	for _, n := range names {
		desc := strings.TrimPrefix(modelDesc(n), " - ")
		if desc == "" {
			desc = n
		}
		lnks = append(lnks, shortcut{filepath.Join(dir, "Local AI - "+n+".lnk"), exe, "start " + n,
			"Load " + desc + " and open its chat (asks first if another model runs)", exe, 1})
	}
	if err := writeShortcuts(lnks); err != nil {
		fmt.Println("Couldn't create the model icons:", err)
	}
	old, _ := filepath.Glob(filepath.Join(dir, "Local AI - *.lnk"))
	for _, p := range staleModelIcons(old, names, ".lnk") {
		if lnkPointsAt(p, "contextsaver") {
			os.Remove(p)
		}
	}
}

// lnkPointsAt: the shortcut's target path mentions name (a .lnk keeps it as plain and
// UTF-16 text), so only ContextSaver's own icons are removed.
func lnkPointsAt(p, name string) bool {
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	s := strings.ToLower(strings.ReplaceAll(string(b), "\x00", ""))
	return strings.Contains(s, strings.ToLower(name))
}
