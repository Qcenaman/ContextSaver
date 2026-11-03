package main

import (
	"encoding/json"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// the background cleanup never runs in tests; they call cleanupRepos themselves
func init() { repoAutoClean = false }

func rawJ(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	pre := []string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false", "-c", "init.defaultBranch=main"}
	cmd := exec.Command("git", append(pre, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// fakeGitHub stands in for github.com/o/n: a bare repo with a.txt "v1", sub/s.go, tag
// v1 and branch dev, plus a work clone to push more commits from.
func fakeGitHub(t *testing.T) (work string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	codeHome(t)
	oldBase, oldWait, oldClean, oldCheck := repoRemoteBase, repoWait, repoAutoClean, repoCheckFor
	t.Cleanup(func() { repoRemoteBase, repoWait, repoAutoClean, repoCheckFor = oldBase, oldWait, oldClean, oldCheck })
	repoAutoClean, repoWait, repoCheckFor = false, time.Minute, 0
	base := t.TempDir()
	bare := filepath.Join(base, "o", "n.git")
	os.MkdirAll(bare, 0o755)
	gitT(t, bare, "init", "-q", "--bare")
	work = filepath.Join(base, "work")
	gitT(t, base, "clone", "-q", bare, work)
	os.MkdirAll(filepath.Join(work, "sub"), 0o755)
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("v1\n"), 0o644)
	os.WriteFile(filepath.Join(work, "sub", "s.go"), []byte("package sub\n\nfunc Hello() {}\n"), 0o644)
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-q", "-m", "first")
	gitT(t, work, "tag", "v1")
	gitT(t, work, "push", "-q", "origin", "HEAD:refs/heads/main", "v1")
	gitT(t, work, "push", "-q", "origin", "HEAD:refs/heads/dev")
	repoRemoteBase = fileURL(base) + "/"
	return work
}

// pushT commits file = content in work and pushes it to branch on the fake GitHub.
func pushT(t *testing.T, work, file, content, msg, branch string) {
	t.Helper()
	p := filepath.Join(work, filepath.FromSlash(file))
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(content+"\n"), 0o644)
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-q", "-m", msg)
	gitT(t, work, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
}

// readRepo reads the first lines of path; update (nil, true or false) is the user's answer.
func readRepo(t *testing.T, path string, update any) string {
	t.Helper()
	a := map[string]any{"path": path, "mode": "head"}
	if update != nil {
		a["update"] = update
	}
	out, _, err := callTool("read", rawJ(a))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.Contains(out, reposDir()) {
		t.Errorf("read %s shows the local folder: %s", path, out)
	}
	return out
}

func asks(out string) bool { return strings.Contains(out, "GitHub has newer commits") }

func originT(t *testing.T, dir, branch string) string {
	t.Helper()
	return gitT(t, dir, "rev-parse", "refs/remotes/origin/"+branch)
}

func TestRepoSpecParse(t *testing.T) {
	codeHome(t)
	for _, c := range []struct{ in, owner, name, ref, sub string }{
		{"Qcenaman/ContextSaver", "Qcenaman", "ContextSaver", "", ""},
		{"o/n@v1.2.0/sub/x.go", "o", "n", "v1.2.0", "sub/x.go"},
		{`o/n.git\sub\x.go`, "o", "n", "", "sub/x.go"},
		{"https://github.com/o/n/tree/dev/a/b", "o", "n", "dev", "a/b"},
		{"github.com/o/n.github", "o", "n.github", "", ""},
	} {
		if s, ok := parseRepoSpec(c.in); !ok || s.Owner != c.owner || s.Name != c.name || s.Ref != c.ref || s.Sub != c.sub {
			t.Errorf("%q: got %+v %v", c.in, s, ok)
		}
	}
	for _, bad := range []string{filepath.Join(t.TempDir(), "x"), "~/x/y", "./o/n", "../o/n", "x", "o/n/../../etc", "https://gitlab.com/o/n", "o/n@-x", "o/n@x.", "o/n@a@{1}", "o/n@a&calc", "o/...", "o/n.", "C:/x/y", "%TEMP%/x"} {
		if s, ok := parseRepoSpec(bad); ok {
			t.Errorf("%q must not be a repo: %+v", bad, s)
		}
	}
	s, _ := parseRepoSpec("Qcenaman/ContextSaver@v1.0/x")
	if want := filepath.Join(appHome, "repos", "qcenaman", "contextsaver"); s.mainDir() != want {
		t.Errorf("main copy %s, want %s", s.mainDir(), want)
	}
	if want := filepath.Join(appHome, "repos", "qcenaman", "contextsaver@v1.0"); s.copyDir() != want {
		t.Errorf("ref copy %s, want %s", s.copyDir(), want)
	}
	if s, _ := parseRepoSpec("o/con.js"); filepath.Base(s.mainDir()) != "_con.js" {
		t.Errorf("a Windows device name must not be a folder name: %s", s.mainDir())
	}
	if !validRepoRef("feature/x-1") || validRepoRef("a;b") || validRepoRef("a b") {
		t.Error("refs: slashes are fine, shell characters and spaces are not")
	}
	if !isHeavyDir("repos", reposDir()) || isHeavyDir("repos", filepath.Join(appHome, "x", "repos")) {
		t.Error("find must skip the temporary copies, and only them")
	}
}

func TestRepoClonesWhenNeededAndAsksBeforeUpdating(t *testing.T) {
	work := fakeGitHub(t)
	main := filepath.Join(reposDir(), "o", "n")
	if exists(main) {
		t.Fatal("nothing may be cloned before a call needs it")
	}
	if out := readRepo(t, "o/n/a.txt", nil); !strings.Contains(out, "v1") {
		t.Fatalf("first use: %s", out)
	}
	if !exists(filepath.Join(main, ".git")) {
		t.Fatalf("no copy in %s", main)
	}
	fetched, origin := readRepoState(main).Fetched, originT(t, main, "main")
	// GitHub moves on: the copy stays as it is and the call asks first
	pushT(t, work, "a.txt", "v2", "second", "main")
	if out := readRepo(t, "o/n/a.txt", nil); !asks(out) || !strings.Contains(out, "o/n (from ") || !strings.Contains(out, "update:true") || strings.Contains(out, "v1") {
		t.Fatalf("want the question, got %q", out)
	}
	if originT(t, main, "main") != origin || !readRepoState(main).Fetched.Equal(fetched) {
		t.Fatal("asking must not fetch or update the copy")
	}
	// no: the copy as it is, and no question again for that commit
	if out := readRepo(t, "o/n/a.txt", false); !strings.Contains(out, "v1") {
		t.Fatalf("update:false: %s", out)
	}
	if out := readRepo(t, "o/n/a.txt", nil); asks(out) || !strings.Contains(out, "v1") {
		t.Fatalf("asked again after no: %s", out)
	}
	if originT(t, main, "main") != origin {
		t.Fatal("no must not fetch")
	}
	// yes later on: a fast-forward
	if out := readRepo(t, "o/n/a.txt", "yes"); !strings.Contains(out, "v2") {
		t.Fatalf("update yes: %s", out)
	}
	// rewritten history on GitHub asks again; yes resets the copy to it
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("v3\n"), 0o644)
	os.WriteFile(filepath.Join(work, "new.txt"), []byte("new\n"), 0o644)
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-q", "--amend", "-m", "second, rewritten")
	gitT(t, work, "push", "-q", "--force", "origin", "HEAD:refs/heads/main")
	if out := readRepo(t, "o/n/a.txt", nil); !asks(out) {
		t.Fatalf("a newer commit must ask again: %s", out)
	}
	if out := readRepo(t, "o/n/a.txt", true); !strings.Contains(out, "v3") {
		t.Fatalf("update:true: %s", out)
	}
	if out := readRepo(t, "o/n/a.txt", nil); asks(out) || !strings.Contains(out, "v3") {
		t.Fatalf("after the update: %s", out)
	}
	// @ref: a tag, a branch and a commit, each in its own worktree
	first := gitT(t, work, "rev-list", "--max-parents=0", "HEAD")
	for _, ref := range []string{"v1", "dev", first[:10]} {
		if out := readRepo(t, "o/n@"+ref+"/a.txt", nil); !strings.Contains(out, "v1") {
			t.Errorf("@%s: %s", ref, out)
		}
	}
	if !exists(filepath.Join(reposDir(), "o", "n@v1", "a.txt")) {
		t.Error("no n@v1 worktree")
	}
	// a branch copy asks when the branch moves on; tag and commit copies never do
	gitT(t, work, "push", "-q", "origin", "HEAD:refs/heads/dev")
	if out := readRepo(t, "o/n@dev/a.txt", nil); !asks(out) || !strings.Contains(out, "o/n@dev (from ") {
		t.Fatalf("o/n@dev: %s", out)
	}
	if out := readRepo(t, "o/n@dev/a.txt", true); !strings.Contains(out, "v3") {
		t.Fatalf("o/n@dev update:true: %s", out)
	}
	for _, ref := range []string{"v1", first[:10]} {
		if out := readRepo(t, "o/n@"+ref+"/a.txt", nil); asks(out) || !strings.Contains(out, "v1") {
			t.Errorf("@%s must not ask: %s", ref, out)
		}
	}
	// find, compare and outline answer with owner/name paths
	out, _, err := callTool("find", rawJ(map[string]any{"path": []string{"o/n@v1"}, "text": "Hello"}))
	if err != nil || !strings.Contains(out, "s.go") || strings.Contains(out, reposDir()) {
		t.Errorf("find: %q %v", out, err)
	}
	out, _, err = callTool("git", rawJ(map[string]any{"action": "compare", "path": "o/n@v1", "to": "o/n"}))
	for _, want := range []string{"A: o/n@v1 (2 files)", "B: o/n (3 files)", "only in A (0): none", "only in B (1): new.txt", "changed (1): a.txt +1 -1", "same: 1"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("compare: missing %q in %q %v", want, out, err)
		}
	}
	if strings.Contains(out, reposDir()) {
		t.Errorf("compare shows the local folder: %s", out)
	}
	out, _, err = callCompact(rawJ(map[string]any{"c": "outline", "a": map[string]any{"path": "o/n/sub"}}))
	if err != nil || !strings.Contains(out, "Hello") {
		t.Errorf("outline: %q %v", out, err)
	}
}

func TestGitPullUpdatesACopyRightAway(t *testing.T) {
	work := fakeGitHub(t)
	readRepo(t, "o/n/a.txt", nil)
	pushT(t, work, "a.txt", "v2", "second", "main")
	out, _, err := callTool("git", rawJ(map[string]any{"action": "pull", "path": "o/n"}))
	if err != nil || !strings.HasPrefix(out, "updated o/n: branch main, last commit") || !strings.Contains(out, "second") {
		t.Fatalf("pull o/n: %q %v", out, err)
	}
	if out := readRepo(t, "o/n/a.txt", nil); asks(out) || !strings.Contains(out, "v2") {
		t.Fatalf("after pull: %s", out)
	}
	// a branch copy that isn't here yet starts at GitHub's newest commit on it
	gitT(t, work, "push", "-q", "origin", "HEAD:refs/heads/dev")
	out, _, err = callTool("git", rawJ(map[string]any{"action": "pull", "path": "o/n@dev"}))
	if err != nil || !strings.HasPrefix(out, "got o/n@dev: ref dev, last commit") || !strings.Contains(out, "second") {
		t.Fatalf("pull o/n@dev: %q %v", out, err)
	}
	pushT(t, work, "a.txt", "v3", "third", "main")
	out, _, err = callCompact(rawJ(map[string]any{"c": "git", "a": map[string]any{"action": "pull", "path": "o/n"}}))
	if err != nil || !strings.Contains(out, "updated o/n") || !strings.Contains(out, "third") {
		t.Fatalf("pc git pull: %q %v", out, err)
	}
}

func TestRepoBigFirstCloneSaysCallAgain(t *testing.T) {
	fakeGitHub(t)
	s, _ := parseRepoSpec("o/n")
	unlock, err := lockRepo(s.mainDir(), time.Second) // as if another call were cloning it
	if err != nil {
		t.Fatal(err)
	}
	repoWait = 300 * time.Millisecond
	out, _, err := callTool("scan", rawJ(map[string]any{"path": "o/n"}))
	if err != nil || !strings.Contains(out, "still cloning o/n") {
		unlock()
		t.Fatalf("got %q %v", out, err)
	}
	if again, err := lockRepo(s.mainDir(), 0); err == nil {
		again()
		t.Error("the lock must stop a second clone of the same repo")
	}
	unlock()
	repoWait = time.Minute
	out, _, err = callTool("scan", rawJ(map[string]any{"path": "o/n"}))
	if err != nil || strings.Contains(out, "still") || !exists(filepath.Join(reposDir(), "o", "n", "a.txt")) {
		t.Fatalf("after the lock: %q %v", out, err)
	}
}

func TestGitCloneUpdatesTheUsersCloneAndAsksForCopies(t *testing.T) {
	work := fakeGitHub(t)
	url := repoRemoteBase + "o/n.git"
	dest := filepath.Join(t.TempDir(), "mine")
	gitT(t, filepath.Dir(dest), "clone", "-q", url, dest)
	pushT(t, work, "a.txt", "v2", "second", "main")
	out, err := toolGit(rawJ(map[string]any{"action": "clone", "url": url, "to": dest}))
	if err != nil || !strings.Contains(out, "already cloned, updated") || !strings.Contains(out, "branch main") || !strings.Contains(out, "second") {
		t.Fatalf("clone into an existing clone: %q %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a.txt")); !strings.Contains(string(b), "v2") {
		t.Errorf("not pulled: %q", b)
	}
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, filepath.Dir(other), "init", "-q", other)
	gitT(t, other, "remote", "add", "origin", "https://example.com/x/y.git")
	if _, err := toolGit(rawJ(map[string]any{"action": "clone", "url": url, "to": other})); err == nil || !strings.Contains(err.Error(), "already a clone of") {
		t.Errorf("a clone of another repo must be refused, got %v", err)
	}
	// no destination: the temporary copies, asked about like any other use
	out, err = toolGit(rawJ(map[string]any{"action": "clone", "url": "o/n"}))
	if err != nil || !strings.HasPrefix(out, "o/n: branch main, last commit") || !strings.Contains(out, "second") {
		t.Fatalf("clone to the temporary copies: %q %v", out, err)
	}
	pushT(t, work, "a.txt", "v3", "third", "main")
	if out, err := toolGit(rawJ(map[string]any{"action": "clone", "url": "o/n"})); err != nil || !asks(out) {
		t.Fatalf("cloning a repo whose copy is older must ask: %q %v", out, err)
	}
	if out, err := toolGit(rawJ(map[string]any{"action": "clone", "url": "o/n", "update": "yes"})); err != nil || !strings.Contains(out, "third") {
		t.Fatalf("clone with update yes: %q %v", out, err)
	}
}

func TestRepoQuestionForChat(t *testing.T) {
	work := fakeGitHub(t)
	readRepo(t, "o/n/a.txt", nil)
	if q := repoQuestion("read", map[string]any{"path": "o/n/a.txt"}); q != "" {
		t.Fatalf("a copy that is up to date must not ask: %q", q)
	}
	pushT(t, work, "a.txt", "v2", "second", "main")
	if q := repoQuestion("cat", map[string]any{"path": "o/n/a.txt"}); !strings.Contains(q, "o/n (from ") || !strings.HasSuffix(q, "update first?") {
		t.Fatalf("got %q", q)
	}
	if q := repoQuestion("git", map[string]any{"action": "compare", "path": "o/n", "to": "o/n@v1"}); !strings.Contains(q, "o/n (from ") {
		t.Fatalf("compare: %q", q)
	}
	if q := repoQuestion("read", map[string]any{"path": "o/n/a.txt", "update": false}); q != "" {
		t.Fatalf("an answered call must not ask: %q", q)
	}
	if q := repoQuestion("write", map[string]any{"path": "o/n/a.txt"}); q != "" {
		t.Fatalf("write takes no repo paths: %q", q)
	}
}

func TestRepoCopiesAreReadOnly(t *testing.T) {
	fakeGitHub(t)
	for _, act := range []string{"commit", "push", "release"} {
		_, _, err := callTool("git", rawJ(map[string]any{"action": act, "path": "o/n", "message": "x", "version": "9.9.9"}))
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s on o/n: %v", act, err)
		}
	}
	readRepo(t, "o/n/a.txt", nil)
	local := filepath.Join(reposDir(), "o", "n")
	for _, act := range []string{"commit", "push"} {
		if _, err := toolGit(rawJ(map[string]any{"action": act, "path": local, "message": "x"})); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s in the local copy: %v", act, err)
		}
	}
	if out, _, err := callTool("git", rawJ(map[string]any{"action": "status", "path": "o/n"})); err != nil || !strings.HasPrefix(out, "o/n\n") {
		t.Errorf("status of o/n: %q %v", out, err)
	}
}

func TestGitCompareFolders(t *testing.T) {
	codeHome(t)
	a, b := t.TempDir(), t.TempDir()
	put := func(root, rel, s string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	for _, r := range []string{a, b} {
		put(r, "same.txt", "same\n")
		put(r, "go.mod", "module x\n")
	}
	put(a, "crlf.txt", "one\r\ntwo\r\n")
	put(b, "crlf.txt", "one\ntwo\n")
	put(a, "changed.go", "package x\n\nfunc a() {}\nfunc b() {}\nfunc c() {}\n")
	put(b, "changed.go", "package x\n\nfunc a() {}\nfunc b2() {}\nfunc c() {}\nfunc d() {}\n")
	put(a, "img.bin", "\x00\x01")
	put(b, "img.bin", "\x00\x02")
	put(a, "onlya.txt", "a\n")
	put(b, "onlyb.txt", "b\n")
	put(a, "x_windows.go", "package x\n\nconst p = 1\n")
	put(a, "x_other.go", "package x\n\nconst p = 3\n")
	put(b, "x_linux.go", "package x\n\nconst p = 2\n")
	for _, skip := range []string{"vendor/v.go", "node_modules/m/m.js", ".git/HEAD", "build/out.txt"} {
		put(a, skip, "x")
	}

	r, err := compareTrees(a, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.OnlyA, ","); got != "onlya.txt,x_other.go,x_windows.go" {
		t.Errorf("only in A: %s", got)
	}
	if got := strings.Join(r.OnlyB, ","); got != "onlyb.txt,x_linux.go" {
		t.Errorf("only in B: %s", got)
	}
	if r.Same != 3 || len(r.Changed) != 2 || r.Changed[0].label() != "changed.go +2 -1" || r.Changed[1].label() != "img.bin (binary)" {
		t.Errorf("same %d (want 3: CRLF vs LF is the same), changed %+v", r.Same, r.Changed)
	}

	// pair: x_windows.go goes with x_linux.go before the catch-all x_other.go does
	out, err := toolGit(rawJ(map[string]any{"action": "compare", "path": a, "to": b, "pair": true, "full": true, "paths": []string{"changed.go"}, "n": 2}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"only in A (2): onlya.txt, x_other.go\n", "only in B (1): onlyb.txt\n", "changed (3): changed.go +2 -1, x_windows.go <-> x_linux.go +1 -1 … +1 more", "same: 3", "--- changed.go +2 -1\n@@ A:4 B:4\n-func b() {}\n+func b2() {}\n@@ A:6 B:6\n+func d() {}"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDiffLineRunsIsMinimal(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 500; i++ {
		a, b := randLines(rng), randLines(rng)
		runs, add, del, approx := diffLineRuns(a, b)
		l := lcsLen(a, b)
		if approx || add != len(b)-l || del != len(a)-l {
			t.Fatalf("%v -> %v: +%d -%d approx=%v, want +%d -%d", a, b, add, del, approx, len(b)-l, len(a)-l)
		}
		var got []string
		ia, ib := 0, 0
		for _, r := range runs {
			switch r.op {
			case '=':
				if ia+r.n > len(a) || ib+r.n > len(b) || strings.Join(a[ia:ia+r.n], ",") != strings.Join(b[ib:ib+r.n], ",") {
					t.Fatalf("%v -> %v: bad kept run in %v", a, b, runs)
				}
				got = append(got, a[ia:ia+r.n]...)
				ia, ib = ia+r.n, ib+r.n
			case '-':
				ia += r.n
			case '+':
				if ib+r.n > len(b) {
					t.Fatalf("%v -> %v: bad added run in %v", a, b, runs)
				}
				got = append(got, b[ib:ib+r.n]...)
				ib += r.n
			}
		}
		if ia != len(a) || ib != len(b) || strings.Join(got, ",") != strings.Join(b, ",") {
			t.Fatalf("%v -> %v: runs %v don't turn a into b", a, b, runs)
		}
	}
}

func randLines(r *rand.Rand) []string {
	out := make([]string, r.Intn(14))
	for i := range out {
		out[i] = string(rune('a' + r.Intn(3)))
	}
	return out
}

func lcsLen(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

func TestRepoCleanupRemovesUnusedCopiesWithoutAsking(t *testing.T) {
	codeHome(t)
	oldApproval := cfg.Approval
	cfg.Approval = "wait" // even "every risky call waits" never applies to this housekeeping
	t.Cleanup(func() { cfg.Approval = oldApproval })
	old := time.Now().Add(-25 * time.Hour)
	mk := func(rel string, used time.Time) string {
		d := filepath.Join(reposDir(), filepath.FromSlash(rel))
		pack := filepath.Join(d, ".git", "objects", "pack")
		os.MkdirAll(pack, 0o755)
		os.WriteFile(filepath.Join(pack, "p.pack"), []byte("x"), 0o444) // Git's packs are read-only
		writeRepoState(d, repoState{Repo: rel, Fetched: used, Used: used})
		return d
	}
	oldMain, oldWT, fresh, busy := mk("o/old", old), mk("o/old@v1", old), mk("o/fresh", time.Now()), mk("o/busy", old)
	unlock, err := lockRepo(busy, 0) // being updated right now
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	part := filepath.Join(reposDir(), "o", "gone.part")
	os.MkdirAll(part, 0o755)
	os.Chtimes(part, old, old)
	mine := filepath.Join(appHome, "mine")
	os.MkdirAll(mine, 0o755)
	os.Chtimes(mine, old, old)

	start := time.Now()
	removed := cleanupRepos(repoMaxIdle)
	if time.Since(start) > 30*time.Second {
		t.Error("cleanup took too long (waiting for an approval?)")
	}
	for _, p := range []string{oldMain, oldWT, part, oldMain + ".json", oldWT + ".json"} {
		if exists(p) {
			t.Errorf("%s not removed", p)
		}
	}
	for _, p := range []string{fresh, busy, mine, busy + ".lock"} {
		if !exists(p) {
			t.Errorf("%s removed", p)
		}
	}
	if len(removed) != 3 {
		t.Errorf("removed %v", removed)
	}
	if err := removeRepoDir(mine); err == nil || !exists(mine) {
		t.Error("deleting outside data\\repos must be refused")
	}
}

func TestRepoShowWritesOwnerName(t *testing.T) {
	codeHome(t)
	p := filepath.Join(reposDir(), "o", "n@v1", "sub", "x.go")
	for _, in := range []string{p + ":3: hi", filepath.ToSlash(p) + ":3: hi"} {
		if got := repoShow(in); got != "o/n@v1/sub/x.go:3: hi" {
			t.Errorf("%q -> %q", in, got)
		}
	}
	q := filepath.Join(reposDir(), "o", "n", "b.go")
	if got := repoShow(p + " and " + q); got != "o/n@v1/sub/x.go and o/n/b.go" {
		t.Errorf("two paths on one line: %q", got)
	}
	s, _ := parseRepoSpec("Qcenaman/ContextSaver")
	s.remember()
	t.Cleanup(func() { repoNames.Delete("qcenaman/contextsaver") })
	if got := repoShow("in " + s.mainDir() + "\n"); got != "in Qcenaman/ContextSaver\n" {
		t.Errorf("got %q", got)
	}
}
