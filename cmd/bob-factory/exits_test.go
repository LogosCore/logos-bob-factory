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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The schema's error codes are the minion's (minion/exits.go): every exit*
// constant declared, and nothing declared that the minion has no constant
// for. The minion is a module of its own, so its constants are read from
// source.
func TestErrorCodesMatchTheMinion(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../../minion/exits.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := (&types.Config{}).Check("main", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var minion []int
	for _, name := range pkg.Scope().Names() {
		if c, ok := pkg.Scope().Lookup(name).(*types.Const); ok && strings.HasPrefix(name, "exit") {
			v, _ := constant.Int64Val(c.Val())
			minion = append(minion, int(v))
		}
	}
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
	bin, err := compile(context.Background(), testMinionSrc, "linux-elf", "linux/amd64", []byte("not a minion config"), io.Discard)
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
