package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// exitConstants are the exit* constants of an exits.go, by name.
func exitConstants(t *testing.T, path string) map[string]int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := (&types.Config{}).Check("main", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, name := range pkg.Scope().Names() {
		if c, ok := pkg.Scope().Lookup(name).(*types.Const); ok && strings.HasPrefix(name, "exit") {
			v, _ := constant.Int64Val(c.Val())
			out[name] = int(v)
		}
	}
	return out
}

// The schema's error codes are the minion's (minion/exits.go): every exit*
// constant declared, and nothing declared that the minion has no constant
// for. The minion is a module of its own, so its constants are read from
// source.
func TestErrorCodesMatchTheMinion(t *testing.T) {
	minion := slices.Collect(maps.Values(exitConstants(t, "../../minion/exits.go")))
	var declared []int
	for _, e := range buildSchema.ErrorCodes {
		declared = append(declared, e.Code)
	}
	slices.Sort(minion)
	slices.Sort(declared)
	if !slices.Equal(minion, declared) {
		t.Errorf("the minion's exit codes %v, the schema's %v", minion, declared)
	}
}

// A minion whose config does not read exits with code 3 at once.
func TestMinionExitsWithItsCode(t *testing.T) {
	needGo(t)
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("runs a linux/amd64 minion")
	}
	bin, err := compile(context.Background(), testMinionSrc, "linux-elf", "linux/amd64", []byte("not a minion config"), nil, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "minion")
	if err := os.WriteFile(path, bin, 0o700); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	cmd := exec.Command(path)
	cmd.Stderr = logs
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("exit %v, want code 3; log:\n%s", err, logs)
	}
	if !strings.Contains(logs.String(), "failed (exit code 3): read the config:") {
		t.Errorf("log:\n%s", logs)
	}
}

// Each build's minion exits with codes of its own but for Go's 1 and 2,
// which stay fixed; what core draws for the schema is what the build bakes.
func TestEachBuildItsOwnExitCodes(t *testing.T) {
	if !buildSchema.ErrorCodesPerBuild {
		t.Fatal("BOB's minions take no exit codes per build")
	}
	for _, e := range buildSchema.ErrorCodes {
		if fixed := e.Code <= 2; e.Fixed != fixed {
			t.Errorf("code %d: fixed %v, want %v", e.Code, e.Fixed, fixed)
		}
	}
	drawn, err := protocol.DrawErrorCodes(buildSchema)
	if err != nil {
		t.Fatal(err)
	}
	exits := exitCodes(protocol.BuildRequest{ErrorCodes: drawn})
	if len(exits) != 3 || exits[3] < protocol.ErrorCodePoolMin || exits[1] != 0 || exits[2] != 0 {
		t.Fatalf("drawn %v", exits)
	}
	if exitCodes(protocol.BuildRequest{}) != nil {
		t.Error("exit codes for a build that drew none")
	}
	if got := formatExitCodes(map[int]int{5: 14, 3: 201}); got != "3→201, 5→14" {
		t.Errorf("logged %q", got)
	}
}

// A build's copy of exits.go takes its codes: each constant whose value is a
// drawn code takes the code's exit code, swapped codes never chained (4
// takes 3 while 3 takes 201), the fixed ones and the comments kept. A code
// no constant has fails the build.
func TestBakeExitCodes(t *testing.T) {
	src, err := os.ReadFile("../../minion/exits.go")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "exits.go")
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bakeExitCodes(path, map[int]int{3: 201, 4: 3, 5: 87}); err != nil {
		t.Fatal(err)
	}
	got := exitConstants(t, path)
	want := map[string]int{"exitFailed": 1, "exitCrashed": 2, "exitConfig": 201, "exitNoKeys": 3, "exitNoConnection": 87}
	if !maps.Equal(got, want) {
		t.Fatalf("baked %v, want %v", got, want)
	}
	if baked, _ := os.ReadFile(path); !strings.Contains(string(baked), "// exitConfig: the config built in does not read.") {
		t.Errorf("the comments went:\n%s", baked)
	}
	if err := bakeExitCodes(path, map[int]int{9: 50}); err == nil || !strings.Contains(err.Error(), "no exit constant is 9") {
		t.Errorf("a code no constant has: %v", err)
	}
}

// A minion built with codes of its own exits with its build's: its config
// does not read, and it exits with what 3 was drawn as.
func TestMinionExitsWithItsBuildsCode(t *testing.T) {
	needGo(t)
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("runs a linux/amd64 minion")
	}
	bin, err := compile(context.Background(), testMinionSrc, "linux-elf", "linux/amd64", []byte("not a minion config"),
		map[int]int{3: 201, 4: 87, 5: 14}, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "minion")
	if err := os.WriteFile(path, bin, 0o700); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	cmd := exec.Command(path)
	cmd.Stderr = logs
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 201 {
		t.Fatalf("exit %v, want code 201; log:\n%s", err, logs)
	}
	if !strings.Contains(logs.String(), "failed (exit code 201): read the config:") {
		t.Errorf("log:\n%s", logs)
	}
	if src, _ := os.ReadFile(filepath.Join(testMinionSrc, "exits.go")); !strings.Contains(string(src), "exitConfig = 3") {
		t.Error("the minion's sources were written to")
	}
}
