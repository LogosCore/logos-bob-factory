package main

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/logoscore/logos-golang-factory-core/pkg/factorytest"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

func needGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command to compile with")
	}
}

// run executes bin and returns what it printed, when this machine can run it.
func run(t *testing.T, bin []byte) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "minion")
	if err := os.WriteFile(path, bin, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(path).Output()
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
		bin, err := compile(context.Background(), "linux-elf", platform, minion)
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
	if _, err := compile(context.Background(), "linux-elf", "windows/amd64", minion); err == nil {
		t.Fatal("compiled for a platform the target does not have")
	}
}

// A linux-elf build plans a compile step and uploads a binary that prints the
// dummy minion.
func TestBuildLinuxELF(t *testing.T) {
	needGo(t)
	st := factorytest.NewStore(t)
	badge := st.File("badge-1", "badge.png", "image/png", []byte("\x89PNG fake"))
	req := coreRequest(t)
	req.Target, req.Platform = "linux-elf", "linux/amd64"
	req.Artifact = protocol.ArtifactUpload{}
	req.Files = map[string]protocol.BuildFile{badge.ID: badge}
	req.Values["badge"] = badge.ID

	out := factorytest.Run(t, buildSchema, &bob{builtBy: "bob bob-test test"}, st, req)
	res := out.Result
	if res.Status != protocol.BuildSucceeded || res.Artifact.Filename != "bob-5b0c4f6e" ||
		res.Artifact.MediaType != "application/x-executable" {
		t.Fatalf("result %+v (error %+v, log %s)", res, res.Error, res.Log)
	}
	if ids := stepIDs(out.Steps); ids != "badge render compile upload" {
		t.Fatalf("plan %s", ids)
	}
	if _, err := elf.NewFile(bytes.NewReader(out.Artifact)); err != nil {
		t.Fatalf("artifact is no ELF: %v", err)
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return
	}
	var m dummyMinion
	if err := json.Unmarshal(run(t, out.Artifact), &m); err != nil {
		t.Fatal(err)
	}
	if m.BuildID != req.BuildID || m.Target != "linux-elf" || m.Platform != "linux/amd64" || m.Greeting != "hello from BOB" {
		t.Fatalf("the binary printed %+v", m)
	}
}
