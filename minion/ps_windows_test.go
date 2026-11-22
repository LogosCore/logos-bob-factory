//go:build windows

package main

import (
	"os"
	"runtime"
	"slices"
	"testing"
	"unsafe"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// SYSTEM_PROCESS_INFORMATION's fields at C's offsets, on 64 bits and on 32.
func TestSystemProcessInfoLayout(t *testing.T) {
	var e systemProcessInfo
	bits64 := unsafe.Sizeof(uintptr(0)) == 8
	for _, f := range []struct {
		name           string
		at, at64, at32 uintptr
	}{
		{"CreateTime", unsafe.Offsetof(e.CreateTime), 0x20, 0x20},
		{"ImageName", unsafe.Offsetof(e.ImageName), 0x38, 0x38},
		{"UniqueProcessID", unsafe.Offsetof(e.UniqueProcessID), 0x50, 0x44},
		{"InheritedFromUniqueProcessID", unsafe.Offsetof(e.InheritedFromUniqueProcessID), 0x58, 0x48},
		{"SessionID", unsafe.Offsetof(e.SessionID), 0x64, 0x50},
		{"WorkingSetSize", unsafe.Offsetof(e.WorkingSetSize), 0x90, 0x68},
	} {
		want := f.at32
		if bits64 {
			want = f.at64
		}
		if f.at != want {
			t.Errorf("%s at %#x, C's %#x", f.name, f.at, want)
		}
	}
}

// The idle and System processes are listed, named as Windows names them,
// System under the idle one; every process with its session.
func TestPsSystemProcesses(t *testing.T) {
	_, task := runOp(t, opPs, nil)
	procs := listed(t, task)
	idle := slices.IndexFunc(procs, func(p protocol.Process) bool { return p.PID == 0 })
	system := slices.IndexFunc(procs, func(p protocol.Process) bool { return p.PID == 4 })
	if idle < 0 || system < 0 || procs[idle].Name != "System Idle Process" || procs[system].Name != "System" ||
		procs[system].PPID == nil || *procs[system].PPID != 0 {
		t.Fatalf("idle %d, System %d", idle, system)
	}
	paths, lines, users := 0, 0, 0
	for _, p := range procs {
		if p.Session == nil {
			t.Fatalf("%d (%s): no session", p.PID, p.Name)
		}
		paths, lines, users = paths+min(len(p.Path), 1), lines+min(len(p.Cmdline), 1), users+min(len(p.User), 1)
	}
	t.Logf("%d processes: %d with a path, %d a command line, %d a user (integrity %s)", len(procs), paths, lines, users,
		protocol.IntegrityName(integrityLevel()))
}

// A process WOW64 runs says its architecture (this test, built for 386 and
// run on 64-bit Windows); one that runs natively says none.
func TestPsWow64Arch(t *testing.T) {
	_, task := runOp(t, opPs, nil)
	procs := listed(t, task)
	want := ""
	if runtime.GOARCH == "386" && os.Getenv("PROCESSOR_ARCHITEW6432") != "" {
		want = "386"
	}
	if i := slices.IndexFunc(procs, func(p protocol.Process) bool { return p.PID == uint64(os.Getpid()) }); i < 0 || procs[i].Arch != want {
		t.Fatalf("this process's arch, want %q", want)
	}
}
