package main

// GitHub repos as paths. A path written owner/name[@ref][/sub/path] (not absolute) in
// find, read, scan, check, outline and git means that GitHub repo, so the local AI can
// search, read and compare code without pulling files into its small context.
// ContextSaver keeps its own temporary copies in data\repos\<owner>\<name>: partial
// clones (small, but with the history for tags and compare), and one <name>@<ref>
// worktree per ref sharing the same objects. A copy is refreshed when it is more than
// 5 minutes old, deleted by ContextSaver itself after 24 hours unused, and results
// show owner/name/... instead of the local folder. The copies are read-only for git:
// commit, push and release are refused there.

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
)

const (
	repoFresh     = 5 * time.Minute // older copies are fetched again before use
	repoMaxIdle   = 24 * time.Hour  // copies unused this long are deleted
	repoLockStale = 2 * time.Minute // a lock whose holder stopped refreshing it
	repoReadOnly  = "temporary GitHub copies (owner/name paths) are read-only: commit, push and release are refused there; clone the repo to your own folder (git{action:clone,url,to}) to change it"
)

func reposDir() string { return filepath.Join(appHome, "repos") }

// repoSpec is owner/name[@ref][/sub].
type repoSpec struct{ Owner, Name, Ref, Sub string }

var (
	repoSpecRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100}?)(?:\.git)?(?:@([^/\\\s]+))?(?:[/\\](.*))?$`)
	repoSHARe  = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
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

func validRepoRef(r string) bool {
	return r == "" || !(strings.HasPrefix(r, "-") || strings.Contains(r, "..") || strings.Contains(r, "@{") || strings.ContainsAny(r, " ~^:?*[\\") || strings.HasSuffix(r, ".lock") || strings.HasSuffix(r, ".") || r == "@")
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
	Repo    string    `json:"repo"`
	Fetched time.Time `json:"fetched"`
	Used    time.Time `json:"used"`
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

type repoJob struct {
	done chan struct{}
	what string
	note string
	err  error
}

var repoJobs sync.Map // copy folder -> *repoJob running in this process

// ensureRepo gets the copy ready (clone, refresh, worktree) and returns its folder. A
// call waits up to repoWait; a big first clone carries on in the background and a
// later call picks it up.
func ensureRepo(s repoSpec) (dir, note string, err error) {
	key := strings.ToLower(s.copyDir())
	j := &repoJob{done: make(chan struct{}), what: "updating"}
	if !exists(s.mainDir()) {
		j.what = "cloning"
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
			j.note, j.err = prepareRepo(s)
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

// prepareRepo clones or refreshes the main copy and, for @ref, its worktree, holding
// the repo's lock so two calls (or two ContextSaver processes) never clone at once.
func prepareRepo(s repoSpec) (note string, err error) {
	main := s.mainDir()
	unlock, err := lockRepo(main, 15*time.Minute)
	if err != nil {
		return "", fmt.Errorf("%s is busy in another ContextSaver; call again", s.repo())
	}
	defer unlock()
	now := time.Now()
	ms := readRepoState(main)
	if !isGitCopy(main) {
		if err := cloneRepo(s, main); err != nil {
			return "", err
		}
		ms = repoState{Fetched: time.Now()}
	} else if time.Since(ms.Fetched) >= repoFresh {
		if err := refreshMain(main); err != nil {
			note = staleNote(s.repo(), err, ms.Fetched)
		} else {
			ms.Fetched = time.Now()
		}
	}
	ms.Repo, ms.Used = s.repo(), now
	writeRepoState(main, ms)
	if s.Ref == "" {
		return note, nil
	}
	wt := s.copyDir()
	ws := readRepoState(wt)
	if !isGitCopy(wt) {
		if err := addWorktree(s, main, wt); err != nil {
			return note, err
		}
		ws = repoState{Fetched: time.Now()}
	} else if time.Since(ws.Fetched) >= repoFresh {
		if err := moveWorktree(s, main, wt); err != nil {
			if note == "" {
				note = staleNote(s.display(), err, ws.Fetched)
			}
		} else {
			ws.Fetched = time.Now()
		}
	}
	ws.Repo, ws.Used = s.display(), now
	writeRepoState(wt, ws)
	return note, nil
}

func staleNote(what string, err error, since time.Time) string {
	return fmt.Sprintf("(couldn't update %s: %s; using the copy from %s)", what, clip1(err.Error(), 160), since.Local().Format("2006-01-02 15:04"))
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

// refreshMain fetches and fast-forwards the main copy, or resets it to origin when the
// branch's history was rewritten.
func refreshMain(main string) error {
	out, code, err := gitExec(main, 10*time.Minute, gitNet("fetch", "--prune", "--tags", "--force", "--quiet", "origin")...)
	if err != nil || code != 0 {
		return errors.New(repoGitErr(out, err))
	}
	if _, code, _ := gitExec(main, 20*time.Second, "rev-parse", "-q", "--verify", "@{u}"); code != 0 {
		return nil // no branch to follow
	}
	if _, code, _ := gitExec(main, 5*time.Minute, gitNet("merge", "--ff-only", "--quiet", "@{u}")...); code == 0 {
		return nil
	}
	out, code, err = gitExec(main, 5*time.Minute, gitNet("reset", "--hard", "--quiet", "@{u}")...)
	if err != nil || code != 0 {
		return errors.New(repoGitErr(out, err))
	}
	return nil
}

// resolveRef finds the ref in the main copy as a branch (origin/<ref>), a tag or a
// commit, fetching once more when it isn't there yet.
func resolveRef(s repoSpec, main string) (string, error) {
	try := func() string {
		if _, code, _ := gitExec(main, 20*time.Second, "rev-parse", "-q", "--verify", "refs/remotes/origin/"+s.Ref); code == 0 {
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

// moveWorktree brings a @ref copy to what the ref means now (a branch moves on).
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
		case "", "status", "log", "diff", "pull", "ci", "actions", "handoff":
			return []string{"path"}
		case "compare":
			return []string{"path", "to"}
		}
	}
	return nil
}

// repoPaths swaps owner/name[@ref][/sub] paths in a call's arguments for the local
// copies, getting or refreshing them first. show (nil when no repo was named) writes
// the copies' folders in a result as owner/name/...; note tells of a copy that could
// not be updated.
func repoPaths(tool string, a map[string]any) (show func(string) string, note string, err error) {
	if tool == "git" {
		switch strings.ToLower(str(a, "action")) {
		case "commit", "push", "release":
			if _, ok := parseRepoSpec(str(a, "path")); ok {
				return nil, "", errors.New(repoReadOnly)
			}
		}
	}
	var notes []string
	used := false
	conv := func(v string) (string, error) {
		s, ok := parseRepoSpec(v)
		if !ok {
			return v, nil
		}
		dir, n, err := ensureRepo(s)
		if err != nil {
			return v, err
		}
		used = true
		if n != "" {
			notes = append(notes, n)
		}
		if s.Sub == "" {
			return dir, nil
		}
		p := filepath.Join(dir, filepath.FromSlash(s.Sub))
		if !hasPathPrefix(p, dir) {
			return v, fmt.Errorf("%s leaves the repo", v)
		}
		return p, nil
	}
	for _, k := range repoArgKeys(tool, a) {
		switch v := a[k].(type) {
		case string:
			if a[k], err = conv(v); err != nil {
				return nil, "", err
			}
		case []string:
			for i := range v {
				if v[i], err = conv(v[i]); err != nil {
					return nil, "", err
				}
			}
		case []any:
			for i := range v {
				if s, ok := v[i].(string); ok {
					if v[i], err = conv(s); err != nil {
						return nil, "", err
					}
				}
			}
		}
	}
	if !used {
		return nil, "", nil
	}
	if tool == "scan" && str(a, "exclude") == "" {
		a["exclude"] = ".git" // a copy's Git data is noise in an overview
	}
	if len(notes) > 0 {
		note = strings.Join(notes, "\n") + "\n"
	}
	return repoShow, note, nil
}

// repoRun runs f with the repo paths in a swapped for the local copies.
func repoRun(tool string, a map[string]any, f func() (string, error)) (string, error) {
	show, note, err := repoPaths(tool, a)
	if err != nil {
		var busy repoBusy
		if errors.As(err, &busy) {
			return busy.msg, nil
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
	seg := `[^\\/\s"'<>|*?:]+`
	repoShowCache.re = regexp.MustCompile(`(?i)(?:` + strings.Join(alts, "|") + `)[\\/]+(` + seg + `)[\\/]+(` + seg + `)((?:[\\/]+` + seg + `(?: ` + seg + `)*)*)[\\/]*`)
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

// ---------- git clone ----------

// gitClone clones into to; an existing clone of the same repo there is updated with
// pull --ff-only instead of failing. Without to (and path) a GitHub repo goes to the
// temporary copies. Returns the path, branch and last commit.
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
		dir, note, err := ensureRepo(spec)
		if err != nil {
			var busy repoBusy
			if errors.As(err, &busy) {
				return busy.msg, nil
			}
			return "", err
		}
		return note + repoShow(repoSummary(dir, spec.Ref)) + "\n(temporary copy: refreshed when older than 5 min, deleted after 24 h unused)", nil
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
		cmd += " --branch " + a.Ref
	}
	cmd += " " + shArg(u) + " " + shArg(dest)
	rargs, _ := json.Marshal(map[string]any{"command": cmd, "name": "git clone", "tail": 5, "wait_seconds": waitSecs(a.Wait, 45, 50), "env": map[string]string{"GIT_TERMINAL_PROMPT": "0"}})
	out, err := toolRun(rargs)
	if err == nil && strings.Contains(out, "exited with code 0") && exists(filepath.Join(dest, ".git")) {
		out += "\n" + repoSummary(dest, "")
	}
	return out, err
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
