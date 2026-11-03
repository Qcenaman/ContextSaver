package main

// git compare: two folders, or owner/name[@ref] copies, side by side for the local AI:
// files only in A, only in B, changed ones sorted by changed lines (+added -removed)
// and how many are the same. CRLF vs LF and a UTF-8 BOM don't count as changes; .git,
// vendor, node_modules and build output are skipped. pair:true matches platform files
// (x_windows.go <-> x_linux.go or x.go); paths[] + full:true adds just those files'
// changed lines.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type cmpEntry struct {
	A, B     string // relative paths; B differs from A for paired platform files
	Add, Del int
	Note     string // binary, large, approx, unreadable
}

func (e cmpEntry) label() string {
	name := e.A
	if e.B != e.A {
		name = e.A + " <-> " + e.B
	}
	switch e.Note {
	case "binary", "large", "unreadable":
		return name + " (" + e.Note + ")"
	case "approx":
		return fmt.Sprintf("%s ~+%d -%d", name, e.Add, e.Del)
	}
	return fmt.Sprintf("%s +%d -%d", name, e.Add, e.Del)
}

type cmpResult struct {
	FilesA, FilesB int
	OnlyA, OnlyB   []string
	Changed        []cmpEntry
	Same           int
}

const (
	cmpMaxFiles = 100000
	cmpMaxBytes = 8 << 20
	cmpMaxD     = 1500       // past this many changed lines a file's counts are estimated
	cmpMaxWork  = 20_000_000 // and past this many diff steps (huge, repetitive files)
)

var (
	cmpSkipDirs   = map[string]bool{".git": true, "vendor": true, "node_modules": true}
	errCmpTooMany = errors.New("too many files")
	platFileRe    = regexp.MustCompile(`(?i)_(windows|linux|darwin|unix|other|posix|win|win32|nix|bsd)((?:_test)?\.[^./]+)$`)
	// platGeneric: suffixes for "every other platform" rather than one
	platGeneric = map[string]bool{"other": true, "unix": true, "posix": true, "nix": true}
)

func gitCompare(a gitArgs) (string, error) {
	if strings.TrimSpace(a.Path) == "" || strings.TrimSpace(a.To) == "" {
		return "", errors.New("compare needs path (A) and to (B): folders or owner/name[@ref]")
	}
	pa, err := expandPath(a.Path)
	if err != nil {
		return "", err
	}
	pb, err := expandPath(a.To)
	if err != nil {
		return "", err
	}
	for _, p := range []string{pa, pb} {
		if err := checkReadable(p); err != nil {
			return "", err
		}
	}
	r, err := compareTrees(pa, pb, a.Pair)
	if err != nil {
		return "", err
	}
	n := a.N
	if n <= 0 {
		n = 20
	}
	var b strings.Builder
	fmt.Fprintf(&b, "A: %s (%d files)\nB: %s (%d files)\n", pa, r.FilesA, pb, r.FilesB)
	fmt.Fprintf(&b, "only in A (%d): %s\n", len(r.OnlyA), cmpList(r.OnlyA, n))
	fmt.Fprintf(&b, "only in B (%d): %s\n", len(r.OnlyB), cmpList(r.OnlyB, n))
	ch := make([]string, len(r.Changed))
	for i, e := range r.Changed {
		ch[i] = e.label()
	}
	fmt.Fprintf(&b, "changed (%d): %s\n", len(r.Changed), cmpList(ch, n))
	fmt.Fprintf(&b, "same: %d", r.Same)
	if a.Full {
		if len(a.Paths) == 0 {
			b.WriteString("\n(full:true needs paths[]: the changed files whose lines you want)")
		} else {
			maxChars := a.MaxChars
			if maxChars <= 0 {
				maxChars = 6000
			}
			b.WriteString("\n" + cmpLines(pa, pb, r, a.Paths, maxChars))
		}
	}
	return b.String(), nil
}

func cmpList(items []string, n int) string {
	if len(items) == 0 {
		return "none"
	}
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" … +%d more", len(items)-n)
}

// compareTrees compares two folders (or two files).
func compareTrees(pa, pb string, pair bool) (*cmpResult, error) {
	sa, err := os.Stat(pa)
	if err != nil {
		return nil, fmt.Errorf("path not found: %s", pa)
	}
	sb, err := os.Stat(pb)
	if err != nil {
		return nil, fmt.Errorf("path not found: %s", pb)
	}
	r := &cmpResult{}
	if !sa.IsDir() || !sb.IsDir() {
		if sa.IsDir() != sb.IsDir() {
			return nil, errors.New("compare needs two folders or two files")
		}
		r.FilesA, r.FilesB = 1, 1
		if e, same := cmpFile(pa, pb); same {
			r.Same = 1
		} else {
			e.A, e.B = filepath.Base(pa), filepath.Base(pb)
			r.Changed = []cmpEntry{e}
		}
		return r, nil
	}
	fa, err := cmpWalk(pa)
	if err != nil {
		return nil, err
	}
	fb, err := cmpWalk(pb)
	if err != nil {
		return nil, err
	}
	r.FilesA, r.FilesB = len(fa), len(fb)
	for k, ra := range fa {
		rb, ok := fb[k]
		if !ok {
			r.OnlyA = append(r.OnlyA, ra)
			continue
		}
		e, same := cmpFile(filepath.Join(pa, filepath.FromSlash(ra)), filepath.Join(pb, filepath.FromSlash(rb)))
		if same {
			r.Same++
			continue
		}
		e.A, e.B = ra, rb
		r.Changed = append(r.Changed, e)
	}
	for k, rb := range fb {
		if _, ok := fa[k]; !ok {
			r.OnlyB = append(r.OnlyB, rb)
		}
	}
	sort.Strings(r.OnlyA)
	sort.Strings(r.OnlyB)
	if pair {
		cmpPair(r, pa, pb)
	}
	sort.SliceStable(r.Changed, func(i, j int) bool {
		ci, cj := r.Changed[i].Add+r.Changed[i].Del, r.Changed[j].Add+r.Changed[j].Del
		if ci != cj {
			return ci > cj
		}
		return r.Changed[i].A < r.Changed[j].A
	})
	return r, nil
}

// cmpWalk lists a folder's files (relative, with /), keyed for matching.
func cmpWalk(root string) (map[string]string, error) {
	m := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if p == root {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if cmpSkipDirs[strings.ToLower(name)] || isBuildOutDir(name, p) || isHeavyDir(name, p) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(name, ".git") || !d.Type().IsRegular() {
			return nil // a worktree's .git file, links
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		m[normCase(rel)] = rel
		if len(m) > cmpMaxFiles {
			return errCmpTooMany
		}
		return nil
	})
	if errors.Is(err, errCmpTooMany) {
		return nil, fmt.Errorf("%s has more than %d files; compare a subfolder", root, cmpMaxFiles)
	}
	return m, err
}

// cmpFile: the same, or how the file changed.
func cmpFile(pa, pb string) (cmpEntry, bool) {
	ia, ea := os.Stat(pa)
	ib, eb := os.Stat(pb)
	if ea != nil || eb != nil {
		return cmpEntry{Note: "unreadable"}, false
	}
	if ia.Size() > cmpMaxBytes || ib.Size() > cmpMaxBytes {
		if ia.Size() == ib.Size() && sameFileBytes(pa, pb) {
			return cmpEntry{}, true
		}
		return cmpEntry{Note: "large"}, false
	}
	ba, ea := os.ReadFile(pa)
	bb, eb := os.ReadFile(pb)
	if ea != nil || eb != nil {
		return cmpEntry{Note: "unreadable"}, false
	}
	if bytes.Equal(ba, bb) {
		return cmpEntry{}, true
	}
	if cmpBinary(ba) || cmpBinary(bb) {
		return cmpEntry{Note: "binary"}, false
	}
	_, add, del, approx := diffLineRuns(cmpSplit(ba), cmpSplit(bb))
	if add == 0 && del == 0 {
		return cmpEntry{}, true // only line ends, a BOM or the last newline differ
	}
	e := cmpEntry{Add: add, Del: del}
	if approx {
		e.Note = "approx"
	}
	return e, false
}

func cmpBinary(b []byte) bool { return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 }

// cmpSplit: the lines, ignoring CRLF vs LF, a BOM and the last newline.
func cmpSplit(b []byte) []string {
	s := strings.TrimPrefix(string(b), "\ufeff")
	s = strings.TrimSuffix(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func sameFileBytes(pa, pb string) bool {
	fa, err := os.Open(pa)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(pb)
	if err != nil {
		return false
	}
	defer fb.Close()
	ra, rb := bufio.NewReaderSize(fa, 1<<16), bufio.NewReaderSize(fb, 1<<16)
	x, y := make([]byte, 1<<16), make([]byte, 1<<16)
	for {
		n1, e1 := io.ReadFull(ra, x)
		n2, e2 := io.ReadFull(rb, y)
		if n1 != n2 || !bytes.Equal(x[:n1], y[:n2]) {
			return false
		}
		if e1 != nil || e2 != nil {
			end := func(e error) bool { return e == io.EOF || e == io.ErrUnexpectedEOF }
			return end(e1) && end(e2)
		}
	}
}

// platKey: the name without its platform part (x_windows.go -> x.go) and the platform.
func platKey(rel string) (key, plat string) {
	m := platFileRe.FindStringSubmatchIndex(rel)
	if m == nil {
		return strings.ToLower(rel), ""
	}
	return strings.ToLower(rel[:m[0]] + rel[m[4]:m[5]]), strings.ToLower(rel[m[2]:m[3]])
}

// pairScore ranks a platform pair, best first: x_windows.go <-> x_linux.go, then the
// shared x.go, then the catch-all x_other.go.
func pairScore(pa, pb string) int {
	ga, gb := platGeneric[pa], platGeneric[pb]
	switch {
	case pa == pb:
		return 5 // the same platform (names that differ only in case)
	case pa == "" || pb == "":
		if ga || gb {
			return 3
		}
		return 1
	case ga && gb:
		return 4
	case ga || gb:
		return 2
	}
	return 0
}

// cmpPair matches files only in A with files only in B that are the same file for
// another platform (x_windows.go <-> x_linux.go or x.go), best pairs first, and
// compares them.
func cmpPair(r *cmpResult, pa, pb string) {
	type cand struct{ a, b, score int }
	keyB := map[string][]int{}
	platB := make([]string, len(r.OnlyB))
	for j, rb := range r.OnlyB {
		k, p := platKey(rb)
		keyB[k] = append(keyB[k], j)
		platB[j] = p
	}
	var cands []cand
	for i, ra := range r.OnlyA {
		k, p := platKey(ra)
		for _, j := range keyB[k] {
			if p == "" && platB[j] == "" {
				continue
			}
			cands = append(cands, cand{i, j, pairScore(p, platB[j])})
		}
	}
	sort.SliceStable(cands, func(x, y int) bool {
		if cands[x].score != cands[y].score {
			return cands[x].score < cands[y].score
		}
		if cands[x].a != cands[y].a {
			return cands[x].a < cands[y].a
		}
		return cands[x].b < cands[y].b
	})
	usedA, usedB := make([]bool, len(r.OnlyA)), make([]bool, len(r.OnlyB))
	for _, c := range cands {
		if usedA[c.a] || usedB[c.b] {
			continue
		}
		usedA[c.a], usedB[c.b] = true, true
		ra, rb := r.OnlyA[c.a], r.OnlyB[c.b]
		e, same := cmpFile(filepath.Join(pa, filepath.FromSlash(ra)), filepath.Join(pb, filepath.FromSlash(rb)))
		if same {
			r.Same++
			continue
		}
		e.A, e.B = ra, rb
		r.Changed = append(r.Changed, e)
	}
	var keepA, keepB []string
	for i, ra := range r.OnlyA {
		if !usedA[i] {
			keepA = append(keepA, ra)
		}
	}
	for j, rb := range r.OnlyB {
		if !usedB[j] {
			keepB = append(keepB, rb)
		}
	}
	r.OnlyA, r.OnlyB = keepA, keepB
}

// cmpLines: the changed lines of the asked files, with the A and B line numbers of
// each hunk so the model can read around them.
func cmpLines(pa, pb string, r *cmpResult, paths []string, maxChars int) string {
	single := false
	if st, err := os.Stat(pa); err == nil && !st.IsDir() {
		single = true
	}
	var b strings.Builder
files:
	for _, want := range paths {
		w := strings.Trim(strings.ReplaceAll(strings.TrimSpace(want), `\`, "/"), "/")
		if i := strings.Index(w, "<->"); i >= 0 {
			w = strings.TrimSpace(w[:i])
		}
		var e *cmpEntry
		for i := range r.Changed {
			if c := &r.Changed[i]; strings.EqualFold(c.A, w) || strings.EqualFold(c.B, w) {
				e = c
				break
			}
		}
		if e == nil {
			fmt.Fprintf(&b, "--- %s: not among the changed files\n", w)
			continue
		}
		fmt.Fprintf(&b, "--- %s\n", e.label())
		if e.Note != "" && e.Note != "approx" {
			continue
		}
		fa, fb := filepath.Join(pa, filepath.FromSlash(e.A)), filepath.Join(pb, filepath.FromSlash(e.B))
		if single {
			fa, fb = pa, pb
		}
		ba, _ := os.ReadFile(fa)
		bb, _ := os.ReadFile(fb)
		la, lb := cmpSplit(ba), cmpSplit(bb)
		runs, _, _, _ := diffLineRuns(la, lb)
		ia, ib, inHunk := 0, 0, false
		for _, rn := range runs {
			if rn.op == '=' {
				ia += rn.n
				ib += rn.n
				inHunk = false
				continue
			}
			if !inHunk {
				fmt.Fprintf(&b, "@@ A:%d B:%d\n", ia+1, ib+1)
				inHunk = true
			}
			for k := 0; k < rn.n; k++ {
				if b.Len() > maxChars {
					break files
				}
				if rn.op == '-' {
					b.WriteString("-" + clip1(la[ia+k], 300) + "\n")
				} else {
					b.WriteString("+" + clip1(lb[ib+k], 300) + "\n")
				}
			}
			if rn.op == '-' {
				ia += rn.n
			} else {
				ib += rn.n
			}
		}
	}
	s := b.String()
	if len(s) > maxChars {
		s = s[:cutAt(s, maxChars)] + fmt.Sprintf("\n… cut at %d chars (max_chars)", maxChars)
	}
	return strings.TrimRight(s, "\n")
}

type diffRun struct {
	op byte // '=', '-', '+'
	n  int
}

// diffLineRuns is a minimal line diff (Myers) of a and b as runs of kept, removed and
// added lines, with the counts. Past cmpMaxD changes (or cmpMaxWork steps) the counts
// are estimated (approx) and the differing middle shows as removed, then added.
func diffLineRuns(a, b []string) (runs []diffRun, add, del int, approx bool) {
	push := func(op byte, n int) {
		if n <= 0 {
			return
		}
		if k := len(runs); k > 0 && runs[k-1].op == op {
			runs[k-1].n += n
			return
		}
		runs = append(runs, diffRun{op, n})
	}
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	ma, mb := a[p:len(a)-s], b[p:len(b)-s]
	push('=', p)
	if len(ma) == 0 || len(mb) == 0 {
		push('-', len(ma))
		push('+', len(mb))
		del, add = len(ma), len(mb)
	} else {
		ids := map[string]int32{}
		id := func(l string) int32 {
			v, ok := ids[l]
			if !ok {
				v = int32(len(ids))
				ids[l] = v
			}
			return v
		}
		xa, xb := make([]int32, len(ma)), make([]int32, len(mb))
		for i, l := range ma {
			xa[i] = id(l)
		}
		for i, l := range mb {
			xb[i] = id(l)
		}
		if mr, ok := myers(xa, xb, cmpMaxD); ok {
			for _, r := range mr {
				push(r.op, r.n)
				switch r.op {
				case '-':
					del += r.n
				case '+':
					add += r.n
				}
			}
		} else {
			approx = true
			left := map[int32]int{}
			for _, x := range xa {
				left[x]++
			}
			common := 0
			for _, y := range xb {
				if left[y] > 0 {
					left[y]--
					common++
				}
			}
			del, add = len(xa)-common, len(xb)-common
			push('-', len(ma))
			push('+', len(mb))
		}
	}
	push('=', s)
	return runs, add, del, approx
}

// myers: the shortest edit script from a to b (Myers' O(ND) algorithm), or false when
// it takes more than maxD edits or cmpMaxWork steps.
func myers(a, b []int32, maxD int) ([]diffRun, bool) {
	n, m := len(a), len(b)
	if maxD > n+m {
		maxD = n + m
	}
	off := maxD + 1
	v := make([]int32, 2*maxD+3)
	var trace [][]int32
	work := 0
	for d := 0; d <= maxD; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = int(v[off+k+1])
			} else {
				x = int(v[off+k-1]) + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
				work++
			}
			work++
			if work > cmpMaxWork {
				return nil, false
			}
			v[off+k] = int32(x)
			if x >= n && y >= m {
				trace = append(trace, append([]int32(nil), v[off-d:off+d+1]...))
				return myersBack(trace, n, m), true
			}
		}
		trace = append(trace, append([]int32(nil), v[off-d:off+d+1]...))
	}
	return nil, false
}

// myersBack walks the saved V rows back from (n,m) to (0,0).
func myersBack(trace [][]int32, n, m int) []diffRun {
	var rev []diffRun
	push := func(op byte, c int) {
		if c <= 0 {
			return
		}
		if k := len(rev); k > 0 && rev[k-1].op == op {
			rev[k-1].n += c
			return
		}
		rev = append(rev, diffRun{op, c})
	}
	x, y := n, m
	for d := len(trace) - 1; d > 0; d-- {
		prev, dd := trace[d-1], d-1
		at := func(k int) int { return int(prev[k+dd]) }
		k := x - y
		pk := k - 1
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
		}
		px := at(pk)
		py := px - pk
		if pk == k+1 { // down: a line of b was added
			push('=', x-px)
			push('+', 1)
		} else { // right: a line of a was removed
			push('=', x-px-1)
			push('-', 1)
		}
		x, y = px, py
	}
	push('=', x)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}
