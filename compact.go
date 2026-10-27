package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ContextSaver exposes one small tool ("pc") instead of many large ones, so a local
// model's context isn't filled with tool schemas. Full arguments for a command are
// fetched on demand with c=help, and every result is capped; the rest is kept on
// disk and paged with c=more.

const compactDesc = `Full control of the user's Windows PC: open apps, files and sites, run any command (admin too), read/write/edit files, search, see window text, click buttons, use Chrome. You CAN do these; never say you can't, call this tool. c=command, a=args. Paths absolute. Cut output: more{id}.
open{target}: app, file, folder or URL. find{path[],name,text} ls{path} read{path,mode:head|lines|grep|text,from_line,lines,pattern} write{path,content} edit{path,find,replace} run{command,shell:cmd|powershell} web{action:search|read,query,url} ai{action:ask|summarize,paths[],question} sys{action:info|processes|ports} model{action:list|use|stop,name} more{id} help{c}
help{c} lists all args; more commands: files job saved browser window media`

var compactTool = json.RawMessage(`{"name":"pc","description":` + strconv.Quote(compactDesc) + `,"inputSchema":{"type":"object","properties":{"c":{"type":"string"},"a":{"type":"object"}},"required":["c"]}}`)

// compact command -> full tool name
var compactAlias = map[string]string{
	"find": "find", "search": "find",
	"ls": "scan", "scan": "scan", "dir": "scan", "list": "scan",
	"read": "read", "cat": "read",
	"run": "run", "cmd": "run", "shell": "run", "exec": "run",
	"web": "web", "browser": "browser", "window": "window", "media": "media",
	"sys": "system", "system": "system",
	"ai": "local_ai", "local_ai": "local_ai",
	"job": "job", "jobs": "job", "files": "run_job", "run_job": "run_job", "saved": "saved",
}

const defaultMaxOut = 1800

func maxOut() int {
	if cfg.MaxOut > 0 {
		return cfg.MaxOut
	}
	return defaultMaxOut
}

// callCompact runs one "pc" call.
func callCompact(raw json.RawMessage) (string, []toolImage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", nil, fmt.Errorf("bad arguments: %v", err)
	}
	c := strings.ToLower(strings.TrimSpace(jsonStr(top["c"])))
	if c == "" {
		c = strings.ToLower(strings.TrimSpace(jsonStr(top["cmd"])))
	}
	args := map[string]any{}
	if a, ok := top["a"]; ok && len(a) > 0 && string(a) != "null" {
		if err := json.Unmarshal(a, &args); err != nil {
			// some models send a as a JSON string
			var s string
			if json.Unmarshal(a, &s) == nil && json.Unmarshal([]byte(s), &args) == nil {
			} else {
				return "", nil, errors.New(`a must be an object, e.g. {"c":"read","a":{"path":"D:\\x.txt"}}`)
			}
		}
	} else {
		// args given flat next to c
		for k, v := range top {
			if k == "c" || k == "cmd" {
				continue
			}
			var x any
			json.Unmarshal(v, &x)
			args[k] = x
		}
	}
	if c == "" {
		return "", nil, errors.New("give c (a command). " + firstLine(compactDesc))
	}
	out, imgs, err := compactDispatch(c, args)
	if err != nil {
		return "", nil, errors.New(clip1(err.Error(), 600))
	}
	return capOut(out), imgs, nil
}

func compactDispatch(c string, a map[string]any) (string, []toolImage, error) {
	switch c {
	case "open", "launch", "start", "app":
		out, err := openTarget(str(a, "target", "app", "path", "url", "name", "file"), str(a, "args"))
		return out, nil, err
	case "help", "?":
		return compactHelp(str(a, "c", "cmd", "command")), nil, nil
	case "more", "next":
		return moreOut(str(a, "id"), intArg(a, "from")), nil, nil
	case "model", "models":
		out, err := toolModel(a)
		return out, nil, err
	case "write", "save", "create":
		p := str(a, "path", "to", "file")
		if p == "" {
			return "", nil, errors.New("write needs path and content")
		}
		content, _ := a["content"].(string)
		if content == "" {
			content, _ = a["text"].(string)
		}
		return compactJob([]map[string]any{{"op": "write", "to": p, "content": content}}, a)
	case "edit", "replace":
		p := str(a, "path", "file", "from")
		if p == "" {
			return "", nil, errors.New("edit needs path, find, replace (or edits[] / append)")
		}
		st := map[string]any{"op": "edit", "from": []string{p}}
		if e, ok := a["edits"]; ok {
			st["edits"] = e
		} else if f := str(a, "find", "old"); f != "" {
			r, _ := a["replace"].(string)
			if r == "" {
				r, _ = a["new"].(string)
			}
			ed := map[string]any{"find": f, "replace": r}
			if b, ok := a["regex"].(bool); ok {
				ed["regex"] = b
			}
			st["edits"] = []any{ed}
		}
		if ap, ok := a["append"].(string); ok {
			st["append"] = ap
		}
		if st["edits"] == nil && st["append"] == nil {
			return "", nil, errors.New("edit needs find+replace, edits[] or append")
		}
		return compactJob([]map[string]any{st}, a)
	}
	name, ok := compactAlias[c]
	if !ok {
		return "", nil, fmt.Errorf("unknown c %q. Commands: find ls read write edit run web ai sys model more help files job saved browser window media", c)
	}
	fixArgs(name, a)
	b, _ := json.Marshal(a)
	out, imgs, err := callTool(name, b)
	if err == nil && name == "run" {
		out = trimRunOut(out)
	}
	if err == nil {
		out = logLineRe.ReplaceAllString(out, "")
	}
	return out, imgs, err
}

func compactJob(steps []map[string]any, a map[string]any) (string, []toolImage, error) {
	j := map[string]any{"steps": steps}
	if d, ok := a["dry_run"]; ok {
		j["dry_run"] = d
	}
	b, _ := json.Marshal(j)
	out, err := toolRunJob(b)
	return logLineRe.ReplaceAllString(out, ""), nil, err
}

// fixArgs accepts the common mistakes small models make: a string where a list
// is expected, and alternate names for the main argument.
func fixArgs(tool string, a map[string]any) {
	rename := func(want string, alts ...string) {
		if _, ok := a[want]; ok {
			return
		}
		for _, k := range alts {
			if v, ok := a[k]; ok {
				a[want] = v
				delete(a, k)
				return
			}
		}
	}
	listify := func(k string) {
		if s, ok := a[k].(string); ok {
			a[k] = []string{s}
		}
	}
	switch tool {
	case "find":
		rename("path", "paths", "folder", "dir", "in")
		listify("path")
		rename("name", "pattern", "glob")
		rename("text", "query", "contains", "grep")
	case "scan", "read", "media":
		rename("path", "file", "folder", "dir")
		if l, ok := a["path"].([]any); ok && len(l) > 0 {
			a["path"] = l[0]
		}
		if tool == "read" {
			rename("from_line", "start", "line", "offset")
			rename("lines", "count", "n", "limit")
			if _, ok := a["mode"]; !ok {
				switch {
				case a["pattern"] != nil:
					a["mode"] = "grep"
				case a["from_line"] != nil:
					a["mode"] = "lines"
				}
			}
			if _, ok := a["max_chars"]; !ok {
				a["max_chars"] = maxOut()
			}
		}
	case "run":
		rename("command", "cmd", "c", "script")
		if _, ok := a["tail"]; !ok {
			a["tail"] = 12
		}
	case "local_ai":
		rename("paths", "path", "files", "file")
		listify("paths")
		rename("question", "q", "query")
		if _, ok := a["action"]; !ok {
			if a["question"] != nil {
				a["action"] = "ask"
			} else {
				a["action"] = "summarize"
			}
		}
	case "web":
		rename("query", "q", "search")
		listify("urls")
		if _, ok := a["action"]; !ok {
			if a["url"] != nil || a["urls"] != nil {
				a["action"] = "read"
			} else {
				a["action"] = "search"
			}
		}
		if _, ok := a["max_chars"]; !ok {
			a["max_chars"] = maxOut()
		}
	case "window", "browser":
		if _, ok := a["max_chars"]; !ok {
			a["max_chars"] = maxOut()
		}
	case "system":
		if _, ok := a["action"]; !ok {
			a["action"] = "info"
		}
	}
}

var (
	runHeadRe  = regexp.MustCompile(`(?m)^\$ .*\n`)
	runBlankRe = regexp.MustCompile(`\n{3,}`)
	logLineRe  = regexp.MustCompile(`(?m)^Log: .*\n?`)
)

// trimRunOut drops the echoed command line; the model knows what it ran.
func trimRunOut(s string) string {
	s = runHeadRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\n  ", "\n")
	return runBlankRe.ReplaceAllString(s, "\n\n")
}

// ---------- help ----------

func compactHelp(c string) string {
	if c == "" {
		return compactDesc
	}
	switch strings.ToLower(c) {
	case "open":
		return "open{target,args}: start an app (notepad, chrome, calc, a full .exe path), open a file/folder in its default app, or a URL in the browser."
	case "write":
		return "write{path,content}: create or replace a text file (old version kept for job undo)."
	case "edit":
		return "edit{path,find,replace,regex}: replace text in a file. Or edits:[{find,replace,regex,count}], or append:text."
	case "model":
		return "model{action:list|use|stop,name}: list = models and which is loaded; use name=qwen|oss|oss-fast loads it on port 8080 (the chat pauses until it's loaded); stop unloads it."
	case "more":
		return "more{id,from}: next part of a cut result; from = character offset."
	}
	name, ok := compactAlias[strings.ToLower(c)]
	if !ok {
		return "unknown command " + c
	}
	for _, d := range toolDefs {
		var t struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Schema      struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"inputSchema"`
		}
		if json.Unmarshal(d, &t) != nil || t.Name != name {
			continue
		}
		var ks []string
		for k, v := range t.Schema.Properties {
			var p struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			}
			json.Unmarshal(v, &p)
			s := k
			if len(p.Enum) > 0 {
				s += ":" + strings.Join(p.Enum, "|")
			} else if p.Type != "" && p.Type != "string" {
				s += "(" + p.Type + ")"
			}
			ks = append(ks, s)
		}
		sortStrings(ks)
		return c + ": " + t.Description + "\nargs: " + strings.Join(ks, " ") + "\nrequired: " + strings.Join(t.Schema.Required, ",")
	}
	return "no help for " + c
}

// ---------- output cap + paging ----------

var (
	outMu   sync.Mutex
	outLast string
	outPos  = map[string]int{}
)

func outDir() string { return filepath.Join(appHome, "out") }

func capOut(s string) string {
	s = strings.TrimSpace(s)
	limit := maxOut()
	if len(s) <= limit+200 {
		return s
	}
	outMu.Lock()
	id := "r" + strconv.FormatInt(time.Now().UnixMilli()%60466176, 36)
	outLast = id
	outPos[id] = cutAt(s, limit)
	outMu.Unlock()
	os.MkdirAll(outDir(), 0o755)
	os.WriteFile(filepath.Join(outDir(), id+".txt"), []byte(s), 0o644)
	n := cutAt(s, limit)
	return s[:n] + fmt.Sprintf("\n…[cut, %d more chars: more{id:%s}]", len(s)-n, id)
}

func moreOut(id string, from int) string {
	if id == "" {
		outMu.Lock()
		id = outLast
		outMu.Unlock()
		if id == "" {
			return "nothing to continue; give id"
		}
	}
	b, err := os.ReadFile(filepath.Join(outDir(), filepath.Base(id)+".txt"))
	if err != nil {
		return "no saved output " + id
	}
	s := string(b)
	outMu.Lock()
	if from <= 0 {
		from = outPos[id]
	}
	if from >= len(s) {
		outMu.Unlock()
		return "end of " + id
	}
	n := from + cutAt(s[from:], maxOut())
	outPos[id] = n
	outMu.Unlock()
	out := s[from:n]
	if n < len(s) {
		out += fmt.Sprintf("\n…[%d more: more{id:%s}]", len(s)-n, id)
	}
	return out
}

// cutAt picks a cut point at or before limit, preferring a line end.
func cutAt(s string, limit int) int {
	if len(s) <= limit {
		return len(s)
	}
	if i := strings.LastIndexByte(s[:limit], '\n'); i > limit/2 {
		return i
	}
	for limit > 0 && limit < len(s) && (s[limit]&0xC0) == 0x80 {
		limit--
	}
	return limit
}

// ---------- small helpers ----------

func jsonStr(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return ""
}

func str(a map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := a[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case []any:
			if len(v) > 0 {
				if s, ok := v[0].(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

func intArg(a map[string]any, k string) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clip1(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:cutAt(s, n)] + "…"
}

func sortStrings(x []string) {
	for i := 1; i < len(x); i++ {
		for j := i; j > 0 && x[j] < x[j-1]; j-- {
			x[j], x[j-1] = x[j-1], x[j]
		}
	}
}

// openTarget starts an app, file, folder or URL the way the Start menu would, and
// returns at once.
func openTarget(t, args string) (string, error) {
	t = strings.TrimSpace(t)
	if t == "" {
		return "", errors.New("open needs target: an app (notepad), file, folder or URL")
	}
	if !isWin {
		return "", errors.New("open works on Windows only")
	}
	cmdArgs := []string{"/c", "start", "", t}
	if args != "" {
		cmdArgs = append(cmdArgs, strings.Fields(args)...)
	}
	if err := quietCmd("cmd.exe", cmdArgs...).Run(); err != nil {
		return "", fmt.Errorf("couldn't open %s: %v", t, err)
	}
	return "opened " + t, nil
}
