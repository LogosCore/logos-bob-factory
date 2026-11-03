//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// On Windows: a path typed in any case, or with /, comes back as the file
// system spells it; a bare drive is its root; a part that does not exist
// yet is kept as typed.
func TestHostPathOnWindows(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "MixedCase", "Inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MixedCase", "Inner", "ReadMe.TXT"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	real, err := hostPath(filepath.Join(dir, "MixedCase", "Inner", "ReadMe.TXT"))
	if err != nil {
		t.Fatal(err)
	}
	typed := strings.ReplaceAll(strings.ToLower(filepath.Join(dir, "mixedcase", "inner", "readme.txt")), `\`, "/")
	got, err := hostPath(typed)
	if err != nil || got != real || !strings.HasSuffix(got, `\MixedCase\Inner\ReadMe.TXT`) {
		t.Fatalf("hostPath(%q) = %q %v, want %q", typed, got, err, real)
	}
	if got, _ := hostPath(strings.ToLower(filepath.Join(dir, "mixedcase", "new folder"))); !strings.HasSuffix(got, `\MixedCase\new folder`) {
		t.Fatalf("a part not there yet: %q", got)
	}
	drive := filepath.VolumeName(dir)
	if got, _ := hostPath(strings.ToLower(drive)); got != strings.ToUpper(drive)+`\` {
		t.Fatalf("hostPath(%q) = %q, want the drive's root", strings.ToLower(drive), got)
	}
	// / (or \) is the root of the drive the minion's current folder is on.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", `\`} {
		if got, _ := hostPath(p); got != strings.ToUpper(filepath.VolumeName(wd))+`\` {
			t.Fatalf("hostPath(%q) = %q, want the root of %s", p, got, wd)
		}
	}
}

// A junction lists as a link to its target, and a walk does not enter it.
func TestJunctionIsALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(dir, "jn"), target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "jn" {
			continue
		}
		d := dirEntry(dir, e)
		if d.Type != protocol.EntryLink || !strings.EqualFold(filepath.Clean(d.Target), target) {
			t.Fatalf("junction: type %d target %q", d.Type, d.Target)
		}
		if e.IsDir() {
			t.Fatal("a junction reads as a folder: a walk would enter it")
		}
		return
	}
	t.Fatal("no junction listed")
}
