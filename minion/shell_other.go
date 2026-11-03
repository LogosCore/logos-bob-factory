//go:build !unix && !windows

package main

import (
	"context"
	"os/exec"
)

// shellCmd is command in a shell, where the minion knows no other.
func shellCmd(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}
