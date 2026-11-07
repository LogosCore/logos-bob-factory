//go:build linux

package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

// A terminal on Linux: a PTY from /dev/ptmx (syscall alone), the process
// its session's leader with the PTY its controlling terminal. A signal goes
// to the terminal's foreground process group (a shell's running job), as
// the terminal's own keys send it.

func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

type winsize struct{ rows, cols, x, y uint16 }

func setSize(f *os.File, rows, cols int) error {
	ws := winsize{rows: uint16(rows), cols: uint16(cols)}
	return ioctl(f, syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
}

// openPTY is a PTY's two ends.
func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	if err := ioctl(m, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	var n uint32
	if err := ioctl(m, syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

func startTerminal(path string, args []string, rows, cols int) (*proc, error) {
	master, slave, err := openPTY()
	if err != nil {
		// No PTYs here (no /dev/pts in a bare container): pipes.
		return nil, errNoTerminal
	}
	defer func() { _ = slave.Close() }()
	if err := setSize(master, rows, cols); err != nil {
		_ = master.Close()
		return nil, err
	}
	cmd := exec.Command(path, args...)
	cmd.Env = terminalEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		return nil, err
	}
	p := newProc(master, master)
	pid := cmd.Process.Pid
	p.resizeFn = func(rows, cols int) error { return setSize(master, rows, cols) }
	p.signalFn = func(sig int) error {
		group := pid
		var fg int32
		if ioctl(master, syscall.TIOCGPGRP, unsafe.Pointer(&fg)) == nil && fg > 0 {
			group = int(fg)
		}
		return signalGroup(group, sig)
	}
	p.killFn = func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	p.watch(func() int64 { return waitExit(cmd) }, nil)
	return p, nil
}
