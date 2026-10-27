//go:build !windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func recycleBatch(paths []string) error { return errors.New("no Recycle Bin on this system") }
func useRecycleBin(p string) bool       { return false }
func holdRoot(p string) string          { return filepath.Join(appHome, "trash") }
func stripVol(p string) string          { return strings.TrimPrefix(p, "/") }

func desktopDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Desktop")
}

func setFileTimes(p string, st fs.FileInfo)   { os.Chtimes(p, st.ModTime(), st.ModTime()) }
func setFileAttrs(p string, st fs.FileInfo)   {}
func creationTime(info fs.FileInfo) time.Time { return info.ModTime() }
func isCrossDevice(err error) bool            { return errors.Is(err, syscall.EXDEV) }
func sameVolume(a, b string) bool             { return true }

func restoreRecycled(orig string, at time.Time) error {
	return errors.New("no Recycle Bin on this system")
}

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

func startScript(script string) (*aiProc, error) {
	cmd := exec.Command("/bin/sh", script)
	cmd.Dir = filepath.Dir(script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &aiProc{cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); close(p.done) }()
	return p, nil
}

func stopLocalAI(p *aiProc, port int) {
	if p.cmd.Process != nil {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func quietCmd(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

func pidOnPort(port int) int { return 0 }
