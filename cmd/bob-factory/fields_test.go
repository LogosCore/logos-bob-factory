package main

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// The minion reads its config by number (minion/fields.go): BOB's channels
// (chan<Module>), its build fields (field<Key>) and its connection
// parameters (param<Key>) by the schema's numbers, the channels' fields
// (http<Key>, tcp<Key>) by the channels' (testChannels). Each constant must
// be its field's number, and each number must have its constant. The minion
// is a module of its own, so its constants are read from source.
func TestFieldsMatchTheMinion(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../../minion/fields.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := (&types.Config{}).Check("main", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }

	want := map[string]int{}
	add := func(prefix, key string, num int) {
		if num != 0 {
			want[prefix+" "+norm(key)] = num
		}
	}
	for _, c := range buildSchema.Channels {
		add("chan", c.ModuleName, c.Num)
		for _, k := range c.Kinds {
			for _, f := range k.Fields {
				add("param", f.Key, f.Num)
			}
		}
	}
	for _, f := range buildSchema.Fields {
		add("field", f.Key, f.Num)
	}
	for _, tg := range buildSchema.Targets {
		for _, f := range tg.Fields {
			add("field", f.Key, f.Num)
		}
	}
	for _, ch := range []struct {
		module  string
		version int
	}{{"http", 3}, {"tcp", 1}} {
		s, ok := testChannels(ch.module, ch.version)
		if !ok {
			t.Fatalf("no %s schema", ch.module)
		}
		for _, k := range s.Kinds {
			for _, f := range append(append([]protocol.FieldSpec{}, k.Fields...), k.Target...) {
				add(ch.module, f.Key, f.Num)
			}
		}
	}

	got := map[string]int{}
	for _, name := range pkg.Scope().Names() {
		c, ok := pkg.Scope().Lookup(name).(*types.Const)
		if !ok {
			continue
		}
		v, ok := constant.Uint64Val(c.Val())
		if !ok {
			t.Errorf("%s is not a number", name)
			continue
		}
		for _, prefix := range []string{"chan", "field", "param", "http", "tcp"} {
			if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" && unicode.IsUpper(rune(rest[0])) {
				got[prefix+" "+norm(rest)] = int(v)
			}
		}
	}
	if !maps.Equal(got, want) {
		for _, k := range slices.Sorted(maps.Keys(want)) {
			if got[k] != want[k] {
				t.Errorf("%s: the schema's %d, the minion's %d", k, want[k], got[k])
			}
		}
		for _, k := range slices.Sorted(maps.Keys(got)) {
			if _, ok := want[k]; !ok {
				t.Errorf("%s: the minion's %d is no field's", k, got[k])
			}
		}
	}
}
