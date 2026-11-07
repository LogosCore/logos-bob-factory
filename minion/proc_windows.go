//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A terminal on Windows: a pseudo console (ConPTY, Windows 10 1809 and
// later), the process created inside it. A Windows without one runs the
// process on pipes, and the reply says so. An interrupt is a Ctrl-C typed
// into the console; terminate and kill end the whole tree (taskkill /T),
// as a cancel of a shell task does. On pipes there is no console to type
// into: an interrupt is let go.

var (
	procCreatePseudoConsole               = kernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole               = kernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole                = kernel32.NewProc("ClosePseudoConsole")
	procInitializeProcThreadAttributeList = kernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute         = kernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList     = kernel32.NewProc("DeleteProcThreadAttributeList")
	procCreateProcessW                    = kernel32.NewProc("CreateProcessW")
)

const (
	procThreadAttributePseudoConsole = 0x00020016
	extendedStartupInfoPresent       = 0x00080000
)

// hostShell is the host's shell: %ComSpec%, else cmd.exe.
func hostShell() string {
	if s := os.Getenv("ComSpec"); s != "" {
		return s
	}
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
}

type startupInfoEx struct {
	syscall.StartupInfo
	attributeList uintptr
}

// coord is a console's size as COORD, by value.
func coord(rows, cols int) uintptr {
	return uintptr(uint32(uint16(cols)) | uint32(uint16(rows))<<16)
}

// commandLine is path and args as CreateProcess takes them.
func commandLine(path string, args []string) string {
	parts := []string{syscall.EscapeArg(path)}
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

func startTerminal(path string, args []string, rows, cols int) (*proc, error) {
	if procCreatePseudoConsole.Find() != nil {
		return nil, errNoTerminal
	}
	var inR, inW, outR, outW syscall.Handle
	if err := syscall.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, err
	}
	if err := syscall.CreatePipe(&outR, &outW, nil, 0); err != nil {
		_ = syscall.CloseHandle(inR)
		_ = syscall.CloseHandle(inW)
		return nil, err
	}
	closeAll := func(hs ...syscall.Handle) {
		for _, h := range hs {
			_ = syscall.CloseHandle(h)
		}
	}
	var hpc uintptr
	if hr, _, _ := procCreatePseudoConsole.Call(coord(rows, cols), uintptr(inR), uintptr(outW), 0, uintptr(unsafe.Pointer(&hpc))); hr != 0 {
		closeAll(inR, inW, outR, outW)
		return nil, fmt.Errorf("CreatePseudoConsole: 0x%x", uint32(hr))
	}
	pid, process, err := createInConsole(hpc, path, args)
	// The console has its ends now.
	closeAll(inR, outW)
	if err != nil {
		_, _, _ = procClosePseudoConsole.Call(hpc)
		closeAll(inW, outR)
		return nil, err
	}
	p := newProc(os.NewFile(uintptr(outR), "conpty-out"), os.NewFile(uintptr(inW), "conpty-in"))
	p.resizeFn = func(rows, cols int) error {
		if hr, _, _ := procResizePseudoConsole.Call(hpc, coord(rows, cols)); hr != 0 {
			return fmt.Errorf("ResizePseudoConsole: 0x%x", uint32(hr))
		}
		return nil
	}
	p.signalFn = func(sig int) error {
		switch sig {
		case protocol.TunnelSignalInterrupt:
			_, err := p.in.Write([]byte{0x03})
			return err
		case protocol.TunnelSignalTerminate, protocol.TunnelSignalKill:
			killTree(pid, process)
		}
		return nil
	}
	p.killFn = func() { killTree(pid, process) }
	p.watch(func() int64 { return waitHandle(process) }, func() {
		// The console closes once its process ended: its output ends then.
		_, _, _ = procClosePseudoConsole.Call(hpc)
	})
	return p, nil
}

// createInConsole creates the process inside the pseudo console: its pid
// and handle.
func createInConsole(hpc uintptr, path string, args []string) (int, syscall.Handle, error) {
	var size uintptr
	_, _, _ = procInitializeProcThreadAttributeList.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return 0, 0, errors.New("InitializeProcThreadAttributeList: no size")
	}
	list := make([]byte, size)
	attrs := uintptr(unsafe.Pointer(&list[0]))
	if ok, _, err := procInitializeProcThreadAttributeList.Call(attrs, 1, 0, uintptr(unsafe.Pointer(&size))); ok == 0 {
		return 0, 0, fmt.Errorf("InitializeProcThreadAttributeList: %w", err)
	}
	defer func() { _, _, _ = procDeleteProcThreadAttributeList.Call(attrs) }()
	if ok, _, err := procUpdateProcThreadAttribute.Call(attrs, 0, procThreadAttributePseudoConsole, hpc, unsafe.Sizeof(hpc), 0, 0); ok == 0 {
		return 0, 0, fmt.Errorf("UpdateProcThreadAttribute: %w", err)
	}
	var si startupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	// No std handles of the minion's: the console's alone.
	si.Flags = syscall.STARTF_USESTDHANDLES
	si.attributeList = attrs
	app, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	line, err := syscall.UTF16PtrFromString(commandLine(path, args))
	if err != nil {
		return 0, 0, err
	}
	var pi syscall.ProcessInformation
	ok, _, cerr := procCreateProcessW.Call(uintptr(unsafe.Pointer(app)), uintptr(unsafe.Pointer(line)), 0, 0, 0,
		extendedStartupInfoPresent, 0, 0, uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
	runtime.KeepAlive(list)
	if ok == 0 {
		return 0, 0, &os.PathError{Op: "CreateProcess", Path: path, Err: cerr}
	}
	_ = syscall.CloseHandle(pi.Thread)
	return int(pi.ProcessId), pi.Process, nil
}

// waitHandle waits for a process: its exit code (a uint32).
func waitHandle(h syscall.Handle) int64 {
	defer func() { _ = syscall.CloseHandle(h) }()
	if _, err := syscall.WaitForSingleObject(h, syscall.INFINITE); err != nil {
		return -1
	}
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return -1
	}
	return int64(code)
}

// killTree ends the process and what it started (taskkill /T), else the
// process alone.
func killTree(pid int, h syscall.Handle) {
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if kill.Run() != nil && h != 0 {
		_ = syscall.TerminateProcess(h, 1)
	}
}

// startPiped runs the process on pipes, no window of its own: stdin one,
// stdout and stderr the other.
func startPiped(path string, args []string) (*proc, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, outW
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	err = cmd.Start()
	_ = inR.Close()
	_ = outW.Close()
	if err != nil {
		_ = outR.Close()
		_ = inW.Close()
		return nil, err
	}
	p := newProc(outR, inW)
	pid := cmd.Process.Pid
	p.signalFn = func(sig int) error {
		if sig == protocol.TunnelSignalTerminate || sig == protocol.TunnelSignalKill {
			killTree(pid, 0)
		}
		return nil
	}
	p.killFn = func() {
		killTree(pid, 0)
		_ = cmd.Process.Kill()
	}
	p.closeWrite = inW.Close
	p.watch(func() int64 {
		_ = cmd.Wait()
		if cmd.ProcessState == nil {
			return -1
		}
		return int64(uint32(cmd.ProcessState.ExitCode()))
	}, nil)
	return p, nil
}
