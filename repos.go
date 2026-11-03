package main

// GitHub repos as paths. A path written owner/name[@ref][/sub/path] (not absolute) in
// find, read, scan, check, outline and git means that GitHub repo, so the local AI can
// search, read and compare code without pulling files into its small context.
// ContextSaver keeps its own temporary copies in data\repos\<owner>\<name>: partial
// clones (small, but with the history for tags and compare), and one <name>@<ref>
// worktree per ref sharing the same objects. A repo is cloned only when a call needs
// it, and a copy that is here already is never updated on its own: when GitHub has
// newer commits for it, the call answers with a question for the user (update it, or
// use it as it is) and runs once called again with update:true or update:false; git
// pull on owner/name updates it right away. Copies unused for 24 hours are deleted by
// ContextSaver itself, and results show owner/name/... instead of the local folder.
// The copies are read-only for git: commit, push and release are refused there.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	repoRemoteBase = "https://github.com/" // tests point it at local bare repos
	repoWait       = 45 * time.Second      // a call waits this long for a first clone, then says "call again"
	repoAutoClean  = true                  // the background cleanup (tests turn it off)
	repoCheckFor   = 2 * time.Minute       // one look at GitHub's newest commit serves the calls this long
)

const (
	repoMaxIdle   = 24 * time.Hour  // copies unused this long are deleted
	repoLockStale = 2 * time.Minute // a lock whose holder stopped refreshing it
	repoReadOnly  = "temporary GitHub copies (owner/name paths) are read-only: commit, push and release are refused there; clone the repo to your own folder (git{action:clone,url,to}) to change it"
)

func reposDir() string { return filepath.Join(appHome, "repos") }

// repoSpec is owner/name[@ref][/sub].
type repoSpec struct{ Owner, Name, Ref, Sub string }

var (
	repoSpecRe  = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100}?)(?:\.git)?(?:@([^/\\\s]+))?(?:[/\\](.*))?$`)
	repoSHARe   = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	repoFullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`)
	repoRefRe   = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,200}$`)
)

// parseRepoSpec reads owner/name[@ref][/sub], also written github.com/... or as a GitHub
// URL (.../tree/<ref>/sub, .../blob/<ref>/file). Absolute and home paths are never repos.
func parseRepoSpec(p string) (repoSpec, bool) {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"'`))
	if p == "" || filepath.IsAbs(p) || filepath.VolumeName(p) != "" || strings.ContainsAny(p[:1], `~./\%$`) {
		return repoSpec{}, false
	}
	low := strings.ToLower(p)
	isURL := false
	for _, pre := range []string{"https://", "http://"} {
		if strings.HasPrefix(low, pre) {
			p, low, isURL = p[len(pre):], low[len(pre):], true
		}
	}
	if strings.HasPrefix(low, "www.") {
		p, low = p[4:], low[4:]
	}
	if strings.HasPrefix(low, "github.com/") {
		p, isURL = p[len("github.com/"):], true
	} else if isURL {
		return repoSpec{}, false // another host
	}
	if strings.Contains(p, ":") {
		return repoSpec{}, false
	}
	m := repoSpecRe.FindStringSubmatch(p)
	if m == nil {
		return repoSpec{}, false
	}
	s := repoSpec{Owner: m[1], Name: m[2], Ref: m[3], Sub: strings.Trim(strings.ReplaceAll(m[4], `\`, "/"), "/")}
	if isURL && s.Ref == "" {
		if parts := strings.SplitN(s.Sub, "/", 3); len(parts) >= 2 && (parts[0] == "tree" || parts[0] == "blob") {
			s.Ref, s.Sub = parts[1], ""
			if len(parts) == 3 {
				s.Sub = parts[2]
			}
		}
	}
	if strings.Trim(s.Name, ".") == "" || strings.HasSuffix(s.Name, ".") || !validRepoRef(s.Ref) {
		return repoSpec{}, false
	}
	for _, seg := range strings.Split(s.Sub, "/") {
		if seg == ".." {
			return repoSpec{}, false
		}
	}
	return s, true
}

// validRepoRef: a branch, tag or commit name that is safe to hand to git and a shell.
func validRepoRef(r string) bool {
	if r == "" {
		return true
	}
	return repoRefRe.MatchString(r) && !strings.HasPrefix(r, "-") && !strings.HasPrefix(r, "/") && !strings.HasSuffix(r, "/") &&
		!strings.Contains(r, "..") && !strings.Contains(r, "//") && !strings.HasSuffix(r, ".lock") && !strings.HasSuffix(r, ".")
}

func (s repoSpec) repo() string { return s.Owner + "/" + s.Name }

func (s repoSpec) display() string {
	if s.Ref != "" {
		return s.repo() + "@" + s.Ref
	}
	return s.repo()
}

func (s repoSpec) url() string { return repoRemoteBase + s.Owner + "/" + s.Name + ".git" }

func (s repoSpec) mainDir() string {
	return filepath.Join(reposDir(), winSafeName(strings.ToLower(s.Owner)), winSafeName(strings.ToLower(s.Name)))
}

// winSafeName keeps a folder name usable on Windows, where con, nul, com1... (with any
// extension) are devices.
func winSafeName(n string) string {
	base, _, _ := strings.Cut(n, ".")
	switch {
	case base == "con" || base == "prn" || base == "aux" || base == "nul",
		len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9':
		return "_" + n
	}
	return n
}

// copyDir is the main clone, or the <name>@<ref> worktree.
func (s repoSpec) copyDir() string {
	if s.Ref == "" {
		return s.mainDir()
	}
	return s.mainDir() + "@" + safeRefName(s.Ref)
}

func safeRefName(r string) string {
	return strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._+-", c) {
			return c
		}
		return '_'
	}, r)
}

// repoNames: copy folder (owner/name[@ref], lower case) -> how it was asked for.
var repoNames sync.Map

func (s repoSpec) remember() {
	if rel, err := filepath.Rel(reposDir(), s.copyDir()); err == nil {
		repoNames.Store(strings.ToLower(filepath.ToSlash(rel)), s.display())
	}
}

// ---------- state, lock ----------

// repoState sits next to each copy as <copy>.json.
type repoState struct {
	Repo     string    `json:"repo"`
	Fetched  time.Time `json:"fetched"` // cloned or last updated
	Used     time.Time `json:"used"`
	Declined string    `json:"declined,omitempty"` // GitHub's commit the user chose not to update to
}

func readRepoState(dir string) repoState {
	var st repoState
	if b, err := os.ReadFile(dir + ".json"); err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

func writeRepoState(dir string, st repoState) { writeJSONAtomic(dir+".json", st) }

var errRepoLocked = errors.New("repo copy is locked")

// lockRepo takes the lock file of a repo's copies, shared by every ContextSaver process.
// The holder keeps the file's time fresh, so the lock of a process that died goes stale.
func lockRepo(main string, wait time.Duration) (func(), error) {
	p := main + ".lock"
	os.MkdirAll(filepath.Dir(p), 0o755)
	deadline := time.Now().Add(wait)
	for {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			stop, stopped := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(stopped)
				t := time.NewTicker(repoLockStale / 4)
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case <-t.C:
						now := time.Now()
						os.Chtimes(p, now, now)
					}
				}
			}()
			var once sync.Once
			return func() {
				once.Do(func() {
					close(stop)
					<-stopped // not while it touches the file (Windows refuses deleting an open file)
					for i := 0; i < 5 && os.Remove(p) != nil && exists(p); i++ {
						time.Sleep(50 * time.Millisecond)
					}
				})
			}, nil
		}
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > repoLockStale && os.Remove(p) == nil {
			continue // its holder is gone
		}
		if !time.Now().Before(deadline) {
			return nil, errRepoLocked
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// ---------- getting a copy ready ----------

// repoBusy: a first clone (or a slow update) still runs in the background; the call
// answers with this message instead of failing.
type repoBusy struct{ msg string }

func (e repoBusy) Error() string { return e.msg }

// repoAsk: GitHub has newer commits than a copy here and the user hasn't said whether
// to update it; the call answers with this question instead of running.
type repoAsk struct{ msg string }

func (e repoAsk) Error() string { return e.msg }

// repoMsg: what a call answers instead of its result while a repo isn't ready yet
// (still cloning) or the user has to decide first (update it or not).
func repoMsg(err error) (string, bool) {
	var busy repoBusy
	if errors.As(err, &busy) {
		return busy.msg, true
	}
	var ask repoAsk
	if errors.As(err, &ask) {
		return ask.msg, true
	}
	return "", false
}

type repoJob struct {
	done chan struct{}
	what string
	note string
	err  error
}

var repoJobs sync.Map // copy folder -> *repoJob running in this process

// repoPrep: what getting a copy ready may do besides cloning what is missing.
type repoPrep struct {
	update   bool   // fetch and move the copy to GitHub's newest commit
	strict   bool   // a failed update is an error (git pull), not a note
	declined string // GitHub's commit the user chose not to update to
}

// ensureRepo gets the copy ready (clone, worktree, the update asked for) and returns
// its folder. A call waits up to repoWait; a big clone carries on in the background
// and a later call picks it up.
func ensureRepo(s repoSpec, o repoPrep) (dir, note string, err error) {
	key := strings.ToLower(s.copyDir())
	j := &repoJob{done: make(chan struct{}), what: "preparing"}
	switch {
	case !exists(s.mainDir()):
		j.what = "cloning"
	case o.update:
		j.what = "updating"
	}
	if v, running := repoJobs.LoadOrStore(key, j); running {
		j = v.(*repoJob)
	} else {
		go func(j *repoJob) {
			defer func() {
				if r := recover(); r != nil {
					j.err = fmt.Errorf("internal error: %v", r)
				}
				repoJobs.Delete(key)
				close(j.done)
			}()
			j.note, j.err = prepareRepo(s, o)
		}(j)
	}
	select {
	case <-j.done:
		if j.err != nil {
			return "", "", j.err
		}
		s.remember()
		maybeCleanRepos()
		return s.copyDir(), j.note, nil
	case <-time.After(repoWait):
		return "", "", repoBusy{fmt.Sprintf("still %s %s in the background (big repo or slow network); call again in a minute", j.what, s.display())}
	}
}

// prepareRepo clones what is missing and makes the update asked for, holding the
// repo's lock so two calls (or two ContextSaver processes) never clone at once.
func prepareRepo(s repoSpec, o repoPrep) (note string, err error) {
	main := s.mainDir()
	unlock, err := lockRepo(main, 15*time.Minute)
	if err != nil {
		return "", fmt.Errorf("%s is busy in another ContextSaver; call again", s.repo())
	}
	defer unlock()
	// failed: an update that didn't work is an error for git pull; otherwise a note,
	// and the copy is used as it is
	failed := func(what string, err error, since time.Time) error {
		if o.strict {
			return fmt.Errorf("couldn't update %s: %s", what, clip1(err.Error(), 200))
		}
		note = staleNote(what, err, since)
		return nil
	}
	now := time.Now()
	ms := readRepoState(main)
	cloned := false
	if !isGitCopy(main) {
		if err := cloneRepo(s, main); err != nil {
			return "", err
		}
		ms, cloned = repoState{Fetched: time.Now()}, true
	} else if o.update && s.Ref == "" {
		if err := refreshMain(main); err != nil {
			if err := failed(s.repo(), err, ms.Fetched); err != nil {
				return "", err
			}
		} else {
			ms.Fetched, ms.Declined = time.Now(), ""
		}
		repoForget(main)
	}
	if s.Ref == "" && o.declined != "" {
		ms.Declined = o.declined
	}
	ms.Repo, ms.Used = s.repo(), now
	writeRepoState(main, ms)
	if s.Ref == "" {
		return note, nil
	}
	wt := s.copyDir()
	ws := readRepoState(wt)
	switch {
	case !isGitCopy(wt):
		// a new copy of a branch starts at GitHub's newest commit on it
		if !cloned && (o.update || repoIsBranch(main, s.Ref)) {
			if err := fetchMain(main); err != nil {
				if err := failed(s.display(), err, ms.Fetched); err != nil {
					return "", err
				}
			}
		}
		if err := addWorktree(s, main, wt); err != nil {
			return note, err
		}
		ws = repoState{Fetched: time.Now()}
	case o.update:
		if err := updateWorktree(s, main, wt); err != nil {
			if err := failed(s.display(), err, ws.Fetched); err != nil {
				return "", err
			}
		} else {
			ws.Fetched, ws.Declined = time.Now(), ""
		}
		repoForget(wt)
	}
	if o.declined != "" {
		ws.Declined = o.declined
	}
	ws.Repo, ws.Used = s.display(), now
	writeRepoState(wt, ws)
	return note, nil
}

func staleNote(what string, err error, since time.Time) string {
	return fmt.Sprintf("(couldn't update %s: %s; using the copy from %s)", what, clip1(err.Error(), 160), repoWhen(since))
}

func repoWhen(t time.Time) string {
	if t.IsZero() {
		return "earlier"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func gitNet(args ...string) []string { return append(append([]string{}, gitNetFlags...), args...) }

// isGitCopy: dir is a working clone or worktree of its own (not a broken copy that
// git would mistake for a repo further up).
func isGitCopy(dir string) bool {
	if !exists(filepath.Join(dir, ".git")) {
		return false
	}
	out, code, err := gitExec(dir, 20*time.Second, "rev-parse", "--show-toplevel", "HEAD")
	if err != nil || code != 0 {
		return false
	}
	top := filepath.FromSlash(strings.TrimSpace(firstLine(out)))
	return samePath(realPath(top), realPath(dir))
}

// cloneRepo makes a partial clone (no file contents until they are needed, full
// history) into <name>.part and renames it, so a broken clone never looks finished.
func cloneRepo(s repoSpec, main string) error {
	part := main + ".part"
	if exists(part) {
		removeRepoDir(part)
	}
	if exists(main) { // a copy that no longer works
		removeRepoDir(main)
	}
	if err := os.MkdirAll(filepath.Dir(main), 0o755); err != nil {
		return err
	}
	out, code, err := gitExec(filepath.Dir(main), 30*time.Minute, gitNet("clone", "--filter=blob:none", "--quiet", "-c", "core.longpaths=true", s.url(), part)...)
	if err != nil || code != 0 {
		removeRepoDir(part)
		msg := repoGitErr(out, err)
		if strings.HasPrefix(msg, "no such repo") {
			msg += " (a path on this PC must be absolute, like D:\\Photos)"
		}
		return fmt.Errorf("couldn't get %s from GitHub: %s", s.repo(), msg)
	}
	var rerr error
	for i := 0; i < 20; i++ {
		if rerr = os.Rename(part, main); rerr == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	removeRepoDir(part)
	return fmt.Errorf("couldn't set up the copy of %s: %v", s.repo(), rerr)
}

// fetchMain gets GitHub's new commits into the main copy without changing any files.
func fetchMain(main string) error {
	out, code, err := gitExec(main, 10*time.Minute, gitNet("fetch", "--prune", "--tags", "--force", "--quiet", "origin")...)
	if err != nil || code != 0 {
		return errors.New(repoGitErr(out, err))
	}
	return nil
}

// refreshMain fetches and fast-forwards the main copy, or resets it to origin when the
// branch's history was rewritten.
func refreshMain(main string) error {
	if err := fetchMain(main); err != nil {
		return err
	}
	if _, code, _ := gitExec(main, 20*time.Second, "rev-parse", "-q", "--verify", "@{u}"); code != 0 {
		return nil // no branch to follow
	}
	if _, code, _ := gitExec(main, 5*time.Minute, gitNet("merge", "--ff-only", "--quiet", "@{u}")...); code == 0 {
		return nil
	}
	out, code, err := gitExec(main, 5*time.Minute, gitNet("reset", "--hard", "--quiet", "@{u}")...)
	if err != nil || code != 0 {
		return errors.New(repoGitErr(out, err))
	}
	return nil
}

func repoIsBranch(main, ref string) bool {
	_, code, _ := gitExec(main, 20*time.Second, "rev-parse", "-q", "--verify", "refs/remotes/origin/"+ref)
	return code == 0
}

// resolveRef finds the ref in the main copy as a branch (origin/<ref>), a tag or a
// commit, fetching once more when it isn't there yet.
func resolveRef(s repoSpec, main string) (string, error) {
	try := func() string {
		if repoIsBranch(main, s.Ref) {
			return "refs/remotes/origin/" + s.Ref
		}
		if _, code, _ := gitExec(main, 20*time.Second, "rev-parse", "-q", "--verify", "refs/tags/"+s.Ref+"^{commit}"); code == 0 {
			return "refs/tags/" + s.Ref
		}
		if out, code, _ := gitExec(main, 20*time.Second, "rev-parse", "-q", "--verify", s.Ref+"^{commit}"); code == 0 {
			return strings.TrimSpace(out)
		}
		return ""
	}
	if t := try(); t != "" {
		return t, nil
	}
	gitExec(main, 10*time.Minute, gitNet("fetch", "--tags", "--force", "--quiet", "origin")...)
	if t := try(); t != "" {
		return t, nil
	}
	if repoSHARe.MatchString(s.Ref) {
		if _, code, _ := gitExec(main, 10*time.Minute, gitNet("fetch", "--quiet", "origin", s.Ref)...); code == 0 {
			if t := try(); t != "" {
				return t, nil
			}
		}
	}
	return "", fmt.Errorf("%s has no branch, tag or commit %q", s.repo(), s.Ref)
}

func addWorktree(s repoSpec, main, wt string) error {
	target, err := resolveRef(s, main)
	if err != nil {
		return err
	}
	if exists(wt) {
		removeRepoDir(wt)
	}
	gitExec(main, time.Minute, "worktree", "prune")
	out, code, err := gitExec(main, 10*time.Minute, gitNet("-c", "advice.detachedHead=false", "worktree", "add", "--detach", "--force", "--quiet", wt, target)...)
	if err != nil || code != 0 {
		removeRepoDir(wt)
		return fmt.Errorf("couldn't check out %s: %s", s.display(), repoGitErr(out, err))
	}
	return nil
}

// moveWorktree checks a @ref copy out at what the ref means in the main copy now.
func moveWorktree(s repoSpec, main, wt string) error {
	target, err := resolveRef(s, main)
	if err != nil {
		return err
	}
	out, code, err := gitExec(wt, 5*time.Minute, gitNet("-c", "advice.detachedHead=false", "checkout", "--quiet", "--force", "--detach", target)...)
	if err != nil || code != 0 {
		return errors.New(repoGitErr(out, err))
	}
	return nil
}

// updateWorktree fetches and moves a @ref copy to what the ref means on GitHub now.
func updateWorktree(s repoSpec, main, wt string) error {
	if err := fetchMain(main); err != nil {
		return err
	}
	return moveWorktree(s, main, wt)
}

func repoGitErr(out string, err error) string {
	if err != nil {
		return err.Error()
	}
	lo := strings.ToLower(out)
	switch {
	case strings.Contains(lo, "authentication") || strings.Contains(lo, "could not read username") || strings.Contains(lo, "403"):
		return "GitHub sign-in needed: git{action:login}, then call again"
	case strings.Contains(lo, "not found") || strings.Contains(lo, "does not exist") || strings.Contains(lo, "not appear to be a git repository"):
		return "no such repo on GitHub, or no access to it"
	case strings.Contains(lo, "could not resolve host") || strings.Contains(lo, "unable to access"):
		return "no connection to GitHub"
	}
	return gitShort(out, 4)
}

// ---------- is GitHub newer than a copy? ----------

type repoRemote struct {
	at  time.Time
	sha string
	err error
}

var repoRemotes sync.Map // copy folder (lower case) -> repoRemote

func repoForget(dir string) { repoRemotes.Delete(strings.ToLower(dir)) }

// repoFollows: the GitHub refs a copy follows: its branch, or the tag (and the commit
// it points at); none for a commit, which never changes.
func repoFollows(s repoSpec) []string {
	main := s.mainDir()
	if s.Ref == "" {
		br, code, _ := gitExec(main, 20*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
		if br = strings.TrimSpace(br); code != 0 || br == "" || br == "HEAD" {
			return nil
		}
		return []string{"refs/heads/" + br}
	}
	if repoIsBranch(main, s.Ref) {
		return []string{"refs/heads/" + s.Ref}
	}
	if _, code, _ := gitExec(main, 20*time.Second, "rev-parse", "-q", "--verify", "refs/tags/"+s.Ref); code == 0 {
		return []string{"refs/tags/" + s.Ref, "refs/tags/" + s.Ref + "^{}"}
	}
	return nil
}

// repoBehind: whether GitHub has a newer commit than the copy for what it follows,
// found with git ls-remote, which changes nothing here. remote = GitHub's commit.
func repoBehind(s repoSpec) (behind bool, remote string, err error) {
	dir := s.copyDir()
	local, code, _ := gitExec(dir, 20*time.Second, "rev-parse", "HEAD")
	if local = strings.TrimSpace(local); code != 0 || local == "" {
		return false, "", nil
	}
	key := strings.ToLower(dir)
	r, fresh := repoRemote{}, false
	if v, ok := repoRemotes.Load(key); ok {
		r = v.(repoRemote)
		fresh = time.Since(r.at) < repoCheckFor
	}
	if !fresh {
		refs := repoFollows(s)
		if len(refs) == 0 {
			return false, "", nil
		}
		out, code, err := gitExec(s.mainDir(), 30*time.Second, gitNet(append([]string{"ls-remote", "origin"}, refs...)...)...)
		r = repoRemote{at: time.Now()}
		if err != nil || code != 0 {
			r.err = errors.New(repoGitErr(out, err))
		} else {
			r.sha = lsRemoteSHA(out)
		}
		repoRemotes.Store(key, r)
	}
	return r.err == nil && r.sha != "" && !strings.EqualFold(r.sha, local), r.sha, r.err
}

// lsRemoteSHA: the commit in git ls-remote output (a tag's peeled ^{} line wins).
func lsRemoteSHA(out string) string {
	sha := ""
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || !repoFullSHA.MatchString(f[0]) {
			continue
		}
		if strings.HasSuffix(f[1], "^{}") {
			return f[0]
		}
		if sha == "" {
			sha = f[0]
		}
	}
	return sha
}

// repoCheck sorts a call's repos: what each one needs (what is missing is cloned, an
// update only when update says the user wants it), and, while update is nil, the copies
// to ask about: those GitHub has newer commits for, unless the user already said no
// to that commit.
func repoCheck(specs []repoSpec, update *bool) (preps []repoPrep, ask, notes []string) {
	preps = make([]repoPrep, len(specs))
	for i, s := range specs {
		dir := s.copyDir()
		if _, running := repoJobs.Load(strings.ToLower(dir)); running || !isGitCopy(dir) {
			continue // being cloned or updated now, or not here yet
		}
		behind, remote, err := repoBehind(s)
		st := readRepoState(dir)
		switch {
		case err != nil:
			if update != nil && *update {
				preps[i].update = true
			} else if update == nil {
				notes = append(notes, fmt.Sprintf("(couldn't check GitHub for newer commits of %s: %s; using the copy from %s)", s.display(), clip1(err.Error(), 120), repoWhen(st.Fetched)))
			}
		case !behind:
		case update != nil && *update:
			preps[i].update = true
		case update != nil:
			preps[i].declined = remote
		case !strings.EqualFold(remote, st.Declined):
			ask = append(ask, s.display()+" (from "+repoWhen(st.Fetched)+")")
		}
	}
	return preps, ask, notes
}

func repoQuestionText(ask []string) string {
	if len(ask) == 1 {
		return "GitHub has newer commits than the copy here of " + ask[0] + ". Ask the user whether to update it first, then call again with the same args plus update:true (update it) or update:false (use it as it is)."
	}
	return "GitHub has newer commits than the copies here of " + strings.Join(ask, ", ") + ". Ask the user whether to update them first, then call again with the same args plus update:true (update them) or update:false (use them as they are)."
}

// repoGet gets the copies for specs ready: what is missing is cloned, and a copy that
// is here already is only updated when update says the user wants it. While GitHub has
// newer commits for a copy and the user hasn't answered, it returns repoAsk.
func repoGet(specs []repoSpec, update *bool) (note string, err error) {
	specs = uniqueSpecs(specs)
	preps, ask, notes := repoCheck(specs, update)
	if len(ask) > 0 {
		return "", repoAsk{repoQuestionText(ask)}
	}
	for i, s := range specs {
		_, n, err := ensureRepo(s, preps[i])
		if err != nil {
			return "", err
		}
		if n != "" {
			notes = append(notes, n)
		}
	}
	if len(notes) > 0 {
		note = strings.Join(notes, "\n") + "\n"
	}
	return note, nil
}

// uniqueSpecs: one entry per copy (o/n/a.go and o/n/b.go both use o/n).
func uniqueSpecs(specs []repoSpec) []repoSpec {
	seen := map[string]bool{}
	var out []repoSpec
	for _, s := range specs {
		s.Sub = ""
		if k := strings.ToLower(s.copyDir()); !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

// repoAnswer reads the update argument, the user's answer to repoAsk: true, false, or
// nil when it isn't given.
func repoAnswer(v any) *bool {
	var b bool
	switch x := v.(type) {
	case bool:
		b = x
	case float64:
		b = x != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "y", "1":
			b = true
		case "false", "no", "n", "0":
		default:
			return nil
		}
	default:
		return nil
	}
	return &b
}

// ---------- tool arguments and results ----------

// repoArgKeys: the arguments of a tool that are paths and may name a GitHub repo.
func repoArgKeys(tool string, a map[string]any) []string {
	switch tool {
	case "find", "scan", "read", "outline":
		return []string{"path", "file"}
	case "check":
		if s := strings.ToLower(str(a, "source")); s == "" || s == "pc" {
			return []string{"path"}
		}
	case "git":
		switch strings.ToLower(str(a, "action")) {
		case "", "status", "log", "diff", "ci", "actions", "handoff":
			return []string{"path"}
		case "compare":
			return []string{"path", "to"}
		}
	}
	return nil
}

// repoSpecsIn: the GitHub repos named in a call's path arguments.
func repoSpecsIn(tool string, a map[string]any) []repoSpec {
	var specs []repoSpec
	add := func(v any) {
		if p, ok := v.(string); ok {
			if s, ok := parseRepoSpec(p); ok {
				specs = append(specs, s)
			}
		}
	}
	for _, k := range repoArgKeys(tool, a) {
		switch v := a[k].(type) {
		case []any:
			for _, x := range v {
				add(x)
			}
		case []string:
			for _, x := range v {
				add(x)
			}
		default:
			add(v)
		}
	}
	return specs
}

// repoPaths swaps owner/name[@ref][/sub] paths in a call's arguments for the local
// copies, getting them first (repoGet). show (nil when no repo was named) writes the
// copies' folders in a result as owner/name/...; note tells of a copy that couldn't be
// checked or updated.
func repoPaths(tool string, a map[string]any) (show func(string) string, note string, err error) {
	if tool == "git" {
		switch strings.ToLower(str(a, "action")) {
		case "commit", "push", "release":
			if _, ok := parseRepoSpec(str(a, "path")); ok {
				return nil, "", errors.New(repoReadOnly)
			}
		}
	}
	specs := repoSpecsIn(tool, a)
	if len(specs) == 0 {
		return nil, "", nil
	}
	update := repoAnswer(a["update"])
	delete(a, "update")
	if note, err = repoGet(specs, update); err != nil {
		return nil, "", err
	}
	conv := func(v string) string {
		s, ok := parseRepoSpec(v)
		if !ok {
			return v
		}
		if s.Sub == "" {
			return s.copyDir()
		}
		return filepath.Join(s.copyDir(), filepath.FromSlash(s.Sub))
	}
	for _, k := range repoArgKeys(tool, a) {
		switch v := a[k].(type) {
		case string:
			a[k] = conv(v)
		case []string:
			for i := range v {
				v[i] = conv(v[i])
			}
		case []any:
			for i := range v {
				if p, ok := v[i].(string); ok {
					v[i] = conv(p)
				}
			}
		}
	}
	if tool == "scan" && str(a, "exclude") == "" {
		a["exclude"] = ".git" // a copy's Git data is noise in an overview
	}
	return repoShow, note, nil
}

// repoRun runs f with the repo paths in a swapped for the local copies.
func repoRun(tool string, a map[string]any, f func() (string, error)) (string, error) {
	show, note, err := repoPaths(tool, a)
	if err != nil {
		if msg, ok := repoMsg(err); ok {
			return msg, nil
		}
		return "", err
	}
	out, err := f()
	if show != nil {
		out = note + show(out)
		if err != nil {
			err = errors.New(show(err.Error()))
		}
	}
	return out, err
}

// repoCall is repoPaths for a call's raw JSON arguments.
func repoCall(tool string, args json.RawMessage) (json.RawMessage, func(string) string, string, error) {
	if !strings.Contains(string(args), "/") {
		return args, nil, "", nil // owner/name always has one
	}
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return args, nil, "", nil
	}
	show, note, err := repoPaths(tool, m)
	if err != nil || show == nil {
		return args, nil, "", err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return args, nil, "", err
	}
	return b, show, note, nil
}

// repoTool: the tool whose path arguments a pc command takes, for repo paths.
func repoTool(c string) string {
	switch c = strings.ToLower(c); c {
	case "outline", "symbols":
		return "outline"
	case "test", "tests", "compile", "build":
		return "check"
	}
	switch t := compactAlias[c]; t {
	case "find", "scan", "read", "check", "git":
		return t
	}
	return ""
}

// repoQuestion is for ContextSaver Chat, which asks the user itself: what a pc call
// would stop to ask (GitHub has newer commits than a copy here), or "".
func repoQuestion(c string, a map[string]any) string {
	if repoAnswer(a["update"]) != nil {
		return ""
	}
	tool := repoTool(c)
	specs := repoSpecsIn(tool, a)
	if tool == "git" && strings.EqualFold(str(a, "action"), "clone") {
		src, to := str(a, "url"), str(a, "to")
		if src == "" {
			src = str(a, "path")
		} else if to == "" {
			to = str(a, "path")
		}
		if s, ok := parseRepoSpec(src); ok && to == "" {
			specs = append(specs, s)
		}
	}
	if len(specs) == 0 {
		return ""
	}
	if _, ask, _ := repoCheck(uniqueSpecs(specs), nil); len(ask) > 0 {
		return "GitHub has newer commits than the copy of " + strings.Join(ask, ", ") + ": update first?"
	}
	return ""
}

var repoShowCache struct {
	sync.Mutex
	key string
	re  *regexp.Regexp
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func repoShowRe() *regexp.Regexp {
	base := reposDir()
	rp := realPath(base)
	repoShowCache.Lock()
	defer repoShowCache.Unlock()
	if repoShowCache.re != nil && repoShowCache.key == base+"\x00"+rp {
		return repoShowCache.re
	}
	var alts []string
	seen := map[string]bool{}
	for _, b := range []string{base, rp} {
		sl := filepath.ToSlash(b)
		if seen[strings.ToLower(sl)] {
			continue
		}
		seen[strings.ToLower(sl)] = true
		var parts []string
		for _, p := range strings.Split(sl, "/") {
			if p != "" {
				parts = append(parts, regexp.QuoteMeta(p))
			}
		}
		alt := strings.Join(parts, `[\\/]+`)
		if strings.HasPrefix(sl, "/") {
			alt = `[\\/]` + alt
		}
		alts = append(alts, alt)
	}
	// one segment: no spaces, so a path never runs into the next one on the line
	seg := `[^\\/\s"'<>|*?:]+`
	repoShowCache.re = regexp.MustCompile(`(?i)(?:` + strings.Join(alts, "|") + `)[\\/]+(` + seg + `)[\\/]+(` + seg + `)((?:[\\/]+` + seg + `)*)[\\/]*`)
	repoShowCache.key = base + "\x00" + rp
	return repoShowCache.re
}

// repoShow writes the copies' local paths in s as owner/name[@ref]/sub: short, and the
// model can pass them straight back.
func repoShow(s string) string {
	if s == "" {
		return s
	}
	re := repoShowRe()
	return re.ReplaceAllStringFunc(s, func(m string) string {
		g := re.FindStringSubmatch(m)
		disp := g[1] + "/" + g[2]
		if v, ok := repoNames.Load(strings.ToLower(disp)); ok {
			disp = v.(string)
		}
		rest := strings.ReplaceAll(g[3], `\`, "/")
		for strings.Contains(rest, "//") {
			rest = strings.ReplaceAll(rest, "//", "/")
		}
		return disp + rest
	})
}

// isRepoCopy: p is inside data\repos (a temporary copy).
func isRepoCopy(p string) bool {
	for _, base := range []string{reposDir(), realPath(reposDir())} {
		for _, q := range []string{p, realPath(p)} {
			if hasPathPrefix(q, base) && !samePath(q, base) {
				return true
			}
		}
	}
	return false
}

// ---------- git clone, git pull ----------

// gitClone clones into to; an existing clone of the same repo there is updated with
// pull --ff-only instead of failing. Without to (and path) a GitHub repo goes to the
// temporary copies, asked about like any other use. Returns path, branch and last commit.
func gitClone(a gitArgs) (string, error) {
	src, to := strings.TrimSpace(a.URL), strings.TrimSpace(a.To)
	if src == "" {
		if _, ok := parseRepoSpec(a.Path); ok {
			src, a.Path = strings.TrimSpace(a.Path), ""
		}
	}
	if to == "" {
		to = strings.TrimSpace(a.Path)
	}
	if src == "" {
		return "", errors.New("clone needs url: owner/name or a git URL")
	}
	spec, isGH := parseRepoSpec(src)
	if to == "" {
		if !isGH {
			return "", errors.New("give to (a folder) for that URL; without to, only GitHub repos (owner/name) go to the temporary copies")
		}
		if spec.Ref == "" {
			spec.Ref = strings.TrimSpace(a.Ref)
		}
		if !validRepoRef(spec.Ref) {
			return "", fmt.Errorf("bad ref %q", spec.Ref)
		}
		spec.Sub = ""
		note, err := repoGet([]repoSpec{spec}, repoAnswer(a.Update))
		if err != nil {
			if msg, ok := repoMsg(err); ok {
				return msg, nil
			}
			return "", err
		}
		return note + repoShow(repoSummary(spec.copyDir(), spec.Ref)) + "\n(temporary copy; updated only when the user says so)", nil
	}
	u := src
	if isGH {
		u = repoRemoteBase + spec.Owner + "/" + spec.Name + ".git"
		if a.Ref == "" {
			a.Ref = spec.Ref
		}
	}
	if !validRepoRef(a.Ref) {
		return "", fmt.Errorf("bad ref %q", a.Ref)
	}
	dest, err := expandPath(to)
	if err != nil {
		return "", err
	}
	if err := checkAllowed(dest); err != nil {
		return "", err
	}
	if exists(filepath.Join(dest, ".git")) {
		origin, _, _ := gitExec(dest, 20*time.Second, "remote", "get-url", "origin")
		if normRemote(origin) != normRemote(u) {
			return "", fmt.Errorf("%s is already a clone of %s, not %s", dest, strings.TrimSpace(origin), src)
		}
		out, code, err := gitExec(dest, 5*time.Minute, gitNet("pull", "--ff-only")...)
		if err != nil {
			return "", err
		}
		if code != 0 {
			return "", fmt.Errorf("%s is already a clone; git pull --ff-only failed:\n%s", dest, gitShort(out, 10))
		}
		return "already cloned, updated (pull --ff-only): " + clip1(firstLine(strings.TrimSpace(out)), 100) + "\n" + repoSummary(dest, ""), nil
	}
	cmd := "git " + strings.Join(gitNetFlags, " ") + " clone --progress"
	if a.Ref != "" {
		cmd += " --branch " + shArg(a.Ref)
	}
	cmd += " " + shArg(u) + " " + shArg(dest)
	rargs, _ := json.Marshal(map[string]any{"command": cmd, "name": "git clone", "tail": 5, "wait_seconds": waitSecs(a.Wait, 45, 50), "env": map[string]string{"GIT_TERMINAL_PROMPT": "0"}})
	out, err := toolRun(rargs)
	if err == nil && strings.Contains(out, "exited with code 0") && exists(filepath.Join(dest, ".git")) {
		out += "\n" + repoSummary(dest, "")
	}
	return out, err
}

// gitRepoPull updates the temporary copy of owner/name[@ref] right away (git pull on a
// repo path: the user asked for it), or gets it when it isn't here yet.
func gitRepoPull(s repoSpec) (string, error) {
	s.Sub = ""
	had := isGitCopy(s.copyDir())
	_, note, err := ensureRepo(s, repoPrep{update: true, strict: true})
	if err != nil {
		if msg, ok := repoMsg(err); ok {
			return msg, nil
		}
		return "", err
	}
	what := "updated "
	if !had {
		what = "got "
	}
	return note + what + repoShow(repoSummary(s.copyDir(), s.Ref)), nil
}

// shArg quotes one argument for the run tool's shell.
func shArg(s string) string {
	if isWin {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// repoSummary: "<dir>: branch main, last commit abc1234 2026-11-03 message" (a @ref
// copy: "ref <ref>").
func repoSummary(dir, ref string) string {
	br, _, _ := gitExec(dir, 20*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
	if ref != "" {
		br = "ref " + ref
	} else if br == "" || br == "HEAD" {
		if t, code, _ := gitExec(dir, 20*time.Second, "describe", "--tags", "--exact-match"); code == 0 && t != "" {
			br = "tag " + t
		} else {
			br = "detached"
		}
	} else {
		br = "branch " + br
	}
	last, _, _ := gitExec(dir, 20*time.Second, "log", "-1", "--date=short", "--format=%h %ad %s")
	return fmt.Sprintf("%s: %s, last commit %s", dir, br, clip1(strings.TrimSpace(last), 120))
}

// normRemote reduces a remote URL or path to host/owner/name for comparing.
func normRemote(u string) string {
	u = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(u)), `\`, "/")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	} else if strings.HasPrefix(u, "git@") {
		u = strings.Replace(u, ":", "/", 1)
	}
	if i := strings.Index(u, "@"); i >= 0 && i < strings.Index(u+"/", "/") {
		u = u[i+1:] // user@host
	}
	u = strings.TrimPrefix(u, "www.")
	u = strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
	return strings.TrimLeft(u, "/")
}

// ---------- cleanup ----------

var (
	repoCleanMu   sync.Mutex
	repoCleanLast time.Time
)

// maybeCleanRepos runs the cleanup in the background at most every 10 minutes.
func maybeCleanRepos() {
	if !repoAutoClean {
		return
	}
	repoCleanMu.Lock()
	due := time.Since(repoCleanLast) > 10*time.Minute
	if due {
		repoCleanLast = time.Now()
	}
	repoCleanMu.Unlock()
	if due {
		go cleanupRepos(repoMaxIdle)
	}
}

// startRepoJanitor cleans up soon after ContextSaver starts, then every hour.
func startRepoJanitor() {
	if !repoAutoClean {
		return
	}
	go func() {
		time.Sleep(30 * time.Second)
		for {
			cleanupRepos(repoMaxIdle)
			time.Sleep(time.Hour)
		}
	}()
}

func repoIdle(dir string) time.Duration {
	t := readRepoState(dir).Used
	if t.IsZero() {
		fi, err := os.Stat(dir)
		if err != nil {
			return 0
		}
		t = fi.ModTime()
	}
	return time.Since(t)
}

// removeRepoDir deletes a temporary copy, Git's read-only files included, and refuses
// anything outside data\repos.
func removeRepoDir(p string) error {
	base := reposDir()
	if !hasPathPrefix(p, base) || samePath(p, base) {
		return fmt.Errorf("refusing to delete %s: not a temporary repo copy", p)
	}
	if err := os.RemoveAll(p); err == nil {
		return nil
	}
	filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err == nil {
			os.Chmod(q, 0o777)
		}
		return nil
	})
	return os.RemoveAll(p)
}

// cleanupRepos deletes the temporary copies nobody used for maxIdle. It is
// ContextSaver's own housekeeping inside data\repos, not a user delete, so it never
// waits for Approve/Deny; a copy whose lock is held (being cloned or updated) stays.
func cleanupRepos(maxIdle time.Duration) (removed []string) {
	repoCleanMu.Lock()
	repoCleanLast = time.Now()
	repoCleanMu.Unlock()
	base := reposDir()
	owners, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	for _, o := range owners {
		if !o.IsDir() {
			continue
		}
		od := filepath.Join(base, o.Name())
		ents, _ := os.ReadDir(od)
		dirs := map[string]bool{}
		var mains, wts []string
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			n := e.Name()
			dirs[n] = true
			switch {
			case strings.HasSuffix(n, ".part"): // a clone that never finished
				p := filepath.Join(od, n)
				if repoIdle(p) > time.Hour {
					if unlock, err := lockRepo(strings.TrimSuffix(p, ".part"), 0); err == nil {
						if removeRepoDir(p) == nil {
							removed = append(removed, p)
							delete(dirs, n)
						}
						unlock()
					}
				}
			case strings.Contains(n, "@"):
				wts = append(wts, n)
			default:
				mains = append(mains, n)
			}
		}
		for _, w := range wts {
			wt := filepath.Join(od, w)
			if repoIdle(wt) <= maxIdle {
				continue
			}
			main := filepath.Join(od, w[:strings.Index(w, "@")])
			unlock, err := lockRepo(main, 0)
			if err != nil {
				continue
			}
			hasMain := isGitCopy(main)
			if hasMain {
				gitExec(main, time.Minute, "worktree", "remove", "--force", wt)
			}
			if !exists(wt) || removeRepoDir(wt) == nil {
				os.Remove(wt + ".json")
				removed = append(removed, wt)
				delete(dirs, w)
			}
			if hasMain {
				gitExec(main, time.Minute, "worktree", "prune")
			}
			unlock()
		}
		for _, m := range mains {
			main := filepath.Join(od, m)
			if repoIdle(main) <= maxIdle {
				continue
			}
			inUse := false // a worktree still in use needs its main copy
			for d := range dirs {
				if strings.HasPrefix(d, m+"@") {
					inUse = true
				}
			}
			if inUse {
				continue
			}
			unlock, err := lockRepo(main, 0)
			if err != nil {
				continue
			}
			if removeRepoDir(main) == nil {
				os.Remove(main + ".json")
				removed = append(removed, main)
				delete(dirs, m)
			}
			unlock()
		}
		for _, e := range ents { // state files whose copy is gone, locks left by a stopped process
			n := e.Name()
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(n, ".json") && !dirs[strings.TrimSuffix(n, ".json")] {
				os.Remove(filepath.Join(od, n))
			}
			if strings.HasSuffix(n, ".lock") && !dirs[strings.TrimSuffix(n, ".lock")] {
				if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > repoLockStale {
					os.Remove(filepath.Join(od, n))
				}
			}
		}
		os.Remove(od) // only when empty
	}
	return removed
}
