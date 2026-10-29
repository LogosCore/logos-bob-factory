package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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
}

// errNoToolchain means BOB has no Go toolchain to compile with.
var errNoToolchain = errors.New("no Go toolchain: the go command is not on PATH")

// compile builds the minion module in src for target and platform, with
// minion baked in as its minion.json, and returns the binary: static (no
// cgo), stripped, and the same for the same input. src is copied first, so
// it is never written to. A vendor directory in it is used; nothing is
// downloaded either way.
func compile(ctx context.Context, src, target, platform string, minion []byte) ([]byte, error) {
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
	defer os.RemoveAll(dir)
	if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
		return nil, fmt.Errorf("copy the minion sources: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "minion.json"), minion, 0o600); err != nil {
		return nil, err
	}
	args := []string{"build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", filepath.Join(dir, ".bin", "minion")}
	if _, err := fs.Stat(os.DirFS(dir), "vendor"); err == nil {
		args = append(args, "-mod=vendor")
	}
	cmd := exec.CommandContext(ctx, goBin, append(args, ".")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GOOS="+pair[0], "GOARCH="+pair[1], "CGO_ENABLED=0",
		"GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off",
	)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("go build: %w: %s", err, strings.TrimSpace(output.String()))
	}
	return os.ReadFile(filepath.Join(dir, ".bin", "minion"))
}
