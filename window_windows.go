//go:build windows

package main

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	procEnumWindows              = modUser32.NewProc("EnumWindows")
	procGetWindowTextW           = modUser32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW     = modUser32.NewProc("GetWindowTextLengthW")
	procGetWindowThreadProcessId = modUser32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = modUser32.NewProc("IsWindowVisible")
	procIsIconic                 = modUser32.NewProc("IsIconic")
	procGetWindowRect            = modUser32.NewProc("GetWindowRect")
	procGetWindowLongW           = modUser32.NewProc("GetWindowLongW")
	procSetForegroundWindow      = modUser32.NewProc("SetForegroundWindow")
	procGetForegroundWindow      = modUser32.NewProc("GetForegroundWindow")
	procShowWindow               = modUser32.NewProc("ShowWindow")
	procPrintWindow              = modUser32.NewProc("PrintWindow")
	procGetWindowDC              = modUser32.NewProc("GetWindowDC")
	procOpenClipboard            = modUser32.NewProc("OpenClipboard")
	procCloseClipboard           = modUser32.NewProc("CloseClipboard")
	procEmptyClipboard           = modUser32.NewProc("EmptyClipboard")
	procGetClipboardData         = modUser32.NewProc("GetClipboardData")
	procSetClipboardData         = modUser32.NewProc("SetClipboardData")
	procGetClipboardSeqNum       = modUser32.NewProc("GetClipboardSequenceNumber")
	procGlobalAlloc              = modKernel32.NewProc("GlobalAlloc")
	procGlobalLock               = modKernel32.NewProc("GlobalLock")
	procGlobalUnlock             = modKernel32.NewProc("GlobalUnlock")
	procGlobalSize               = modKernel32.NewProc("GlobalSize")
)

type winRect struct{ L, T, R, B int32 }

func listWindowsRaw() []winInfo {
	var out []winInfo
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		vis, _, _ := procIsWindowVisible.Call(h)
		n, _, _ := procGetWindowTextLengthW.Call(h)
		if vis == 0 || n == 0 {
			return 1
		}
		ex, _, _ := procGetWindowLongW.Call(h, uintptr(0xFFFFFFEC)) // GWL_EXSTYLE
		if ex&0x80 != 0 {                                           // WS_EX_TOOLWINDOW
			return 1
		}
		buf := make([]uint16, n+1)
		procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), n+1)
		var pid uint32
		procGetWindowThreadProcessId.Call(h, uintptr(unsafe.Pointer(&pid)))
		var r winRect
		procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&r)))
		mn, _, _ := procIsIconic.Call(h)
		out = append(out, winInfo{hwnd: h, title: syscall.UTF16ToString(buf), pid: int(pid), exe: filepath.Base(processPath(pid)),
			minimized: mn != 0, x: int(r.L), y: int(r.T), w: int(r.R - r.L), h: int(r.B - r.T)})
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return out
}

func foregroundHwnd() uintptr { h, _, _ := procGetForegroundWindow.Call(); return h }

func focusWindow(w winInfo) error {
	if w.minimized {
		procShowWindow.Call(w.hwnd, 9) // SW_RESTORE
		time.Sleep(250 * time.Millisecond)
	}
	sendKey(0x12, false) // a tap of Alt lets a background process take the foreground
	sendKey(0x12, true)
	procSetForegroundWindow.Call(w.hwnd)
	for i := 0; i < 20; i++ {
		if foregroundHwnd() == w.hwnd {
			time.Sleep(150 * time.Millisecond)
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("Windows didn't let ContextSaver bring that window to the front")
}

// captureWindow draws the window into an image even when other windows cover it.
func captureWindow(w winInfo) ([]byte, error) {
	if w.minimized {
		return nil, errors.New("that window is minimized")
	}
	if w.w <= 0 || w.h <= 0 || w.w*w.h > 8000*8000 {
		return nil, errors.New("bad window size")
	}
	hdc, _, _ := procGetWindowDC.Call(w.hwnd)
	if hdc == 0 {
		return nil, errors.New("can't read that window")
	}
	defer procReleaseDC.Call(w.hwnd, hdc)
	mdc, _, _ := procCreateCompatibleDC.Call(hdc)
	defer procDeleteDC.Call(mdc)
	bmi := struct {
		size                 uint32
		width, height        int32
		planes, bitCount     uint16
		compression, sizeImg uint32
		xppm, yppm           int32
		clrUsed, clrImp      uint32
	}{size: 40, width: int32(w.w), height: -int32(w.h), planes: 1, bitCount: 32}
	var bits unsafe.Pointer
	hbm, _, _ := procCreateDIBSection.Call(mdc, uintptr(unsafe.Pointer(&bmi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		return nil, errors.New("can't read that window")
	}
	defer procDeleteObject.Call(hbm)
	old, _, _ := procSelectObject.Call(mdc, hbm)
	r, _, _ := procPrintWindow.Call(w.hwnd, mdc, 2) // PW_RENDERFULLCONTENT
	procSelectObject.Call(mdc, old)
	px := append([]byte(nil), unsafe.Slice((*byte)(bits), w.w*w.h*4)...)
	if r == 0 {
		var err error
		if px, err = grabArea(w.x, w.y, w.w, w.h); err != nil {
			return nil, err
		}
	}
	return bgraPNG(px, w.w, w.h), nil
}

func clipSeq() uint32 { n, _, _ := procGetClipboardSeqNum.Call(); return uint32(n) }

func openClip() bool {
	for i := 0; i < 20; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// clipText returns the clipboard text; ok is false when it holds no text.
func clipText() (string, bool) {
	if !openClip() {
		return "", false
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(13) // CF_UNICODETEXT
	if h == 0 {
		return "", false
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return "", false
	}
	defer procGlobalUnlock.Call(h)
	n, _, _ := procGlobalSize.Call(h)
	return syscall.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Add(nil, p)), n/2)), true
}

func setClipText(s string) error {
	if !openClip() {
		return errors.New("clipboard busy")
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	u := utf16Units(s + "\x00")
	h, _, _ := procGlobalAlloc.Call(0x0002, uintptr(len(u)*2)) // GMEM_MOVEABLE
	if h == 0 {
		return errors.New("out of memory")
	}
	p, _, _ := procGlobalLock.Call(h)
	copy(unsafe.Slice((*uint16)(unsafe.Add(nil, p)), len(u)), u)
	procGlobalUnlock.Call(h)
	if r, _, _ := procSetClipboardData.Call(13, h); r == 0 {
		return errors.New("couldn't set the clipboard")
	}
	return nil
}

// copyWindowText focuses the window, presses Ctrl+A and Ctrl+C, reads the copy and puts the old clipboard text back.
func copyWindowText(w winInfo) (string, error) {
	prev, hadText := clipText()
	prevFg := foregroundHwnd()
	if err := focusWindow(w); err != nil {
		return "", err
	}
	seq := clipSeq()
	pressCombo("ctrl+a")
	time.Sleep(80 * time.Millisecond)
	pressCombo("ctrl+c")
	got := ""
	for i := 0; i < 40; i++ {
		time.Sleep(25 * time.Millisecond)
		if clipSeq() != seq {
			time.Sleep(30 * time.Millisecond)
			got, _ = clipText()
			break
		}
	}
	pressCombo("right") // drop the selection
	if hadText {
		setClipText(prev)
	}
	if prevFg != 0 && prevFg != w.hwnd {
		procSetForegroundWindow.Call(prevFg)
	}
	if strings.TrimSpace(got) == "" {
		return "", errors.New("nothing was copied (the window may not allow selecting its text)")
	}
	return got, nil
}

// pasteText puts text in through the clipboard (fast and exact), then restores the clipboard text.
func pasteText(s string) error {
	prev, had := clipText()
	if err := setClipText(s); err != nil {
		return err
	}
	time.Sleep(30 * time.Millisecond)
	pressCombo("ctrl+v")
	time.Sleep(150 * time.Millisecond)
	if had {
		setClipText(prev)
	}
	return nil
}
