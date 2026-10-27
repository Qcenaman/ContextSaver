//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func hideWindow(c *exec.Cmd) {}

func buildCommand(shell, command string) (*exec.Cmd, error) {
	switch strings.ToLower(shell) {
	case "", "cmd", "sh":
	default:
		return nil, errors.New("only the default shell is available here")
	}
	c := exec.Command("/bin/sh", "-c", command)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return c, nil
}

func killTree(pid int) {
	if syscall.Kill(-pid, syscall.SIGKILL) != nil {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

func pidAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		f := strings.Fields(string(b))
		if len(f) > 2 && f[2] == "Z" {
			return false
		}
	}
	return true
}

func claudeRunning() bool { return false }

func platformInfo() []string {
	lines := []string{fmt.Sprintf("OS: %s/%s", runtime.GOOS, runtime.GOARCH), fmt.Sprintf("CPU: %d logical cores", runtime.NumCPU())}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, avail int64
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				v, _ := strconv.ParseInt(f[1], 10, 64)
				switch f[0] {
				case "MemTotal:":
					total = v * 1024
				case "MemAvailable:":
					avail = v * 1024
				}
			}
		}
		lines = append(lines, fmt.Sprintf("RAM: %s free of %s", hb(avail), hb(total)))
	}
	var st syscall.Statfs_t
	if syscall.Statfs("/", &st) == nil {
		lines = append(lines, "Disks: "+fmtDisk("/", "fixed", st.Bavail*uint64(st.Bsize), st.Blocks*uint64(st.Bsize)))
	}
	return lines
}

func procs() ([]procInfo, error) {
	out, err := runQuiet(15*time.Second, "ps", "-eo", "pid=,rss=,comm=,args=")
	if err != nil {
		return nil, err
	}
	var ps []procInfo
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		rss, _ := strconv.ParseInt(f[1], 10, 64)
		ps = append(ps, procInfo{pid: pid, name: f[2], mem: rss * 1024, cmd: strings.Join(f[3:], " ")})
	}
	return ps, nil
}

func procNames() map[int]string {
	m := map[int]string{}
	ps, _ := procs()
	for _, p := range ps {
		m[p.pid] = p.name
	}
	return m
}

func listening() ([]portInfo, error) { return nil, errors.New("ports isn't supported on this system") }
