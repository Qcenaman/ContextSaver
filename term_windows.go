//go:build windows

package main

import (
	"os"
	"sync"
	"time"
	"unsafe"
)

var (
	procGetConsoleMode             = modKernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = modKernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = modKernel32.NewProc("GetConsoleScreenBufferInfo")
	procSetConsoleOutputCP         = modKernel32.NewProc("SetConsoleOutputCP")
)

// isTerminal: f is a console a person can type in or read, not a pipe, a file or NUL
// (NUL is a character device too, so the file type can't tell; only a console has a mode).
func isTerminal(f *os.File) bool {
	var m uint32
	r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&m)))
	return r != 0
}

type consoleInfo struct {
	sizeX, sizeY             int16
	curX, curY               int16
	attr                     uint16
	left, top, right, bottom int16
	maxX, maxY               int16
}

// termSize: rows and columns of the console window (0, 0 when f isn't one).
func termSize(f *os.File) (rows, cols int) {
	var ci consoleInfo
	r, _, _ := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&ci)))
	if r == 0 {
		return 0, 0
	}
	return int(ci.bottom-ci.top) + 1, int(ci.right-ci.left) + 1
}

// enableVT turns on ANSI escape codes (colors, the chat's command bar) in this console
// and UTF-8 output; false when the console can't (very old Windows), so no codes are sent.
func enableVT() bool {
	const enableVirtualTerminalProcessing = 0x0004
	h := os.Stdout.Fd()
	var m uint32
	if r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&m))); r == 0 {
		return false
	}
	procSetConsoleOutputCP.Call(65001)
	if m&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ := procSetConsoleMode.Call(h, uintptr(m|enableVirtualTerminalProcessing))
	return r != 0
}

// watchResize calls redraw when the console window changes size (Windows sends no
// signal for that, so it looks twice a second); the returned func stops it.
func watchResize(redraw func()) func() {
	stop := make(chan struct{})
	go func() {
		r0, c0 := termSize(os.Stdout)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if r, c := termSize(os.Stdout); r != r0 || c != c0 {
					r0, c0 = r, c
					redraw()
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}
