package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/logoscore/logos-golang-protocol/protocol"
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
		"linux/arm64": {"linux", "arm64"},
		"linux/arm":   {"linux", "arm"}, // the ARM version: GOARM, the build's goarm
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
// minion (its config) baked in as its minion.cfg and exits (the build's own
// exit codes, by declared code; nil: the declared ones) as its exits.go's
// constants, for 32-bit ARM the ARM version goarm (GOARM: "5", "6" or "7";
// "" is 7, Go's own default; other platforms ignore it), and returns the binary: static (no cgo), stripped, and the
// same for the same input. src is copied first, so it is never written to. Its modules come from the module cache, and what
// is not there through GOPROXY as BOB's environment sets it (a mirror such
// as Nexus on a network without the internet), checked against the
// minion's go.sum: what compiles is what go.sum names, wherever it came
// from. The command and everything the compiler writes to stdout and stderr
// go to log as they come.
func compile(ctx context.Context, src, target, platform string, minion []byte, exits map[int]int, goarm string, log io.Writer) ([]byte, error) {
	pair, ok := compiledTargets[target][platform]
	if !ok {
		return nil, fmt.Errorf("target %s does not compile for %s", target, platform)
	}
	goEnv := []string{"GOOS=" + pair[0], "GOARCH=" + pair[1]}
	if pair[1] == "arm" {
		goarm = cmp.Or(goarm, "7")
		if !slices.Contains(goarmVersions, goarm) {
			return nil, fmt.Errorf("GOARM %q: BOB builds for ARM versions %s", goarm, strings.Join(goarmVersions, ", "))
		}
		goEnv = append(goEnv, "GOARM="+goarm)
	}
	goEnv = append(goEnv, "CGO_ENABLED=0")
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
	if len(exits) > 0 {
		if err := bakeExitCodes(filepath.Join(dir, "exits.go"), exits); err != nil {
			return nil, fmt.Errorf("the build's exit codes: %w", err)
		}
	}
	// readonly: go.mod and go.sum are as committed, never fixed up here.
	args := []string{"build", "-v", "-trimpath", "-mod=readonly", "-ldflags=-s -w -buildid=", "-o", filepath.Join(".bin", "minion")}
	cmd := exec.CommandContext(ctx, goBin, append(args, ".")...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), goEnv...), "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local")
	// The build log's own line: a write it refuses fails the compile below.
	_, _ = fmt.Fprintf(log, "$ %s go %s\n", strings.Join(goEnv, " "), strings.Join(cmd.Args[1:], " "))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("go build: %w", err)
	}
	return os.ReadFile(filepath.Join(dir, ".bin", "minion"))
}

// exitCodes are the build's own exit codes, by the code BOB declares
// (minion/exits.go): those core drew for it; nil when it drew none, and the
// minion exits with the declared ones.
// goarmVersions are the ARM versions BOB builds 32-bit ARM minions for,
// its goarm field's options.
var goarmVersions = []string{"7", "6", "5"}

// goarmOf is a build's ARM version (its goarm value, which core sends
// with its default for a linux/arm build and never for another); "" when
// it has none.
func goarmOf(req protocol.BuildRequest) string {
	s, _ := req.Values["goarm"].(string)
	return s
}

func exitCodes(req protocol.BuildRequest) map[int]int {
	if len(req.ErrorCodes) == 0 {
		return nil
	}
	exits := make(map[int]int, len(req.ErrorCodes))
	for _, e := range req.ErrorCodes {
		exits[e.Code] = e.ExitCode
	}
	return exits
}

// formatExitCodes says a build's exit codes for its log: "3→87, 4→201".
func formatExitCodes(exits map[int]int) string {
	parts := make([]string, 0, len(exits))
	for _, code := range slices.Sorted(maps.Keys(exits)) {
		parts = append(parts, fmt.Sprintf("%d→%d", code, exits[code]))
	}
	return strings.Join(parts, ", ")
}

// bakeExitCodes rewrites the minion's exit codes in path, a build's copy of
// its exits.go: each exit* constant whose value is a code in exits takes
// that code's exit code. The values are swapped as they stood, so codes that
// trade places do not chain. Every code in exits must be a constant's, or
// the minion would exit with a code core never drew.
func bakeExitCodes(path string, exits map[int]int) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return err
	}
	baked := make(map[int]bool, len(exits))
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "exit") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					return fmt.Errorf("%s: not a number", name.Name)
				}
				code, err := strconv.Atoi(lit.Value)
				if err != nil {
					return fmt.Errorf("%s: %w", name.Name, err)
				}
				if exit, ok := exits[code]; ok {
					lit.Value = strconv.Itoa(exit)
					baked[code] = true
				}
			}
		}
	}
	for code := range exits {
		if !baked[code] {
			return fmt.Errorf("no exit constant is %d", code)
		}
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
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
