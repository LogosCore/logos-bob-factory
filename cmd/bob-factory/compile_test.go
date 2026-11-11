package main

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/pe"
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

// run executes bin with "describe" and returns the config it printed.
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

// Each platform gets an ELF for its machine, with the config baked in as it
// came.
func TestCompile(t *testing.T) {
	needGo(t)
	minion := []byte{0xa1, 0x01, 0x50, 0xde, 0xad, 0xbe, 0xef}
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
	if b, err := os.ReadFile(filepath.Join(testMinionSrc, "minion.cfg")); err != nil || len(b) != 0 {
		t.Fatalf("a build wrote to the minion sources: %q %v", b, err)
	}
}

// The command and what the compiler prints reach the log, a line at a time.
// windows-exe gives a console program for each Windows platform's machine,
// the description baked in all the same.
func TestCompileWindows(t *testing.T) {
	needGo(t)
	minion := []byte(`{"format":"logos.bob-dummy.v1","greeting":"hello"}` + "\n")
	for platform, machine := range map[string]uint16{
		"windows/amd64": pe.IMAGE_FILE_MACHINE_AMD64, "windows/386": pe.IMAGE_FILE_MACHINE_I386, "windows/arm64": pe.IMAGE_FILE_MACHINE_ARM64,
	} {
		bin, err := compile(context.Background(), testMinionSrc, "windows-exe", platform, minion, io.Discard)
		if err != nil {
			t.Fatalf("%s: %v", platform, err)
		}
		f, err := pe.NewFile(bytes.NewReader(bin))
		if err != nil || f.Machine != machine {
			t.Fatalf("%s: pe %v, %v", platform, f, err)
		}
		var subsystem uint16
		switch h := f.OptionalHeader.(type) {
		case *pe.OptionalHeader64:
			subsystem = h.Subsystem
		case *pe.OptionalHeader32:
			subsystem = h.Subsystem
		}
		if subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_CUI {
			t.Fatalf("%s: subsystem %d, not a console program", platform, subsystem)
		}
		if !bytes.Contains(bin, minion) {
			t.Fatalf("%s: the description is not baked in", platform)
		}
	}
	if _, err := compile(context.Background(), testMinionSrc, "windows-exe", "linux/amd64", minion, io.Discard); err == nil {
		t.Fatal("windows-exe compiled for linux/amd64")
	}
}

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
