//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	modAdvapi32Elev                 = syscall.NewLazyDLL("advapi32.dll")
	procConvertStringSDToSD         = modAdvapi32Elev.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procCreateNamedPipeW            = modKernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe            = modKernel32.NewProc("ConnectNamedPipe")
	procPeekNamedPipe               = modKernel32.NewProc("PeekNamedPipe")
	procWaitNamedPipeW              = modKernel32.NewProc("WaitNamedPipeW")
	procGetNamedPipeClientProcessId = modKernel32.NewProc("GetNamedPipeClientProcessId")
	procGetNamedPipeServerProcessId = modKernel32.NewProc("GetNamedPipeServerProcessId")
	procQueryFullProcessImageNameW  = modKernel32.NewProc("QueryFullProcessImageNameW")
	procFreeConsole                 = modKernel32.NewProc("FreeConsole")
	procLocalFree                   = modKernel32.NewProc("LocalFree")
)

var errStopped = errors.New("stopped")

// ---------- pipe connection (synchronous handle, polled so reads never block writes) ----------

type pipeConn struct {
	h      syscall.Handle
	mu     sync.Mutex
	closed bool
	buf    []byte
}

func (p *pipeConn) available() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errStopped
	}
	var n uint32
	r, _, e := procPeekNamedPipe.Call(uintptr(p.h), 0, 0, 0, uintptr(unsafe.Pointer(&n)), 0)
	if r == 0 {
		return 0, e
	}
	return int(n), nil
}

func (p *pipeConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errStopped
	}
	for len(b) > 0 {
		var w uint32
		if err := syscall.WriteFile(p.h, b, &w, nil); err != nil {
			return err
		}
		b = b[w:]
	}
	return nil
}

// readLine polls the pipe until a full line has arrived.
func (p *pipeConn) readLine(deadline time.Time, stop <-chan struct{}) ([]byte, error) {
	for {
		if i := bytes.IndexByte(p.buf, '\n'); i >= 0 {
			l := p.buf[:i]
			p.buf = p.buf[i+1:]
			return l, nil
		}
		n, err := p.available()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			b := make([]byte, n)
			var r uint32
			p.mu.Lock()
			err := syscall.ReadFile(p.h, b, &r, nil)
			p.mu.Unlock()
			if err != nil {
				return nil, err
			}
			p.buf = append(p.buf, b[:r]...)
			continue
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, errors.New("timed out")
		}
		select {
		case <-stop:
			return nil, errStopped
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func (p *pipeConn) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		syscall.CloseHandle(p.h)
	}
}

// ---------- small Windows helpers ----------

func isElevated() bool {
	t, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return false
	}
	defer t.Close()
	var e, n uint32
	err = syscall.GetTokenInformation(t, 20 /* TokenElevation */, (*byte)(unsafe.Pointer(&e)), 4, &n)
	return err == nil && e != 0
}

func processPath(pid uint32) string {
	h, _, _ := procOpenProcess.Call(0x1000, 0, uintptr(pid)) // PROCESS_QUERY_LIMITED_INFORMATION
	if h == 0 {
		return ""
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNameW.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func pipeUp() bool {
	name, _ := syscall.UTF16PtrFromString(elevPipeName)
	r, _, _ := procWaitNamedPipeW.Call(uintptr(unsafe.Pointer(name)), 1)
	return r != 0
}

func taskInstalled() bool {
	return quietCmd("schtasks", "/Query", "/TN", elevTaskName).Run() == nil
}

func auditPath() string {
	return filepath.Join(appRoot(), "admin-log", "elevated.log")
}

var auditMu sync.Mutex

func audit(format string, a ...any) {
	auditMu.Lock()
	defer auditMu.Unlock()
	f, err := os.OpenFile(auditPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\r\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}

// ---------- helper (server) ----------

func serveElevatedHelper() int {
	if !isElevated() {
		fmt.Println("The elevated helper has to run as administrator. Install it with: contextsaver install-helper")
		return 1
	}
	procFreeConsole.Call() // no window while it runs in the background
	sid, err := currentSID()
	if err != nil {
		audit("HELPER could not read the user account: %v", err)
		return 1
	}
	// Only this user (and SYSTEM) may open it, never over the network; medium label so the
	// non-elevated ContextSaver can reach it.
	sddl := "D:P(D;;GA;;;NU)(A;;GA;;;" + sid + ")(A;;GA;;;SY)"
	sa, err := secAttrs(sddl + "S:(ML;;NW;;;ME)")
	if err != nil {
		sa, err = secAttrs(sddl)
	}
	if err != nil {
		audit("HELPER bad security descriptor: %v", err)
		return 1
	}
	name, _ := syscall.UTF16PtrFromString(elevPipeName)
	first := true
	audit("HELPER started (pid %d)", os.Getpid())
	go helperWatchdog()
	for {
		openMode := uint32(0x00000003) // PIPE_ACCESS_DUPLEX
		if first {
			openMode |= 0x00080000 // FILE_FLAG_FIRST_PIPE_INSTANCE
		}
		pipeMode := uint32(0x00000008) // PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT | PIPE_REJECT_REMOTE_CLIENTS
		h, _, e := procCreateNamedPipeW.Call(uintptr(unsafe.Pointer(name)), uintptr(openMode), uintptr(pipeMode),
			255, 64<<10, 64<<10, 0, uintptr(unsafe.Pointer(sa)))
		if syscall.Handle(h) == syscall.InvalidHandle {
			if first {
				audit("HELPER not started: the pipe already exists (%v) — another helper is running, or something else took the name", e)
				return 0
			}
			time.Sleep(time.Second)
			continue
		}
		first = false
		r, _, e := procConnectNamedPipe.Call(h, 0)
		if r == 0 && e != syscall.Errno(535) { // ERROR_PIPE_CONNECTED
			syscall.CloseHandle(syscall.Handle(h))
			continue
		}
		go handleElevated(syscall.Handle(h))
	}
}

func secAttrs(sddl string) (*syscall.SecurityAttributes, error) {
	s, _ := syscall.UTF16PtrFromString(sddl)
	var sd uintptr
	r, _, e := procConvertStringSDToSD.Call(uintptr(unsafe.Pointer(s)), 1, uintptr(unsafe.Pointer(&sd)), 0)
	if r == 0 {
		return nil, e
	}
	return &syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

// startHelper starts the admin helper (first step of the "Local AI" icon).
func startHelper() string {
	if !taskInstalled() {
		return "Admin helper isn't installed (run once: contextsaver install-helper)"
	}
	if pipeUp() {
		return "Admin helper running"
	}
	quietCmd("schtasks", "/Run", "/TN", elevTaskName).Run()
	for i := 0; i < 40 && !pipeUp(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if pipeUp() {
		return "Admin helper started"
	}
	return "Admin helper didn't start"
}

// stopHelper ends the admin helper; if Windows won't let us end it, its watchdog
// exits on its own within a minute now that nothing is using it.
func stopHelper() string {
	if !pipeUp() {
		return ""
	}
	quietCmd("schtasks", "/End", "/TN", elevTaskName).Run()
	for i := 0; i < 20 && pipeUp(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if pipeUp() {
		return "Admin helper will stop by itself within a minute"
	}
	return "Admin helper stopped"
}

// While AnythingLLM is open the helper counts as in use.
// The helper only runs while ContextSaver does (AnythingLLM using a local model):
// it starts on the first admin command and exits once no ContextSaver tool process
// is left and no admin command is running.
var helperActive atomic.Int32

func helperWatchdog() {
	quiet := 0
	for {
		time.Sleep(30 * time.Second)
		if helperActive.Load() > 0 || contextSaverToolRunning() || processRunning("anythingllm.exe") {
			quiet = 0
			continue
		}
		if quiet++; quiet >= 2 {
			audit("HELPER stopped: ContextSaver isn't in use")
			os.Exit(0)
		}
	}
}

func contextSaverToolRunning() bool {
	ps, err := procs()
	if err != nil {
		return true // can't tell: keep running
	}
	for _, p := range ps {
		c := strings.ToLower(p.cmd)
		if strings.Contains(c, "contextsaver") && p.pid != os.Getpid() && !strings.Contains(c, " helper") {
			return true
		}
	}
	return false
}

func handleElevated(h syscall.Handle) {
	helperActive.Add(1)
	defer helperActive.Add(-1)
	p := &pipeConn{h: h}
	defer func() {
		if r := recover(); r != nil {
			audit("ERROR internal: %v", r)
		}
		p.mu.Lock()
		if !p.closed {
			syscall.FlushFileBuffers(h)
		}
		p.mu.Unlock()
		p.close()
	}()
	var cpid uint32
	procGetNamedPipeClientProcessId.Call(uintptr(h), uintptr(unsafe.Pointer(&cpid)))
	client := processPath(cpid)
	if client == "" || !helperClientOK(client) {
		audit("REJECTED connection from pid %d (%s) — only %s may use the helper", cpid, client, installPath())
		return
	}
	line, err := p.readLine(time.Now().Add(10*time.Second), nil)
	if err != nil {
		return
	}
	var req elevRequest
	if err := json.Unmarshal(line, &req); err != nil || strings.TrimSpace(req.Command) == "" {
		p.send(elevMsg{Error: "bad request"})
		return
	}
	shell := req.Shell
	if shell == "" {
		shell = "cmd"
	}
	if m := destructiveMatch(req.Command); m != "" && !req.Confirmed {
		audit("REFUSED (not confirmed, matched %q)  shell=%s  cwd=%s  cmd=%s", m, shell, req.Cwd, oneLine(req.Command))
		p.send(elevMsg{Error: fmt.Sprintf("refused: %q can delete or change system settings. Ask the user in chat, and only after they say yes run it again with confirmed: true.", m)})
		return
	}
	if req.Cwd != "" {
		if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
			p.send(elevMsg{Error: "folder not found: " + req.Cwd})
			return
		}
	}
	cmd, err := buildCommand(shell, req.Command)
	if err != nil {
		p.send(elevMsg{Error: err.Error()})
		return
	}
	cmd.Dir = req.Cwd
	cmd.Env = append(os.Environ(), req.Env...)
	pr, pw, err := os.Pipe()
	if err != nil {
		p.send(elevMsg{Error: err.Error()})
		return
	}
	defer pr.Close()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		audit("FAILED to start  shell=%s  cwd=%s  cmd=%s  — %v", shell, req.Cwd, oneLine(req.Command), err)
		p.send(elevMsg{Error: "couldn't start: " + err.Error()})
		return
	}
	pw.Close()
	pid := cmd.Process.Pid
	started := time.Now()
	confirmed := ""
	if req.Confirmed {
		confirmed = "  (confirmed by user)"
	}
	audit("START pid=%d  shell=%s  cwd=%s  cmd=%s%s", pid, shell, req.Cwd, oneLine(req.Command), confirmed)
	p.send(elevMsg{PID: pid})

	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := pr.Read(buf)
			if n > 0 && p.send(elevMsg{Out: append([]byte(nil), buf[:n]...)}) != nil {
				io.Copy(io.Discard, pr)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	waitDone := make(chan struct{})
	go func() { cmd.Wait(); close(waitDone) }()
	canceled := false
loop:
	for {
		select {
		case <-waitDone:
			break loop
		case <-time.After(500 * time.Millisecond):
			if _, err := p.available(); err != nil && !canceled {
				canceled = true // ContextSaver closed the connection: canceled, timed out, or it quit
				killTree(pid)
			}
		}
	}
	select {
	case <-outDone:
	case <-time.After(3 * time.Second): // something it started still holds the output open
	}
	code := cmd.ProcessState.ExitCode()
	note := ""
	if canceled {
		note = "  (stopped by ContextSaver)"
	}
	audit("END   pid=%d  exit=%d  after %s%s", pid, code, hd(time.Since(started)), note)
	p.send(elevMsg{Exit: &code})
}

// ---------- client ----------

func openElevPipe() (syscall.Handle, error) {
	name, _ := syscall.UTF16PtrFromString(elevPipeName)
	triedStart := false
	deadline := time.Now().Add(10 * time.Second)
	for {
		h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING,
			0x00100000|0x00010000, 0) // SECURITY_SQOS_PRESENT | SECURITY_IDENTIFICATION: the helper can't impersonate us
		if err == nil {
			return h, nil
		}
		switch {
		case err == syscall.ERROR_FILE_NOT_FOUND:
			if !taskInstalled() {
				return 0, errors.New("the elevated helper isn't installed. The user has to run once, in a normal window: \"" + installPath() + "\" install-helper (one UAC prompt)")
			}
			if !triedStart {
				triedStart = true
				quietCmd("schtasks", "/Run", "/TN", elevTaskName).Run()
			}
		case err == syscall.Errno(231): // ERROR_PIPE_BUSY
		default:
			return 0, fmt.Errorf("couldn't reach the elevated helper: %v", err)
		}
		if time.Now().After(deadline) {
			return 0, errors.New("the elevated helper isn't running and didn't start. It starts on demand; the user can start it with: schtasks /Run /TN \"" + elevTaskName + "\"")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func startElevated(req elevRequest, out io.Writer) (*elevProc, error) {
	h, err := openElevPipe()
	if err != nil {
		return nil, err
	}
	p := &pipeConn{h: h}
	var spid uint32
	procGetNamedPipeServerProcessId.Call(uintptr(h), uintptr(unsafe.Pointer(&spid)))
	if sp := processPath(spid); sp != "" && !helperClientOK(sp) {
		p.close()
		// Self-heal: a stale ContextSaver helper left at an old install path (after the
		// program was updated or moved) is re-pointed at the current install once, then
		// we reconnect. A non-ContextSaver process on the pipe is still refused.
		if !isContextSaverExe(sp) || !healHelper() {
			return nil, fmt.Errorf("refused: the elevated pipe is served by %s, not ContextSaver", sp)
		}
		if h, err = openElevPipe(); err != nil {
			return nil, err
		}
		p = &pipeConn{h: h}
		spid = 0
		procGetNamedPipeServerProcessId.Call(uintptr(h), uintptr(unsafe.Pointer(&spid)))
		if sp := processPath(spid); sp != "" && !helperClientOK(sp) {
			p.close()
			return nil, fmt.Errorf("refused: the elevated pipe is served by %s, not ContextSaver", sp)
		}
	}
	if err := p.send(req); err != nil {
		p.close()
		return nil, fmt.Errorf("couldn't talk to the elevated helper: %v", err)
	}
	line, err := p.readLine(time.Now().Add(20*time.Second), nil)
	if err != nil {
		p.close()
		return nil, fmt.Errorf("no answer from the elevated helper: %v", err)
	}
	var m elevMsg
	json.Unmarshal(line, &m)
	if m.Error != "" || m.PID == 0 {
		p.close()
		if m.Error == "" {
			m.Error = "bad answer from the elevated helper"
		}
		return nil, errors.New(m.Error)
	}
	stopCh := make(chan struct{})
	var once sync.Once
	ep := &elevProc{pid: m.PID, done: make(chan struct{}), stop: func() { once.Do(func() { close(stopCh) }) }}
	go func() {
		defer close(ep.done)
		defer p.close()
		for {
			line, err := p.readLine(time.Time{}, stopCh)
			if err != nil {
				if err == errStopped {
					ep.err = errCanceled
				} else {
					ep.err = fmt.Errorf("lost the connection to the elevated helper: %v", err)
				}
				return
			}
			var m elevMsg
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			if len(m.Out) > 0 {
				out.Write(m.Out)
			}
			if m.Error != "" {
				ep.err = errors.New(m.Error)
				return
			}
			if m.Exit != nil {
				ep.code = m.Exit
				return
			}
		}
	}()
	return ep, nil
}

// ---------- install / uninstall ----------

func elevateSelf(arg string) error {
	exe := installPath()
	ps := "Start-Process -Verb RunAs -Wait -FilePath '" + strings.ReplaceAll(exe, "'", "''") + "' -ArgumentList '" + arg + "'"
	return quietCmd("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Run()
}

func consoleInstallHelper() {
	exe := installPath()
	if !exists(exe) {
		fmt.Println("Install ContextSaver first (run contextsaver.exe without arguments), then install the helper.")
		return
	}
	if !isElevated() {
		fmt.Println("Windows will ask once for permission to set up the elevated helper...")
		if err := elevateSelf("install-helper"); err != nil {
			fmt.Println("Not installed — the permission prompt was declined or failed:", err)
			return
		}
		if taskInstalled() {
			fmt.Println("Elevated helper installed. It starts by itself when ContextSaver needs admin and stops when it's no longer in use.")
			fmt.Println("Every elevated command is logged in", auditPath())
		} else if taskInstalled() {
			fmt.Println("Helper installed but not answering yet. It starts at your next logon, or now with:")
			fmt.Println(`  schtasks /Run /TN "` + elevTaskName + `"`)
		} else {
			fmt.Println("The helper wasn't installed.")
		}
		return
	}
	fail := func(msg string, err error) {
		fmt.Println(msg, err)
		waitEnter("Press Enter to close.", 0)
	}
	sid, err := currentSID()
	if err != nil {
		fail("Couldn't read your account:", err)
		return
	}
	// Audit log folder: only admins and SYSTEM can write, you can read.
	dir := filepath.Dir(auditPath())
	quietCmd("schtasks", "/End", "/TN", elevTaskName).Run() // the old helper may hold its log or program open
	if pd := os.Getenv("ProgramData"); pd != "" {
		if oldDir := filepath.Join(pd, "ContextSaver"); exists(oldDir) {
			moveTree(oldDir, dir)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail("Couldn't create "+dir+":", err)
		return
	}
	quietCmd("icacls", dir, "/setowner", "*S-1-5-32-544", "/T", "/C", "/Q").Run()
	if out, err := quietCmd("icacls", dir, "/inheritance:r",
		"/grant:r", "*S-1-5-32-544:(OI)(CI)F", "/grant:r", "*S-1-5-18:(OI)(CI)F", "/grant:r", "*"+sid+":(OI)(CI)RX",
		"/T", "/C", "/Q").CombinedOutput(); err != nil {
		fail("Couldn't lock down the log folder: "+strings.TrimSpace(string(out)), err)
		return
	}
	var esc bytes.Buffer
	xml.EscapeText(&esc, []byte(exe))
	task := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>ContextSaver elevated helper: runs the admin commands ContextSaver hands it, without a UAC prompt each time. Every command is logged in ` + auditPath() + `</Description>
    <SecurityDescriptor>D:P(A;;FA;;;BA)(A;;FA;;;SY)(A;;GRGX;;;` + sid + `)</SecurityDescriptor>
  </RegistrationInfo>
  <Triggers />
  <Principals>
    <Principal id="Author"><UserId>` + sid + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>HighestAvailable</RunLevel></Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>4</Priority>
    <RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec><Command>` + esc.String() + `</Command><Arguments>helper</Arguments></Exec>
  </Actions>
</Task>
`
	tmp := filepath.Join(filepath.Dir(exe), "helper-task.xml")
	u := utf16.Encode([]rune(task))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for i, x := range u {
		b[2+2*i], b[3+2*i] = byte(x), byte(x>>8)
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		fail("Couldn't write the task file:", err)
		return
	}
	defer os.Remove(tmp)
	quietCmd("schtasks", "/End", "/TN", elevTaskName).Run()
	if out, err := quietCmd("schtasks", "/Create", "/TN", elevTaskName, "/XML", tmp, "/F").CombinedOutput(); err != nil {
		fail("Couldn't create the scheduled task: "+strings.TrimSpace(string(out)), err)
		return
	}
	for i := 0; i < 20 && pipeUp(); i++ { // let a previous helper exit
		time.Sleep(250 * time.Millisecond)
	}
	audit("INSTALLED helper task for %s (starts only when ContextSaver needs it)", sid)
}

func consoleUninstallHelper() {
	if !isElevated() {
		if !taskInstalled() {
			fmt.Println("The elevated helper isn't installed.")
			return
		}
		fmt.Println("Windows will ask once for permission to remove the elevated helper...")
		if err := elevateSelf("uninstall-helper"); err != nil {
			fmt.Println("Not removed — the permission prompt was declined or failed:", err)
			return
		}
		if taskInstalled() {
			fmt.Println("The helper is still installed; removing it failed.")
		} else {
			fmt.Println("Elevated helper removed. The log is kept at", auditPath())
		}
		return
	}
	quietCmd("schtasks", "/End", "/TN", elevTaskName).Run()
	if out, err := quietCmd("schtasks", "/Delete", "/TN", elevTaskName, "/F").CombinedOutput(); err != nil {
		fmt.Println("Couldn't remove the task:", strings.TrimSpace(string(out)))
		waitEnter("Press Enter to close.", 0)
		return
	}
	audit("UNINSTALLED helper task")
}

// refreshHelper points an installed helper at the newly installed program (one UAC prompt).
func refreshHelper() {
	if !taskInstalled() {
		return
	}
	// The task always runs the installed program and only starts on demand, so a
	// running helper just has to end: the next admin command starts the new version.
	if pipeUp() {
		quietCmd("schtasks", "/End", "/TN", elevTaskName).Run()
	}
}

func helperStatus() string {
	switch {
	case pipeUp():
		return "running"
	case taskInstalled():
		return "installed, not running"
	}
	return "not installed"
}

// moveTree merges src into dst (works across drives) and removes src once it's moved.
func moveTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	c := quietCmd("robocopy", src, dst, "/E", "/MOVE", "/COPY:DAT", "/DCOPY:T", "/R:1", "/W:1", "/NFL", "/NDL", "/NJH", "/NJS", "/NP")
	err := c.Run()
	if c.ProcessState != nil && c.ProcessState.ExitCode() < 8 {
		if exists(src) {
			return fmt.Errorf("some files are in use and stayed in %s", src)
		}
		return nil
	}
	if err == nil {
		err = fmt.Errorf("robocopy failed")
	}
	return err
}

// helperClientOK: the installed program, or an older ContextSaver still running after
// an update (install renames it to contextsaver.exe.old, later moved into old\).
func helperClientOK(client string) bool {
	if samePath(client, installPath()) {
		return true
	}
	base := strings.ToLower(filepath.Base(client))
	if !strings.HasPrefix(base, "contextsaver.exe.old") {
		return false
	}
	root := strings.ToLower(filepath.Clean(appRoot())) + `\`
	return strings.HasPrefix(strings.ToLower(filepath.Clean(client)), root)
}
