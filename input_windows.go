//go:build windows

package main

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Mouse, keyboard and screen pixels through user32/gdi32, for macros and triggers.

var (
	modUser32                     = syscall.NewLazyDLL("user32.dll")
	modGdi32                      = syscall.NewLazyDLL("gdi32.dll")
	procSendInput                 = modUser32.NewProc("SendInput")
	procSetCursorPos              = modUser32.NewProc("SetCursorPos")
	procGetCursorPos              = modUser32.NewProc("GetCursorPos")
	procGetAsyncKeyState          = modUser32.NewProc("GetAsyncKeyState")
	procSetWindowsHookExW         = modUser32.NewProc("SetWindowsHookExW")
	procCallNextHookEx            = modUser32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx       = modUser32.NewProc("UnhookWindowsHookEx")
	procGetMessageW               = modUser32.NewProc("GetMessageW")
	procPostThreadMessageW        = modUser32.NewProc("PostThreadMessageW")
	procGetDC                     = modUser32.NewProc("GetDC")
	procReleaseDC                 = modUser32.NewProc("ReleaseDC")
	procSetProcessDpiAwarenessCtx = modUser32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware        = modUser32.NewProc("SetProcessDPIAware")
	procMapVirtualKeyW            = modUser32.NewProc("MapVirtualKeyW")
	procGetModuleHandleW          = modKernel32.NewProc("GetModuleHandleW")
	procGetCurrentThreadId        = modKernel32.NewProc("GetCurrentThreadId")
	procCreateCompatibleDC        = modGdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection          = modGdi32.NewProc("CreateDIBSection")
	procSelectObject              = modGdi32.NewProc("SelectObject")
	procBitBlt                    = modGdi32.NewProc("BitBlt")
	procDeleteObject              = modGdi32.NewProc("DeleteObject")
	procDeleteDC                  = modGdi32.NewProc("DeleteDC")
)

type winPoint struct{ X, Y int32 }

type mouseInput struct {
	dx, dy             int32
	mouseData, dwFlags uint32
	time               uint32
	dwExtraInfo        uintptr
}

type keybdInput struct {
	wVk, wScan  uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
	_           [8]byte
}

type inputMouse struct {
	typ uint32
	mi  mouseInput
}

type inputKey struct {
	typ uint32
	ki  keybdInput
}

var dpiOnce sync.Once

// dpiAware makes this process see real screen pixels (displays with scaling report smaller sizes otherwise).
func dpiAware() {
	dpiOnce.Do(func() {
		if procSetProcessDpiAwarenessCtx.Find() == nil {
			if r, _, _ := procSetProcessDpiAwarenessCtx.Call(^uintptr(3)); r != 0 { // PER_MONITOR_AWARE_V2 (-4)
				return
			}
		}
		procSetProcessDPIAware.Call()
	})
}

func cursorPos() (int, int) {
	var p winPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return int(p.X), int(p.Y)
}

func setCursor(x, y int) {
	procSetCursorPos.Call(uintptr(int32(x)), uintptr(int32(y)))
}

func sendMouse(flags uint32, data int32) {
	in := inputMouse{typ: 0, mi: mouseInput{mouseData: uint32(data), dwFlags: flags}}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

var buttonFlags = map[string][2]uint32{"left": {0x2, 0x4}, "right": {0x8, 0x10}, "middle": {0x20, 0x40}}

func mouseButton(button string, down bool) error {
	f, ok := buttonFlags[button]
	if !ok {
		return errors.New("button must be left, right or middle")
	}
	if down {
		sendMouse(f[0], 0)
	} else {
		sendMouse(f[1], 0)
	}
	return nil
}

func mouseWheel(notches int) { sendMouse(0x800, int32(notches*120)) }

var extendedVK = map[uint16]bool{0x21: true, 0x22: true, 0x23: true, 0x24: true, 0x25: true, 0x26: true, 0x27: true, 0x28: true,
	0x2C: true, 0x2D: true, 0x2E: true, 0x5B: true, 0x5C: true, 0x5D: true, 0x6F: true, 0x90: true, 0xA3: true, 0xA5: true}

func sendKey(vk uint16, up bool) {
	scan, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0)
	var flags uint32
	if up {
		flags |= 0x2
	}
	if extendedVK[vk] {
		flags |= 0x1
	}
	in := inputKey{typ: 1, ki: keybdInput{wVk: vk, wScan: uint16(scan), dwFlags: flags}}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

// typeText types text as Unicode characters, whatever the keyboard layout.
func typeText(s string) {
	for _, u := range utf16Units(s) {
		in := [2]inputKey{{typ: 1, ki: keybdInput{wScan: u, dwFlags: 0x4}}, {typ: 1, ki: keybdInput{wScan: u, dwFlags: 0x6}}}
		procSendInput.Call(2, uintptr(unsafe.Pointer(&in[0])), unsafe.Sizeof(in[0]))
		time.Sleep(12 * time.Millisecond)
	}
}

func keyHeld(vk uint16) bool {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return r&0x8000 != 0
}

// grabArea returns the screen pixels of an area as BGRA rows.
func grabArea(x, y, w, h int) ([]byte, error) {
	if w <= 0 || h <= 0 || w*h > 4000*4000 {
		return nil, errors.New("bad area size")
	}
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return nil, errors.New("can't read the screen")
	}
	defer procReleaseDC.Call(0, hdc)
	mdc, _, _ := procCreateCompatibleDC.Call(hdc)
	if mdc == 0 {
		return nil, errors.New("can't read the screen")
	}
	defer procDeleteDC.Call(mdc)
	bmi := struct {
		size                 uint32
		width, height        int32
		planes, bitCount     uint16
		compression, sizeImg uint32
		xppm, yppm           int32
		clrUsed, clrImp      uint32
		colors               [4]byte
	}{size: 40, width: int32(w), height: -int32(h), planes: 1, bitCount: 32}
	var bits unsafe.Pointer
	hbm, _, _ := procCreateDIBSection.Call(mdc, uintptr(unsafe.Pointer(&bmi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		return nil, errors.New("can't read the screen")
	}
	defer procDeleteObject.Call(hbm)
	old, _, _ := procSelectObject.Call(mdc, hbm)
	r, _, _ := procBitBlt.Call(mdc, 0, 0, uintptr(w), uintptr(h), hdc, uintptr(int32(x)), uintptr(int32(y)), 0x00CC0020)
	procSelectObject.Call(mdc, old)
	if r == 0 {
		return nil, errors.New("can't read that part of the screen")
	}
	out := make([]byte, w*h*4)
	copy(out, unsafe.Slice((*byte)(bits), w*h*4))
	return out, nil
}

// ---------- recording ----------

type msllHook struct {
	X, Y      int32
	mouseData uint32
	flags     uint32
	time      uint32
	extra     uintptr
}

type kbdllHook struct {
	vk, scan, flags, time uint32
	extra                 uintptr
}

var (
	recMu      sync.Mutex
	recOn      bool
	recEvents  []rawInput
	recStart   time.Time
	recStopVK  uint32
	recThread  uintptr
	hookOnce   sync.Once
	keyHookCB  uintptr
	mouseCB    uintptr
	errRecBusy = errors.New("already recording")
)

func recAdd(e rawInput) {
	recMu.Lock()
	e.at = time.Since(recStart)
	recEvents = append(recEvents, e)
	recMu.Unlock()
}

func makeHooks() {
	mouseCB = syscall.NewCallback(func(code int, wp uintptr, h *msllHook) uintptr {
		if code >= 0 && h.flags&1 == 0 {
			switch wp {
			case 0x201, 0x202, 0x204, 0x205, 0x207, 0x208:
				btn := map[uintptr]string{0x201: "left", 0x202: "left", 0x204: "right", 0x205: "right", 0x207: "middle", 0x208: "middle"}[wp]
				recAdd(rawInput{kind: "mouse", button: btn, down: wp == 0x201 || wp == 0x204 || wp == 0x207, x: int(h.X), y: int(h.Y)})
			case 0x20A:
				recAdd(rawInput{kind: "wheel", wheel: int(int16(h.mouseData >> 16)), x: int(h.X), y: int(h.Y)})
			case 0x200:
				if keyHeld(0x01) || keyHeld(0x02) {
					recAdd(rawInput{kind: "move", x: int(h.X), y: int(h.Y)})
				}
			}
		}
		r, _, _ := procCallNextHookEx.Call(0, uintptr(code), wp, uintptr(unsafe.Pointer(h)))
		return r
	})
	keyHookCB = syscall.NewCallback(func(code int, wp uintptr, h *kbdllHook) uintptr {
		if code >= 0 && h.flags&0x10 == 0 {
			down := wp == 0x100 || wp == 0x104
			if h.vk == recStopVK {
				if down {
					procPostThreadMessageW.Call(recThread, 0x12, 0, 0) // WM_QUIT
				}
				return 1 // swallow the stop key
			}
			recAdd(rawInput{kind: "key", vk: uint16(h.vk), down: down})
		}
		r, _, _ := procCallNextHookEx.Call(0, uintptr(code), wp, uintptr(unsafe.Pointer(h)))
		return r
	})
}

// recordInput records clicks, wheel, drags and keys until stopVK is pressed.
func recordInput(stopVK uint16) ([]rawInput, error) {
	recMu.Lock()
	if recOn {
		recMu.Unlock()
		return nil, errRecBusy
	}
	recOn, recEvents, recStart, recStopVK = true, nil, time.Now(), uint32(stopVK)
	recMu.Unlock()
	defer func() { recMu.Lock(); recOn = false; recMu.Unlock() }()
	hookOnce.Do(makeHooks)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tid, _, _ := procGetCurrentThreadId.Call()
	recThread = tid
	mod, _, _ := procGetModuleHandleW.Call(0)
	mh, _, err := procSetWindowsHookExW.Call(14, mouseCB, mod, 0) // WH_MOUSE_LL
	if mh == 0 {
		return nil, errors.New("couldn't watch the mouse: " + err.Error())
	}
	defer procUnhookWindowsHookEx.Call(mh)
	kh, _, err := procSetWindowsHookExW.Call(13, keyHookCB, mod, 0) // WH_KEYBOARD_LL
	if kh == 0 {
		return nil, errors.New("couldn't watch the keyboard: " + err.Error())
	}
	defer procUnhookWindowsHookEx.Call(kh)
	var msg [48]byte
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			break
		}
	}
	recMu.Lock()
	defer recMu.Unlock()
	return append([]rawInput(nil), recEvents...), nil
}

const inputSupported = true

var errNoInput = errors.New("macros and screen reading need Windows")
