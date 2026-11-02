//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsSetupSteps(t *testing.T) {
	names := map[string]bool{}
	for _, st := range setupSteps() {
		names[st.name] = true
	}
	for _, n := range strings.Fields(setupStepNames)[1:] {
		if !names[n] {
			t.Errorf("no setup step %s", n)
		}
	}
}

func TestPickLlamaAssets(t *testing.T) {
	as := map[string]string{
		"llama-b7000-bin-win-cuda-12.4-x64.zip": "a", "llama-b7000-bin-win-cuda-13.1-x64.zip": "b",
		"cudart-llama-bin-win-cuda-13.1-x64.zip": "c", "llama-b7000-bin-win-vulkan-x64.zip": "d",
		"llama-b7000-bin-ubuntu-x64.zip": "e",
	}
	if got := strings.Join(pickLlamaAssets(as, true), " "); got != "llama-b7000-bin-win-cuda-13.1-x64.zip cudart-llama-bin-win-cuda-13.1-x64.zip" {
		t.Fatalf("nvidia: %s", got)
	}
	if got := strings.Join(pickLlamaAssets(as, false), " "); got != "llama-b7000-bin-win-vulkan-x64.zip" {
		t.Fatalf("no nvidia: %s", got)
	}
}

func TestHelperMissingSaysHow(t *testing.T) {
	if helperInstalled() {
		t.Skip("helper installed here")
	}
	if !strings.Contains(helperHowTo(), "install-helper") || !strings.Contains(startHelper(), "install-helper") {
		t.Fatal("no how-to")
	}
	if _, err := startElevated(elevRequest{Command: "echo x", Shell: "cmd"}, nil); err == nil || !strings.Contains(err.Error(), "Tell the user") || !strings.Contains(err.Error(), "install-helper") {
		t.Fatalf("elevated run without helper: %v", err)
	}
}

func TestNULIsNotAConsole(t *testing.T) {
	f, err := os.Open("NUL")
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Fatal("NUL counted as a console, so setup would answer its own questions")
	}
}

func TestWSLPath(t *testing.T) {
	if got := wslPath(`D:\Local Projects\x`); got != "/mnt/d/Local Projects/x" {
		t.Fatal(got)
	}
}

func TestModelIconsFollowModels(t *testing.T) {
	codeHome(t)
	dir := t.TempDir()
	oi := iconDir
	iconDir = func() string { return dir }
	t.Cleanup(func() { iconDir = oi })
	exe := filepath.Join(dir, "contextsaver.exe")
	os.WriteFile(exe, []byte("MZ"), 0o644)
	old := filepath.Join(dir, "Local AI - gone.lnk")
	os.WriteFile(old, []byte("C:\\x\\contextsaver.exe"), 0o644)
	keep := filepath.Join(dir, "Local AI - mine.lnk") // not ContextSaver's
	os.WriteFile(keep, []byte("C:\\other.exe"), 0o644)
	writeModelIcons(exe, []string{"qwen"})
	if !exists(filepath.Join(dir, "Local AI - qwen.lnk")) {
		t.Fatal("no icon for qwen")
	}
	if exists(old) || !exists(keep) {
		t.Fatal("stale icon handling")
	}
}
