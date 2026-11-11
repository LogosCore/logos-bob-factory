package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// defaultMinionSrc is where BOB's image keeps the minion's sources
// (MINION_SRC overrides it).
const defaultMinionSrc = "/app/minion"

// compiledTargets are the targets BOB compiles a binary for, by platform:
// GOOS/GOARCH pairs.
var compiledTargets = map[string]map[string][2]string{
	"linux-elf": {
		"linux/amd64": {"linux", "amd64"},
		"linux/386":   {"linux", "386"},
	},
	"windows-exe": {
		"windows/amd64": {"windows", "amd64"},
		"windows/386":   {"windows", "386"},
		"windows/arm64": {"windows", "arm64"},
	},
}

// errNoToolchain means BOB has no Go toolchain to compile with.
var errNoToolchain = errors.New("no Go toolchain: the go command is not on PATH")

// compile builds the minion module in src for target and platform, with
// minion (its config) baked in as its minion.cfg, and returns the binary: static (no
// cgo), stripped, and the same for the same input. src is copied first, so
// it is never written to. Its modules come from the module cache, and what
// is not there through GOPROXY as BOB's environment sets it (a mirror such
// as Nexus on a network without the internet), checked against the
// minion's go.sum: what compiles is what go.sum names, wherever it came
// from. The command and everything the compiler writes to stdout and stderr
// go to log as they come.
func compile(ctx context.Context, src, target, platform string, minion []byte, log io.Writer) ([]byte, error) {
	pair, ok := compiledTargets[target][platform]
	if !ok {
		return nil, fmt.Errorf("target %s does not compile for %s", target, platform)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil, errNoToolchain
	}
	if _, err := os.Stat(filepath.Join(src, "go.mod")); err != nil {
		return nil, fmt.Errorf("no minion sources in %s: %w", src, err)
	}
	dir, err := os.MkdirTemp("", "bob-build-*")
	if err != nil {
		return nil, err
	}
	// A build's scratch copy: one left behind (a busy file) is only space.
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
		return nil, fmt.Errorf("copy the minion sources: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "minion.cfg"), minion, 0o600); err != nil {
		return nil, err
	}
	// readonly: go.mod and go.sum are as committed, never fixed up here.
	args := []string{"build", "-v", "-trimpath", "-mod=readonly", "-ldflags=-s -w -buildid=", "-o", filepath.Join(".bin", "minion")}
	cmd := exec.CommandContext(ctx, goBin, append(args, ".")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GOOS="+pair[0], "GOARCH="+pair[1], "CGO_ENABLED=0",
		"GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local",
	)
	// The build log's own line: a write it refuses fails the compile below.
	_, _ = fmt.Fprintf(log, "$ GOOS=%s GOARCH=%s CGO_ENABLED=0 go %s\n", pair[0], pair[1], strings.Join(cmd.Args[1:], " "))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("go build: %w", err)
	}
	return os.ReadFile(filepath.Join(dir, ".bin", "minion"))
}

// lineLog writes whole lines to a build log as they come: what a process
// prints, split at newlines, each line one Logf.
type lineLog struct {
	logf func(format string, args ...any)
	buf  []byte
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		l.logf("%s", l.buf[:i])
		l.buf = l.buf[i+1:]
	}
}

// Flush logs what is left after the last newline.
func (l *lineLog) Flush() {
	if len(l.buf) > 0 {
		l.logf("%s", l.buf)
		l.buf = nil
	}
}
