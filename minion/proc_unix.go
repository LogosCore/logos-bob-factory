//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// hostShell is the host's shell: $SHELL, else /bin/sh.
func hostShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

// terminalEnv is the minion's environment, with a TERM a terminal's
// programs read.
func terminalEnv() []string {
	env := os.Environ()
	if os.Getenv("TERM") == "" {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}

// startPiped runs the process on pipes, in a process group of its own:
// stdin one, stdout and stderr the other.
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	p.signalFn = func(sig int) error { return signalGroup(pid, sig) }
	p.killFn = func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }
	p.closeWrite = inW.Close
	p.watch(func() int64 { return waitExit(cmd) }, nil)
	return p, nil
}

// signalGroup sends a process group the host's own for sig; one the
// minion does not know it lets go.
func signalGroup(group, sig int) error {
	var s syscall.Signal
	switch sig {
	case protocol.TunnelSignalInterrupt:
		s = syscall.SIGINT
	case protocol.TunnelSignalTerminate:
		s = syscall.SIGTERM
	case protocol.TunnelSignalKill:
		s = syscall.SIGKILL
	default:
		return nil
	}
	return syscall.Kill(-group, s)
}

// waitExit waits for the process: its exit code, 128 and the signal's
// number when a signal ended it, as a shell says it.
func waitExit(cmd *exec.Cmd) int64 {
	_ = cmd.Wait()
	st := cmd.ProcessState
	if st == nil {
		return -1
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int64(ws.Signal())
	}
	return int64(st.ExitCode())
}
