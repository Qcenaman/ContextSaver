//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	modShell32               = syscall.NewLazyDLL("shell32.dll")
	modKernel32              = syscall.NewLazyDLL("kernel32.dll")
	modOle32                 = syscall.NewLazyDLL("ole32.dll")
	procSHFileOperationW     = modShell32.NewProc("SHFileOperationW")
	procSHGetKnownFolderPath = modShell32.NewProc("SHGetKnownFolderPath")
	procGetDriveTypeW        = modKernel32.NewProc("GetDriveTypeW")
	procCoInitializeEx       = modOle32.NewProc("CoInitializeEx")
	procCoTaskMemFree        = modOle32.NewProc("CoTaskMemFree")
)

type shFileOpStruct struct {
	Hwnd                  uintptr
	WFunc                 uint32
	PFrom                 *uint16
	PTo                   *uint16
	FFlags                uint16
	FAnyOperationsAborted int32
	HNameMappings         uintptr
	LpszProgressTitle     *uint16
}

const (
	foDelete           = 3
	fofSilent          = 0x4
	fofNoConfirmation  = 0x10
	fofAllowUndo       = 0x40
	fofNoErrorUI       = 0x400
	fofWantNukeWarning = 0x4000
)

type recycleReq struct {
	paths []string
	done  chan error
}

var (
	recycleCh   = make(chan recycleReq)
	recycleOnce sync.Once
)

// All Recycle Bin calls go through one thread, as the Windows shell expects.
func recycleWorker() {
	runtime.LockOSThread()
	procCoInitializeEx.Call(0, 0x2|0x4)
	for r := range recycleCh {
		r.done <- shDelete(r.paths)
	}
}

func recycleBatch(paths []string) error {
	recycleOnce.Do(func() { go recycleWorker() })
	done := make(chan error, 1)
	recycleCh <- recycleReq{paths, done}
	return <-done
}

func shDelete(paths []string) error {
	var buf []uint16
	for _, p := range paths {
		buf = append(buf, utf16.Encode([]rune(p))...)
		buf = append(buf, 0)
	}
	buf = append(buf, 0)
	op := shFileOpStruct{WFunc: foDelete, PFrom: &buf[0],
		FFlags: fofSilent | fofNoConfirmation | fofAllowUndo | fofNoErrorUI | fofWantNukeWarning}
	r, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	runtime.KeepAlive(buf)
	if r != 0 {
		return fmt.Errorf("Windows error 0x%x", r)
	}
	if op.FAnyOperationsAborted != 0 {
		return errors.New("cancelled")
	}
	return nil
}

func driveType(p string) uint32 {
	vol := filepath.VolumeName(p)
	if vol == "" || strings.HasPrefix(vol, `\\`) {
		return 4
	}
	root, _ := syscall.UTF16PtrFromString(vol + `\`)
	r, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(root)))
	return uint32(r)
}

// Fixed drives use the Recycle Bin; USB sticks and network drives have none,
// so files there go to a holding folder on the same drive instead.
func useRecycleBin(p string) bool { return driveType(p) == 3 && len(p) < 250 }

// Held (undoable) deletes from drives without a Recycle Bin go into appRoot too.
func holdRoot(p string) string { return filepath.Join(appRoot(), "undo-hold") }

func stripVol(p string) string {
	return strings.TrimPrefix(p[len(filepath.VolumeName(p)):], `\`)
}

var folderIDDesktop = syscall.GUID{Data1: 0xB4BFCC3A, Data2: 0xDB2C, Data3: 0x424C, Data4: [8]byte{0xB0, 0x29, 0x7F, 0xE9, 0x9A, 0x87, 0xC6, 0x41}}

func desktopDir() string {
	var p *uint16
	r, _, _ := procSHGetKnownFolderPath.Call(uintptr(unsafe.Pointer(&folderIDDesktop)), 0, 0, uintptr(unsafe.Pointer(&p)))
	if r != 0 || p == nil {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, "Desktop")
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	var u []uint16
	for ptr := unsafe.Pointer(p); ; ptr = unsafe.Add(ptr, 2) {
		c := *(*uint16)(ptr)
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

func setFileTimes(path string, st fs.FileInfo) {
	d, ok := st.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		os.Chtimes(path, st.ModTime(), st.ModTime())
		return
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	h, err := syscall.CreateFile(p, 0x100, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		os.Chtimes(path, st.ModTime(), st.ModTime())
		return
	}
	defer syscall.CloseHandle(h)
	syscall.SetFileTime(h, &d.CreationTime, &d.LastAccessTime, &d.LastWriteTime)
}

func setFileAttrs(path string, st fs.FileInfo) {
	d, ok := st.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return
	}
	a := d.FileAttributes & (syscall.FILE_ATTRIBUTE_READONLY | syscall.FILE_ATTRIBUTE_HIDDEN | syscall.FILE_ATTRIBUTE_SYSTEM | syscall.FILE_ATTRIBUTE_ARCHIVE)
	if a&^syscall.FILE_ATTRIBUTE_ARCHIVE == 0 {
		return
	}
	if p, err := syscall.UTF16PtrFromString(path); err == nil {
		syscall.SetFileAttributes(p, a)
	}
}

func creationTime(info fs.FileInfo) time.Time {
	if d, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, d.CreationTime.Nanoseconds())
	}
	return info.ModTime()
}

func isCrossDevice(err error) bool {
	var le *os.LinkError
	if errors.As(err, &le) {
		if en, ok := le.Err.(syscall.Errno); ok && en == 17 {
			return true
		}
	}
	return false
}

func sameVolume(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b))
}

func currentSID() (string, error) {
	t, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer t.Close()
	u, err := t.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String()
}

// restoreRecycled finds the Recycle Bin entry for orig that was deleted after
// `at` and moves it back to where it was.
func restoreRecycled(orig string, at time.Time) error {
	sid, err := currentSID()
	if err != nil {
		return err
	}
	dir := filepath.Join(filepath.VolumeName(orig)+`\`, "$Recycle.Bin", sid)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("couldn't open the Recycle Bin: %v", err)
	}
	best, bestT := "", time.Time{}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "$I") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil || len(b) < 28 {
			continue
		}
		ver := binary.LittleEndian.Uint64(b[0:8])
		ft := syscall.Filetime{LowDateTime: binary.LittleEndian.Uint32(b[16:20]), HighDateTime: binary.LittleEndian.Uint32(b[20:24])}
		t := time.Unix(0, ft.Nanoseconds())
		var raw []byte
		if ver >= 2 {
			nch := int(binary.LittleEndian.Uint32(b[24:28]))
			raw = b[28:min(len(b), 28+2*nch)]
		} else {
			raw = b[24:min(len(b), 24+520)]
		}
		u := make([]uint16, len(raw)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(raw[2*i:])
		}
		path := strings.TrimRight(string(utf16.Decode(u)), "\x00")
		if strings.EqualFold(path, orig) && !t.Before(at.Add(-2*time.Minute)) && t.After(bestT) {
			best, bestT = n, t
		}
	}
	if best == "" {
		return errors.New("not in the Recycle Bin anymore (maybe it was emptied)")
	}
	if err := os.MkdirAll(filepath.Dir(orig), 0o755); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(dir, "$R"+best[2:]), orig); err != nil {
		return err
	}
	os.Remove(filepath.Join(dir, best))
	return nil
}

// ---------- local AI process ----------

type aiProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *aiProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// startScript runs the user's model script in its own console window, so it's visible while it runs.
func startScript(script string) (*aiProc, error) {
	cmd := exec.Command("cmd.exe", "/c", script)
	cmd.Dir = filepath.Dir(script)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010} // CREATE_NEW_CONSOLE
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &aiProc{cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); close(p.done) }()
	return p, nil
}

func quietCmd(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return c
}

func stopLocalAI(p *aiProc, port int) {
	if p.cmd.Process != nil && !p.exited() {
		quietCmd("taskkill", "/PID", strconv.Itoa(p.cmd.Process.Pid), "/T", "/F").Run()
	}
	// If the script launched the server separately, stop whatever is listening on its port.
	for i := 0; i < 5; i++ {
		if !portAnswers(port) {
			return
		}
		if pid := pidOnPort(port); pid > 0 {
			quietCmd("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
		}
		time.Sleep(time.Second)
	}
}

func portAnswers(port int) bool {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func pidOnPort(port int) int {
	out, err := quietCmd("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return 0
	}
	suffix := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && strings.EqualFold(f[3], "LISTENING") && strings.HasSuffix(f[1], suffix) {
			if pid, err := strconv.Atoi(f[4]); err == nil {
				return pid
			}
		}
	}
	return 0
}
