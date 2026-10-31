package main

import (
	"strings"
	"testing"
	"time"
)

func TestFailedTail(t *testing.T) {
	log := "test\tRun go test\t2026-10-31T10:00:00.1234567Z --- FAIL: TestX\ntest\tRun go test\t2026-10-31T10:00:00.2234567Z x_test.go:9: want 2\n"
	got := failedTail(log, 25)
	if !strings.HasPrefix(got, "failing step: Run go test\n") || !strings.Contains(got, "x_test.go:9: want 2") || strings.Contains(got, "2026-10-31T") {
		t.Fatalf("%q", got)
	}
}

func TestGitCI(t *testing.T) {
	dir := t.TempDir()
	if _, code, err := gitExec(dir, 20*time.Second, "init", "-q"); err != nil || code != 0 {
		t.Skip("git not available")
	}
	gitExec(dir, 20*time.Second, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x")
	head, _, _ := gitExec(dir, 20*time.Second, "rev-parse", "HEAD")
	old := ghExec
	t.Cleanup(func() { ghExec = old })
	calls := 0
	ghExec = func(_ string, _ time.Duration, args ...string) (string, error) {
		calls++
		if args[1] == "view" {
			return "test\tRun go test\t2026-10-31T10:00:00.1Z FAIL x", nil
		}
		return `[{"databaseId":7,"status":"completed","conclusion":"failure","workflowName":"test","headSha":"` + strings.TrimSpace(head) + `","url":"u"}]`, nil
	}
	out, err := gitCI(dir, gitArgs{})
	if err != nil || !strings.Contains(out, "failure") || !strings.Contains(out, "failing step: Run go test") || !strings.Contains(out, "FAIL x") {
		t.Fatalf("%q %v", out, err)
	}
}
