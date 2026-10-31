package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// git ci: the branch's latest GitHub Actions run via the gh CLI (user session, never
// elevated). Waits up to wait_seconds for it to finish; on failure only the failing
// step's last lines come back.

type ghRun struct {
	ID         int64  `json:"databaseId"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Name       string `json:"workflowName"`
	Sha        string `json:"headSha"`
	URL        string `json:"url"`
}

// ghExec runs gh in dir (a var so tests can fake it).
var ghExec = func(dir string, timeout time.Duration, args ...string) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", errors.New("GitHub CLI (gh) is not installed")
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "NO_COLOR=1")
	hideWindow(cmd)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return "", fmt.Errorf("gh %s timed out", args[0])
	}
	s := strings.TrimSpace(stripCtl(string(out)))
	if err != nil {
		if strings.Contains(s, "gh auth login") {
			return "", errors.New("gh isn't signed in: run `gh auth login` once on the PC")
		}
		return "", fmt.Errorf("gh %s: %s", args[0], clip1(s, 300))
	}
	return s, nil
}

var ciPoll = 10 * time.Second

func gitCI(root string, a gitArgs) (string, error) {
	branch := a.Ref
	if branch == "" {
		b, _, err := gitExec(root, 20*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return "", err
		}
		branch = strings.TrimSpace(b)
	}
	head, _, _ := gitExec(root, 20*time.Second, "rev-parse", "HEAD")
	head = strings.TrimSpace(head)
	wait := waitSecs(a.Wait, 0, 600)
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	var r ghRun
	for {
		out, err := ghExec(root, 30*time.Second, "run", "list", "--branch", branch, "-L", "1", "--json", "databaseId,status,conclusion,workflowName,headSha,url")
		if err != nil {
			return "", err
		}
		var rs []ghRun
		if err := json.Unmarshal([]byte(out), &rs); err != nil {
			return "", fmt.Errorf("gh run list: %s", clip1(out, 200))
		}
		if len(rs) == 0 {
			if time.Now().After(deadline) {
				return "no Actions runs for branch " + branch + " (is there a .github/workflows file, and was it pushed?)", nil
			}
		} else {
			r = rs[0]
			// a run for an older commit means the new one hasn't started yet
			fresh := head == "" || r.Sha == head
			if fresh && r.Status == "completed" || time.Now().After(deadline) {
				break
			}
		}
		time.Sleep(ciPoll)
	}
	sha := r.Sha
	if len(sha) > 7 {
		sha = sha[:7]
	}
	stale := ""
	if head != "" && r.Sha != head {
		stale = " (for an older commit; the latest push hasn't started a run yet)"
	}
	if r.Status != "completed" {
		return fmt.Sprintf("%s %s: %s%s. Check again with git{action:ci,wait_seconds:120}", r.Name, sha, r.Status, stale), nil
	}
	res := fmt.Sprintf("%s %s: %s%s", r.Name, sha, r.Conclusion, stale)
	if r.Conclusion == "success" {
		return res, nil
	}
	log, err := ghExec(root, 60*time.Second, "run", "view", fmt.Sprint(r.ID), "--log-failed")
	if err != nil {
		return res + "\n" + r.URL, nil
	}
	return res + "\n" + failedTail(log, 25), nil
}

// failedTail keeps the last n lines of gh's failed-step log, without the job/step prefix
// columns and timestamps.
func failedTail(log string, n int) string {
	lines := strings.Split(strings.ReplaceAll(log, "\r\n", "\n"), "\n")
	var out []string
	step := ""
	for _, l := range lines {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) == 3 {
			step = parts[1]
			l = parts[2]
		}
		if len(l) > 29 && l[4] == '-' && l[10] == 'T' && strings.Contains(l[:29], "Z") {
			l = strings.TrimSpace(l[strings.Index(l, "Z")+1:])
		}
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	head := ""
	if step != "" {
		head = "failing step: " + step + "\n"
	}
	return head + strings.Join(out, "\n")
}
