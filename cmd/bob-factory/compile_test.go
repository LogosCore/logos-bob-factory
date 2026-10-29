package main

import (
	"bytes"
	"context"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testMinionSrc is the repository's minion module: the sources the image
// ships.
const testMinionSrc = "../../minion"

func needGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command to compile with")
	}
}

// run executes bin with "describe" and returns the description it printed.
func run(t *testing.T, bin []byte) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "minion")
	if err := os.WriteFile(path, bin, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(path, "describe").Output()
	if err != nil {
		t.Fatalf("run the minion: %v", err)
	}
	return out
}

// Each platform gets an ELF for its machine, with the description baked in.
func TestCompile(t *testing.T) {
	needGo(t)
	minion := []byte(`{"format":"logos.bob-dummy.v1","greeting":"hello"}` + "\n")
	for platform, machine := range map[string]elf.Machine{"linux/amd64": elf.EM_X86_64, "linux/386": elf.EM_386} {
		bin, err := compile(context.Background(), testMinionSrc, "linux-elf", platform, minion, io.Discard)
		if err != nil {
			t.Fatalf("%s: %v", platform, err)
		}
		f, err := elf.NewFile(bytes.NewReader(bin))
		if err != nil || f.Machine != machine || f.Type != elf.ET_EXEC {
			t.Fatalf("%s: elf %v, %v", platform, f, err)
		}
		if platform == "linux/amd64" && runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
			if out := run(t, bin); !bytes.Equal(out, minion) {
				t.Fatalf("printed %q, want %q", out, minion)
			}
		}
	}
	if _, err := compile(context.Background(), testMinionSrc, "linux-elf", "windows/amd64", minion, io.Discard); err == nil {
		t.Fatal("compiled for a platform the target does not have")
	}
	if _, err := compile(context.Background(), t.TempDir(), "linux-elf", "linux/amd64", minion, io.Discard); err == nil {
		t.Fatal("compiled without minion sources")
	}
	if b, _ := os.ReadFile(filepath.Join(testMinionSrc, "minion.json")); string(b) != "{}\n" {
		t.Fatalf("a build wrote to the minion sources: %q", b)
	}
}

// The command and what the compiler prints reach the log, a line at a time.
func TestCompileLogs(t *testing.T) {
	needGo(t)
	var lines []string
	l := &lineLog{logf: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }}
	if _, err := compile(context.Background(), testMinionSrc, "linux-elf", "linux/386", []byte("{}"), l); err != nil {
		t.Fatal(err)
	}
	l.Flush()
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "$ GOOS=linux GOARCH=386 CGO_ENABLED=0 go build -v ") {
		t.Fatalf("log %q", lines)
	}

	// A minion that does not compile: the compiler's errors are in the log.
	src := t.TempDir()
	if err := os.CopyFS(src, os.DirFS(testMinionSrc)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "broken.go"), []byte("package main\nfunc broken() { undefined() }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines = nil
	if _, err := compile(context.Background(), src, "linux-elf", "linux/amd64", []byte("{}"), l); err == nil {
		t.Fatal("a broken minion compiled")
	}
	l.Flush()
	if !strings.Contains(strings.Join(lines, "\n"), "undefined: undefined") {
		t.Fatalf("log %q", lines)
	}
}
