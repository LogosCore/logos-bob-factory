package main

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// The minion dispatches by number (minion/ops.go): each command's op and
// each param's num in the schema must be the constant the minion reads it
// by. The minion is a module of its own, so its constants are read from
// source: op<Command> for a command, <command><Param> for a param (upload's
// are uploadArg<Param>), in camel case.
func TestOpsMatchTheMinion(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../../minion/ops.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	conf := types.Config{}
	pkg, err := conf.Check("main", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	value := func(name string) (int64, bool) {
		c, ok := pkg.Scope().Lookup(name).(*types.Const)
		if !ok {
			return 0, false
		}
		v, ok := constant.Int64Val(c.Val())
		return v, ok
	}
	seen := map[string]bool{}
	for _, c := range buildSchema.Commands {
		name := "op" + camel(c.ID, true)
		seen[name] = true
		if v, ok := value(name); !ok || v != int64(c.Op) {
			t.Errorf("command %q: op %d, the minion's %s is %d (%v)", c.ID, c.Op, name, v, ok)
		}
		prefix := camel(c.ID, false)
		if c.ID == "upload" {
			prefix = "uploadArg"
		}
		for _, p := range c.Params {
			name := prefix + camel(p.Key, true)
			seen[name] = true
			if v, ok := value(name); !ok || v != int64(p.Num) {
				t.Errorf("command %q param %q: num %d, the minion's %s is %d (%v)", c.ID, p.Key, p.Num, name, v, ok)
			}
		}
	}
	// And nothing the minion numbers that the schema does not have.
	for _, name := range pkg.Scope().Names() {
		if !seen[name] {
			t.Errorf("the minion numbers %s, which the schema has not", name)
		}
	}
}

// camel is snake_case in camel case: "max_folders" → "MaxFolders" (upper) or
// "maxFolders"; "one_fs" → "OneFS".
func camel(s string, upper bool) string {
	var b strings.Builder
	for i, w := range strings.Split(s, "_") {
		switch {
		case w == "fs":
			w = "FS"
		case i > 0 || upper:
			w = strings.ToUpper(w[:1]) + w[1:]
		}
		b.WriteString(w)
	}
	return b.String()
}
