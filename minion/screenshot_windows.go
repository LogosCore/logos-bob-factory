//go:build windows

package main

import (
	"errors"
	"fmt"
	"image"
	"sync"
	"syscall"
	"unsafe"
)

// A screenshot on Windows: the desktop of the session the minion runs in,
// copied by GDI (BitBlt from the screen's device context, then GetDIBits),
// every monitor's in one picture (the virtual screen) or one monitor's. The
// minion asks for the desktop's own pixels (DPI aware, per monitor where
// Windows has that), not a copy scaled for a program that is not. A minion
// run as a service is in session 0, which has no desktop to take.

var (
	user32                            = syscall.NewLazyDLL("user32.dll")
	gdi32                             = syscall.NewLazyDLL("gdi32.dll")
	procGetDC                         = user32.NewProc("GetDC")
	procReleaseDC                     = user32.NewProc("ReleaseDC")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procEnumDisplayMonitors           = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procCreateCompatibleDC            = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap        = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject                  = gdi32.NewProc("SelectObject")
	procBitBlt                        = gdi32.NewProc("BitBlt")
	procGetDIBits                     = gdi32.NewProc("GetDIBits")
	procDeleteObject                  = gdi32.NewProc("DeleteObject")
	procDeleteDC                      = gdi32.NewProc("DeleteDC")
	procProcessIdToSessionId          = kernel32.NewProc("ProcessIdToSessionId")
)

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
	srcCopy           = 0x00CC0020
	// captureBlt takes layered windows too.
	captureBlt         = 0x40000000
	monitorInfoPrimary = 1
	// perMonitorAwareV2 is DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, -4.
	perMonitorAwareV2 = ^uintptr(3)
)

type rect struct{ left, top, right, bottom int32 }

// monitorInfoEx is MONITORINFOEXW.
type monitorInfoEx struct {
	size    uint32
	monitor rect
	work    rect
	flags   uint32
	device  [32]uint16
}

// bitmapInfo is BITMAPINFO: its header, room for its colours.
type bitmapInfo struct {
	size                         uint32
	width, height                int32
	planes, bitCount             uint16
	compression, sizeImage       uint32
	xPelsPerMeter, yPelsPerMeter int32
	clrUsed, clrImportant        uint32
	colors                       [4]uint32
}

var dpiAwareOnce sync.Once

// capture takes a picture of screen n of the session's desktop, 0 for every
// monitor in one picture. Windows has no X display: the second is not used.
func capture(n int, _ string) (*shot, error) {
	if sessionZero() {
		return nil, errors.New("the minion runs in session 0, as a service: it has no desktop to take")
	}
	dpiAwareOnce.Do(dpiAware)
	area := monitor{x: metric(smXVirtualScreen), y: metric(smYVirtualScreen), w: metric(smCXVirtualScreen), h: metric(smCYVirtualScreen)}
	screens := 0
	if n > 0 {
		ms, err := enumMonitors()
		if err != nil {
			return nil, err
		}
		if area, err = pickScreen(ms, n); err != nil {
			return nil, err
		}
		screens = len(ms)
	}
	img, err := grab(area)
	if err != nil {
		return nil, err
	}
	return &shot{img: img, display: area.name, screens: screens}, nil
}

// sessionZero reports whether the minion runs in session 0, the services'.
func sessionZero() bool {
	var sid uint32
	r, _, _ := procProcessIdToSessionId.Call(uintptr(syscall.Getpid()), uintptr(unsafe.Pointer(&sid)))
	return r != 0 && sid == 0
}

// dpiAware asks for the desktop's own pixels: per monitor (Windows 10 1703
// on), else the system's.
func dpiAware() {
	if procSetProcessDpiAwarenessContext.Find() == nil {
		if r, _, _ := procSetProcessDpiAwarenessContext.Call(perMonitorAwareV2); r != 0 {
			return
		}
	}
	if procSetProcessDPIAware.Find() == nil {
		_, _, _ = procSetProcessDPIAware.Call()
	}
}

// metric is GetSystemMetrics(i), signed: the virtual screen may start left
// of or above the main monitor.
func metric(i uintptr) int {
	r, _, _ := procGetSystemMetrics.Call(i)
	return int(int32(r))
}

var (
	// monitorsMu keeps one enumeration at a time, as its callback fills
	// monitorsSeen.
	monitorsMu   sync.Mutex
	monitorsSeen []monitor
	// monitorsCallback is made once: Windows callbacks are few per process.
	monitorsCallback = sync.OnceValue(func() uintptr { return syscall.NewCallback(monitorFound) })
)

// monitorFound is EnumDisplayMonitors' callback: a monitor, its rectangle
// on the desktop and its device name (\\.\DISPLAY1).
func monitorFound(hmon, _, _, _ uintptr) uintptr {
	mi := monitorInfoEx{size: uint32(unsafe.Sizeof(monitorInfoEx{}))}
	if r, _, _ := procGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi))); r != 0 {
		m := mi.monitor
		monitorsSeen = append(monitorsSeen, monitor{name: syscall.UTF16ToString(mi.device[:]), x: int(m.left), y: int(m.top),
			w: int(m.right - m.left), h: int(m.bottom - m.top), primary: mi.flags&monitorInfoPrimary != 0})
	}
	return 1
}

// enumMonitors are the desktop's monitors, in Windows' order.
func enumMonitors() ([]monitor, error) {
	monitorsMu.Lock()
	defer monitorsMu.Unlock()
	monitorsSeen = nil
	if r, _, err := procEnumDisplayMonitors.Call(0, 0, monitorsCallback(), 0); r == 0 {
		return nil, fmt.Errorf("EnumDisplayMonitors: %w", err)
	}
	return monitorsSeen, nil
}

// grab copies a rectangle of the desktop into an RGBA picture.
func grab(a monitor) (*image.RGBA, error) {
	if a.w <= 0 || a.h <= 0 {
		return nil, fmt.Errorf("a desktop of %d × %d pixels", a.w, a.h)
	}
	screen, _, _ := procGetDC.Call(0)
	if screen == 0 {
		return nil, errors.New("GetDC: the session has no desktop to take")
	}
	defer func() { _, _, _ = procReleaseDC.Call(0, screen) }()
	mem, _, _ := procCreateCompatibleDC.Call(screen)
	if mem == 0 {
		return nil, errors.New("CreateCompatibleDC failed")
	}
	defer func() { _, _, _ = procDeleteDC.Call(mem) }()
	bmp, _, _ := procCreateCompatibleBitmap.Call(screen, uintptr(a.w), uintptr(a.h))
	if bmp == 0 {
		return nil, fmt.Errorf("CreateCompatibleBitmap of %d × %d pixels failed", a.w, a.h)
	}
	defer func() { _, _, _ = procDeleteObject.Call(bmp) }()
	old, _, _ := procSelectObject.Call(mem, bmp)
	ok, _, err := procBitBlt.Call(mem, 0, 0, uintptr(a.w), uintptr(a.h), screen, uintptr(a.x), uintptr(a.y), srcCopy|captureBlt)
	// GetDIBits takes a bitmap no device context holds.
	_, _, _ = procSelectObject.Call(mem, old)
	if ok == 0 {
		return nil, fmt.Errorf("BitBlt: %w (a locked or secure desktop?)", err)
	}
	img := image.NewRGBA(image.Rect(0, 0, a.w, a.h))
	// Top-down rows (a negative height), 32 bits a pixel: BGRA.
	bi := bitmapInfo{width: int32(a.w), height: -int32(a.h), planes: 1, bitCount: 32}
	bi.size = uint32(unsafe.Offsetof(bi.colors))
	lines, _, err := procGetDIBits.Call(screen, bmp, 0, uintptr(a.h), uintptr(unsafe.Pointer(&img.Pix[0])), uintptr(unsafe.Pointer(&bi)), 0)
	if int(lines) != a.h {
		return nil, fmt.Errorf("GetDIBits: %w", err)
	}
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+2], img.Pix[i+3] = img.Pix[i+2], img.Pix[i], 255
	}
	return img, nil
}
