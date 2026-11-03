package main

import (
	"slices"
	"testing"
)

func TestBareDrive(t *testing.T) {
	for p, want := range map[string]bool{"C:": true, "z:": true, `C:\`: false, "C": false, "1:": false, "/": false, "CC:": false} {
		if bareDrive(p) != want {
			t.Errorf("bareDrive(%q) = %v", p, !want)
		}
	}
}

func TestSplitAfterVolume(t *testing.T) {
	for _, c := range []struct {
		abs, volume, root string
		parts             []string
	}{
		{`C:\Users\Ann`, "C:", `C:\`, []string{"Users", "Ann"}},
		{`C:\`, "C:", `C:\`, nil},
		{`\\srv\share\docs\x.txt`, `\\srv\share`, `\\srv\share\`, []string{"docs", "x.txt"}},
	} {
		root, parts := splitAfterVolume(c.abs, c.volume)
		if root != c.root || !slices.Equal(parts, c.parts) {
			t.Errorf("%q: %q %q", c.abs, root, parts)
		}
	}
}
