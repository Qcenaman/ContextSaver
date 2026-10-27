package main

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// isContextSaverExe reports whether p is a ContextSaver program (by file name). It
// lets a stale ContextSaver helper from an old install path be told apart from an
// unrelated process that happens to hold the elevated pipe.
func isContextSaverExe(p string) bool {
	return strings.EqualFold(filepath.Base(p), "contextsaver.exe")
}

var (
	healMu   sync.Mutex
	healedOK bool
)

// healHelper re-registers the elevated helper so its scheduled task points at the
// current install (one UAC prompt). This runs automatically when an admin command
// finds the helper still serving an old path after the program was updated or
// moved, so the user isn't forced to re-approve a prompt for every command. It
// succeeds at most once per process; if the prompt is declined it may run again
// on a later command.
func healHelper() bool {
	healMu.Lock()
	defer healMu.Unlock()
	if healedOK {
		return true
	}
	if !taskInstalled() {
		return false
	}
	if err := elevateSelf("install-helper"); err != nil {
		return false
	}
	for i := 0; i < 40 && !pipeUp(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	healedOK = pipeUp()
	return healedOK
}
