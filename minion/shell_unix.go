//go:build unix

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCmd is command in the system shell, /bin/sh -c, in a process group of
// its own, all of it killed on a cancel or the timeout: the shell's children
// (a sleep) would otherwise live on holding the output open, and the task
// would never end.
func shellCmd(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}
