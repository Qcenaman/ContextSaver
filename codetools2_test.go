package main

import (
	"strings"
	"testing"
)

func TestForcePushAndTruncateAreDestructive(t *testing.T) {
	for _, c := range []string{`git push --force origin main`, `git push -f`, `: > notes.txt`, `:>D:\x\log.txt`} {
		if dangerousCommand(c) == "" {
			t.Errorf("not flagged: %s", c)
		}
	}
	for _, c := range []string{`git push --follow-tags`, `git push -u origin main`, `echo a > b.txt`} {
		if m := dangerousCommand(c); m != "" {
			t.Errorf("flagged %s (%s)", c, m)
		}
	}
}

func TestFailureLinesWithSourcePosition(t *testing.T) {
	out := "Running...\n    src/Main.java:12: oops it broke\nsome noise\napp.rs:3:5 mismatched types\n"
	got := strings.Join(buildErrLines(out, 10), "\n")
	if !strings.Contains(got, "Main.java:12") || !strings.Contains(got, "app.rs:3") || strings.Contains(got, "noise") {
		t.Fatalf("got %q", got)
	}
}

func TestProcArgsAndChatProcs(t *testing.T) {
	for cmd, want := range map[string]string{
		`"D:\Claude Apps\ContextSaver\app\contextsaver.exe" start qwen`: "start qwen",
		`contextsaver.exe chat`:            "chat",
		`/home/x/contextsaver start gemma`: "start gemma",
		`"C:\x.exe"`:                       "",
	} {
		if got := strings.Join(procArgs(cmd), " "); got != want {
			t.Errorf("%s: got %q", cmd, got)
		}
	}
	if !isChatProc(procInfo{name: "contextsaver.exe", cmd: `"D:\Claude Apps\ContextSaver\app\contextsaver.exe" start qwen`}) {
		t.Fatal("a model icon's chat isn't seen")
	}
	for _, c := range []string{`"D:\a b\contextsaver.exe" mcp`, `"D:\a b\contextsaver.exe" helper`} {
		if isChatProc(procInfo{name: "contextsaver.exe", cmd: c}) {
			t.Fatalf("%s counted as a chat", c)
		}
	}
}

func TestStaleModelIcons(t *testing.T) {
	got := staleModelIcons([]string{"D/Local AI - qwen.lnk", "D/Local AI - old.lnk", "D/Local AI.lnk", "D/Other.lnk"}, []string{"qwen"}, ".lnk")
	if len(got) != 1 || !strings.HasSuffix(got[0], "Local AI - old.lnk") {
		t.Fatalf("got %v", got)
	}
}

func TestSetupUnknownStepNamesTheSteps(t *testing.T) {
	codeHome(t)
	if _, _, err := compactDispatch("sys", map[string]any{"action": "setup", "what": "bogus"}); err == nil || !strings.Contains(err.Error(), "android") {
		t.Fatalf("unknown step: %v", err)
	}
	if approvalAction("sys", map[string]any{"action": "setup"}) == "" {
		t.Fatal("setup runs without approval")
	}
	for c, need := range map[string]bool{"new": true, "release": true, "compile": true, "test": true, "outline": false, "logcat": false, "login": false} {
		if got := approvalAction(c, map[string]any{"path": "x"}) != ""; got != need {
			t.Errorf("%s: approval %v, want %v", c, got, need)
		}
	}
}
