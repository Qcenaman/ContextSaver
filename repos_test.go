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
	oldBase, oldWait, oldClean := repoRemoteBase, repoWait, repoAutoClean
	t.Cleanup(func() { repoRemoteBase, repoWait, repoAutoClean = oldBase, oldWait, oldClean })
	repoAutoClean, repoWait = false, time.Minute
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

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	out, _, err := callTool("read", rawJ(map[string]any{"path": path, "mode": "head"}))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.Contains(out, reposDir()) {
		t.Errorf("read %s shows the local folder: %s", path, out)
	}
	return out
}

// ageRepo makes a copy look last fetched 10 minutes ago.
func ageRepo(dir string) {
	st := readRepoState(dir)
	st.Fetched = time.Now().Add(-10 * time.Minute)
	writeRepoState(dir, st)
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
	for _, bad := range []string{filepath.Join(t.TempDir(), "x"), "~/x/y", "./o/n", "../o/n", "x", "o/n/../../etc", "https://gitlab.com/o/n", "o/n@-x", "o/n@x.", "o/n@a@{1}", "o/...", "o/n.", "C:/x/y", "%TEMP%/x"} {
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
}

func TestRepoPathClonesReusesUpdatesAndChecksOutRefs(t *testing.T) {
	work := fakeGitHub(t)
	if out := readRepoFile(t, "o/n/a.txt"); !strings.Contains(out, "v1") {
		t.Fatalf("first use: %s", out)
	}
	main := filepath.Join(reposDir(), "o", "n")
	if !exists(filepath.Join(main, ".git")) {
		t.Fatalf("no copy in %s", main)
	}
	// a new commit on GitHub: a copy younger than 5 minutes is reused as it is...
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("v2\n"), 0o644)
	gitT(t, work, "commit", "-q", "-am", "second")
	gitT(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	if out := readRepoFile(t, "o/n/a.txt"); !strings.Contains(out, "v1") {
		t.Fatalf("a fresh copy should be reused: %s", out)
	}
	// ...an older one is fetched and fast-forwarded instead of failing
	ageRepo(main)
	if out := readRepoFile(t, "o/n/a.txt"); !strings.Contains(out, "v2") {
		t.Fatalf("not updated: %s", out)
	}
	// rewritten history: reset to origin
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("v3\n"), 0o644)
	gitT(t, work, "commit", "-q", "--amend", "-am", "second, rewritten")
	gitT(t, work, "push", "-q", "--force", "origin", "HEAD:refs/heads/main")
	ageRepo(main)
	if out := readRepoFile(t, "o/n/a.txt"); !strings.Contains(out, "v3") {
		t.Fatalf("not reset to the rewritten history: %s", out)
	}
	// @ref: a tag, a branch and a commit, each in its own worktree
	first := gitT(t, work, "rev-list", "--max-parents=0", "HEAD")
	for _, ref := range []string{"v1", "dev", first[:10]} {
		if out := readRepoFile(t, "o/n@"+ref+"/a.txt"); !strings.Contains(out, "v1") {
			t.Errorf("@%s: %s", ref, out)
		}
	}
	if !exists(filepath.Join(reposDir(), "o", "n@v1", "a.txt")) {
		t.Error("no n@v1 worktree")
	}
	// find, compare and outline answer with owner/name paths
	out, _, err := callTool("find", rawJ(map[string]any{"path": []string{"o/n@v1"}, "text": "Hello"}))
	if err != nil || !strings.Contains(out, "s.go") || strings.Contains(out, reposDir()) {
		t.Errorf("find: %q %v", out, err)
	}
	out, _, err = callTool("git", rawJ(map[string]any{"action": "compare", "path": "o/n@v1", "to": "o/n"}))
	if err != nil || !strings.Contains(out, "A: o/n@v1 (2 files)") || !strings.Contains(out, "changed (1): a.txt +1 -1") || !strings.Contains(out, "same: 1") || strings.Contains(out, reposDir()) {
		t.Errorf("compare: %q %v", out, err)
	}
	out, _, err = callCompact(rawJ(map[string]any{"c": "outline", "a": map[string]any{"path": "o/n/sub"}}))
	if err != nil || !strings.Contains(out, "Hello") {
		t.Errorf("outline: %q %v", out, err)
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

func TestGitCloneUpdatesAnExistingClone(t *testing.T) {
	work := fakeGitHub(t)
	url := repoRemoteBase + "o/n.git"
	dest := filepath.Join(t.TempDir(), "mine")
	gitT(t, filepath.Dir(dest), "clone", "-q", url, dest)
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("v2\n"), 0o644)
	gitT(t, work, "commit", "-q", "-am", "second")
	gitT(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
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
	// no destination: the temporary copies
	out, err = toolGit(rawJ(map[string]any{"action": "clone", "url": "o/n"}))
	if err != nil || !strings.HasPrefix(out, "o/n: branch main, last commit") || !strings.Contains(out, "second") {
		t.Fatalf("clone to the temporary copies: %q %v", out, err)
	}
	if !exists(filepath.Join(reposDir(), "o", "n", ".git")) {
		t.Error("no temporary copy")
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
	readRepoFile(t, "o/n/a.txt")
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
	put(b, "x_linux.go", "package x\n\nconst p = 2\n")
	for _, skip := range []string{"vendor/v.go", "node_modules/m/m.js", ".git/HEAD", "build/out.txt"} {
		put(a, skip, "x")
	}

	r, err := compareTrees(a, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.OnlyA, ","); got != "onlya.txt,x_windows.go" {
		t.Errorf("only in A: %s", got)
	}
	if got := strings.Join(r.OnlyB, ","); got != "onlyb.txt,x_linux.go" {
		t.Errorf("only in B: %s", got)
	}
	if r.Same != 3 || len(r.Changed) != 2 || r.Changed[0].label() != "changed.go +2 -1" || r.Changed[1].label() != "img.bin (binary)" {
		t.Errorf("same %d (want 3: CRLF vs LF is the same), changed %+v", r.Same, r.Changed)
	}

	out, err := toolGit(rawJ(map[string]any{"action": "compare", "path": a, "to": b, "pair": true, "full": true, "paths": []string{"changed.go"}, "n": 2}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"only in A (1): onlya.txt\n", "only in B (1): onlyb.txt\n", "changed (3): changed.go +2 -1, x_windows.go <-> x_linux.go +1 -1 … +1 more", "same: 3", "--- changed.go +2 -1\n@@ A:4 B:4\n-func b() {}\n+func b2() {}\n@@ A:6 B:6\n+func d() {}"} {
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
	s, _ := parseRepoSpec("Qcenaman/ContextSaver")
	s.remember()
	t.Cleanup(func() { repoNames.Delete("qcenaman/contextsaver") })
	if got := repoShow("in " + s.mainDir() + "\n"); got != "in Qcenaman/ContextSaver\n" {
		t.Errorf("got %q", got)
	}
}
