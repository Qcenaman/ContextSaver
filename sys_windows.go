//go:build windows

package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	procGlobalMemoryStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")
	procGetLogicalDrives     = modKernel32.NewProc("GetLogicalDrives")
	procGetDiskFreeSpaceExW  = modKernel32.NewProc("GetDiskFreeSpaceExW")
	procOpenProcess          = modKernel32.NewProc("OpenProcess")
	procGetExitCodeProcess   = modKernel32.NewProc("GetExitCodeProcess")
)

func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

func buildCommand(shell, command string) (*exec.Cmd, error) {
	flags := uint32(0x08000000 | 0x00000200) // CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP
	switch strings.ToLower(shell) {
	case "", "cmd":
		comspec := os.Getenv("ComSpec")
		if comspec == "" {
			comspec = `C:\Windows\System32\cmd.exe`
		}
		c := exec.Command(comspec)
		c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags,
			CmdLine: `cmd.exe /d /s /c "chcp 65001>nul & ` + command + `"`}
		return c, nil
	case "powershell", "pwsh":
		exe := "powershell.exe"
		if strings.ToLower(shell) == "pwsh" {
			exe = "pwsh.exe"
		}
		script := "[Console]::OutputEncoding=[Text.Encoding]::UTF8\n$ProgressPreference='SilentlyContinue'\n" + command + "\nif ($LASTEXITCODE) { exit $LASTEXITCODE }"
		u := utf16.Encode([]rune(script))
		b := make([]byte, len(u)*2)
		for i, x := range u {
			binary.LittleEndian.PutUint16(b[2*i:], x)
		}
		c := exec.Command(exe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", base64.StdEncoding.EncodeToString(b))
		c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		return c, nil
	}
	return nil, errors.New("shell must be cmd, powershell or pwsh")
}

func killTree(pid int) {
	quietCmd("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}

func pidAlive(pid int) bool {
	h, _, _ := procOpenProcess.Call(0x1000, 0, uintptr(pid)) // PROCESS_QUERY_LIMITED_INFORMATION
	if h == 0 {
		return false
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var code uint32
	r, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code)))
	return r != 0 && code == 259 // STILL_ACTIVE
}

func claudeRunning() bool {
	out, err := quietCmd("tasklist", "/FI", "IMAGENAME eq claude.exe", "/NH").Output()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "claude.exe")
}

func regString(path, name string) string {
	var h syscall.Handle
	p, _ := syscall.UTF16PtrFromString(path)
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, p, 0, syscall.KEY_READ, &h) != nil {
		return ""
	}
	defer syscall.RegCloseKey(h)
	n, _ := syscall.UTF16PtrFromString(name)
	var typ, size uint32
	syscall.RegQueryValueEx(h, n, nil, &typ, nil, &size)
	if size == 0 {
		return ""
	}
	buf := make([]uint16, size/2+1)
	if syscall.RegQueryValueEx(h, n, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size) != nil {
		return ""
	}
	return strings.TrimSpace(syscall.UTF16ToString(buf))
}

type memStatusEx struct {
	Length, MemoryLoad                     uint32
	TotalPhys, AvailPhys                   uint64
	TotalPageFile, AvailPageFile           uint64
	TotalVirtual, AvailVirtual, AvailExtra uint64
}

func platformInfo() []string {
	const nt = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	prod := regString(nt, "ProductName")
	build := regString(nt, "CurrentBuild")
	if b, _ := strconv.Atoi(build); b >= 22000 {
		prod = strings.Replace(prod, "Windows 10", "Windows 11", 1)
	}
	lines := []string{fmt.Sprintf("OS: %s %s (build %s)", prod, regString(nt, "DisplayVersion"), build)}
	cpu := regString(`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "ProcessorNameString")
	lines = append(lines, fmt.Sprintf("CPU: %s, %s logical cores", cpu, os.Getenv("NUMBER_OF_PROCESSORS")))
	var m memStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r != 0 {
		lines = append(lines, fmt.Sprintf("RAM: %s free of %s", hb(int64(m.AvailPhys)), hb(int64(m.TotalPhys))))
	}
	mask, _, _ := procGetLogicalDrives.Call()
	var disks []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A'+i)) + ":"
		t := driveType(letter + `\`)
		kind := map[uint32]string{2: "removable", 3: "fixed", 4: "network", 5: "CD/DVD", 6: "RAM disk"}[t]
		if t == 5 || kind == "" {
			continue
		}
		root, _ := syscall.UTF16PtrFromString(letter + `\`)
		var freeAvail, total, totalFree uint64
		r, _, _ := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(root)), uintptr(unsafe.Pointer(&freeAvail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
		if r == 0 {
			continue
		}
		if s := fmtDisk(letter, kind, freeAvail, total); s != "" {
			disks = append(disks, s)
		}
	}
	lines = append(lines, "Disks: "+strings.Join(disks, " · "))
	return lines
}

func procs() ([]procInfo, error) {
	out, err := runQuiet(30*time.Second, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"[Console]::OutputEncoding=[Text.Encoding]::UTF8; Get-CimInstance Win32_Process | Select-Object ProcessId,Name,WorkingSetSize,CommandLine | ConvertTo-Json -Compress")
	if err != nil {
		return nil, fmt.Errorf("couldn't list processes: %v", err)
	}
	var raw []struct {
		ProcessId      int
		Name           string
		WorkingSetSize int64
		CommandLine    string
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("couldn't read the process list: %v", err)
	}
	ps := make([]procInfo, 0, len(raw))
	for _, r := range raw {
		ps = append(ps, procInfo{pid: r.ProcessId, name: r.Name, mem: r.WorkingSetSize, cmd: r.CommandLine})
	}
	return ps, nil
}

func procNames() map[int]string {
	m := map[int]string{}
	out, err := quietCmd("tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return m
	}
	recs, _ := csv.NewReader(strings.NewReader(string(out))).ReadAll()
	for _, r := range recs {
		if len(r) >= 2 {
			if pid, err := strconv.Atoi(r[1]); err == nil {
				m[pid] = r[0]
			}
		}
	}
	return m
}

func listening() ([]portInfo, error) {
	out, err := quietCmd("netstat", "-ano").Output()
	if err != nil {
		return nil, fmt.Errorf("netstat failed: %v", err)
	}
	var ls []portInfo
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "TCP" || !strings.EqualFold(f[3], "LISTENING") {
			continue
		}
		i := strings.LastIndex(f[1], ":")
		if i < 0 {
			continue
		}
		port, _ := strconv.Atoi(f[1][i+1:])
		pid, _ := strconv.Atoi(f[4])
		ls = append(ls, portInfo{port: port, addr: f[1][:i], pid: pid})
	}
	return ls, nil
}
