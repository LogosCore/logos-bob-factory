package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// stubSource is the program compiled for linux-elf builds (stub/main.go).
//
//go:embed stub/main.go
var stubSource []byte

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

// compile builds the stub of target for platform with minion baked in, and
// returns the binary: static (no cgo), stripped, and the same for the same
// input.
func compile(ctx context.Context, target, platform string, minion []byte) ([]byte, error) {
	pair, ok := compiledTargets[target][platform]
	if !ok {
		return nil, fmt.Errorf("target %s does not compile for %s", target, platform)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil, errNoToolchain
	}
	dir, err := os.MkdirTemp("", "bob-build-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	files := map[string][]byte{
		"go.mod":      []byte("module bobminion\n\ngo 1.25\n"),
		"main.go":     stubSource,
		"minion.json": minion,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			return nil, err
		}
	}
	out := filepath.Join(dir, "minion")
	cmd := exec.CommandContext(ctx, goBin, "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", out, ".")
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
	return os.ReadFile(out)
}
