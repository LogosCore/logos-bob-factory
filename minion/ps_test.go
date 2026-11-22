package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// hasProcesses: the host lists and kills processes (BOB's targets do).
func hasProcesses(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && !onWindows {
		t.Skip("processes: Linux and Windows")
	}
}

// runOp runs one task of op on a runner built with it, and gives it ended.
func runOp(t *testing.T, op uint64, args protocol.Args) (*runner, *task) {
	t.Helper()
	r := newRunner(description{Ops: []uint64{op}}, &schedule{interval: time.Second})
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: bytes.Repeat([]byte{byte(op)}, 16), Cmd: op, Args: args})
	return r, waitEnd(t, r, 10*time.Second)
}

// listed is a ps task's processes, its parts read as core reads them: each
// named, then checked.
func listed(t *testing.T, task *task) []protocol.Process {
	t.Helper()
	var out []protocol.Process
	for i, chunk := range task.chunks {
		named, err := protocol.NamedResultData(protocol.ResultProcessList, chunk)
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		l, err := protocol.ReadProcessList(uint64(i), named)
		if err != nil {
			t.Fatalf("part %d does not fit process_list: %v", i, err)
		}
		if l.Sep != string(filepath.Separator) {
			t.Fatalf("part %d: sep %q", i, l.Sep)
		}
		out = append(out, l.Processes...)
	}
	return out
}

// sleeper is a process that runs 30 seconds unless ended, reaped when it
// ends (done says so), killed at the test's end.
func sleeper(t *testing.T, name string, args ...string) (int, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd.Process.Pid, done
}

func sleep30Process(t *testing.T) (int, <-chan struct{}) {
	t.Helper()
	if onWindows {
		return sleeper(t, "ping", "-n", "30", "127.0.0.1")
	}
	return sleeper(t, "sleep", "30")
}

// startedOf is when the process with pid started, as a kill reads it.
func startedOf(t *testing.T, pid int) int64 {
	t.Helper()
	p, err := openTarget(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if p.started == 0 {
		t.Fatalf("pid %d: no start time", pid)
	}
	return p.started
}

// The host's processes as core reads them, this test's own among them: its
// parent, name, executable, command line, who it runs as and at what
// integrity, when it started, its threads, memory and CPU time.
func TestPs(t *testing.T) {
	hasProcesses(t)
	_, task := runOp(t, opPs, nil)
	if !task.ok {
		t.Fatalf("ps: %s", task.err)
	}
	procs := listed(t, task)
	i := slices.IndexFunc(procs, func(p protocol.Process) bool { return p.PID == uint64(os.Getpid()) })
	if len(procs) < 2 || i < 0 {
		t.Fatalf("%d processes, this one not among them", len(procs))
	}
	me, exe := procs[i], must(os.Executable())
	same := func(a, b string) bool { return a == b || onWindows && strings.EqualFold(a, b) }
	if me.PPID == nil || *me.PPID != uint64(os.Getppid()) || !same(me.Name, filepath.Base(exe)) || !same(me.Path, exe) {
		t.Errorf("this process: ppid %v, name %q, path %q (its executable %s)", me.PPID, me.Name, me.Path, exe)
	}
	if !strings.Contains(strings.ToLower(me.Cmdline), strings.ToLower(strings.TrimSuffix(filepath.Base(exe), ".exe"))) {
		t.Errorf("command line %q", me.Cmdline)
	}
	if u, err := user.Current(); err == nil && !same(me.User, u.Username) {
		t.Errorf("user %q, the test's %q", me.User, u.Username)
	}
	if me.Integrity != protocol.IntegrityName(integrityLevel()) {
		t.Errorf("integrity %q, the hello's %q", me.Integrity, protocol.IntegrityName(integrityLevel()))
	}
	if me.Started == nil || time.Since(time.Unix(*me.Started, 0)) > time.Hour || time.Until(time.Unix(*me.Started, 0)) > 2*time.Second {
		t.Errorf("started %v", me.Started)
	}
	if me.Threads == nil || *me.Threads < 1 || me.Memory == nil || *me.Memory == 0 || me.CPUTime == nil {
		t.Errorf("threads %v, memory %v, CPU time %v", me.Threads, me.Memory, me.CPUTime)
	}
	// Its main thread's state (running, or sleeping while another reads the
	// list); Windows says none.
	if runtime.GOOS == "linux" && me.State != protocol.ProcessRunning && me.State != protocol.ProcessSleeping || onWindows && me.State != "" {
		t.Errorf("state %q", me.State)
	}
}

// Parts keep within their budget, a process at least each, in order; a
// command line past half a part is cut to fit, an ellipsis saying so.
func TestProcessParts(t *testing.T) {
	var procs []protocol.Process
	for i := range 200 {
		procs = append(procs, protocol.Process{PID: uint64(i + 1), Name: fmt.Sprintf("p%d", i), Cmdline: strings.Repeat("x", 50)})
	}
	procs[7].Cmdline = strings.Repeat("é", 3000)
	parts := processParts(procs, 2048)
	var pids []uint64
	for i, part := range parts {
		b, err := protocol.WireResultData(part)
		if err != nil || len(b) > 2048 || len(part.Processes) == 0 || part.Sep != string(filepath.Separator) {
			t.Fatalf("part %d of %d: %d bytes, %d processes, sep %q, %v", i, len(parts), len(b), len(part.Processes), part.Sep, err)
		}
		for _, p := range part.Processes {
			pids = append(pids, p.PID)
			if p.PID == 8 && (len(p.Cmdline) > 1024 || !strings.HasSuffix(p.Cmdline, "…") || !utf8.ValidString(p.Cmdline)) {
				t.Errorf("the long command line: %d bytes, %q…", len(p.Cmdline), p.Cmdline[:16])
			}
		}
	}
	if len(parts) < 5 || len(pids) != 200 || !slices.IsSorted(pids) {
		t.Fatalf("%d parts, %d processes", len(parts), len(pids))
	}
	if empty := processParts(nil, 2048); len(empty) != 1 || empty[0].Sep == "" || len(empty[0].Processes) != 0 {
		t.Fatalf("none: %+v", empty)
	}
}

// Each process passes core's checks: a name for one without, and no path
// that is not absolute for the host's separator; core takes the part.
func TestFitForCore(t *testing.T) {
	abs := "/usr/sbin/sshd"
	if onWindows {
		abs = `C:\Windows\System32\svchost.exe`
	}
	procs := []protocol.Process{{PID: 1, Path: "Registry"}, {PID: 2, Name: "sshd", Path: abs}, {PID: 3, Name: "x", Path: `\\nas\tools\x.exe`}}
	fitForCore(procs)
	if procs[0].Name != "(no name)" || procs[0].Path != "" || procs[1].Name != "sshd" || procs[1].Path != abs || procs[2].Path != "" && !onWindows {
		t.Fatalf("%+v", procs)
	}
	data, err := protocol.WireResultData(protocol.ProcessList{Sep: string(filepath.Separator), Processes: procs})
	if err != nil {
		t.Fatal(err)
	}
	if named, err := protocol.NamedResultData(protocol.ResultProcessList, data); err != nil {
		t.Fatal(err)
	} else if _, err := protocol.ReadProcessList(0, named); err != nil {
		t.Fatalf("core refuses it: %v", err)
	}
}

// A kill ends the process it names as a list said it, the list's start
// time the one it checks; not one that started at another second (a pid
// reused), nor a pid no process has, nor 0 or less.
func TestKill(t *testing.T) {
	hasProcesses(t)
	pid, done := sleep30Process(t)
	started := startedOf(t, pid)
	_, list := runOp(t, opPs, nil)
	procs := listed(t, list)
	if i := slices.IndexFunc(procs, func(p protocol.Process) bool { return p.PID == uint64(pid) }); i < 0 ||
		procs[i].Started == nil || *procs[i].Started != started {
		t.Fatalf("the list's start time is not the kill's (%d)", started)
	}

	_, task := runOp(t, opKill, protocol.Args{killPid: uint64(pid), killStarted: started + 5})
	if task.ok || !strings.Contains(task.err, "another process's now") {
		t.Fatalf("another start time: ok %v, %q", task.ok, task.err)
	}
	select {
	case <-done:
		t.Fatal("ended by a kill of another start time")
	case <-time.After(200 * time.Millisecond):
	}

	_, task = runOp(t, opKill, protocol.Args{killPid: uint64(pid), killStarted: started})
	if !task.ok || !strings.Contains(string(task.out), ": it ended") {
		t.Fatalf("kill: ok %v, %q, %q", task.ok, task.out, task.err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still running")
	}

	for _, c := range []struct {
		pid  any
		want string
	}{{uint64(0), "is no process's"}, {int64(-5), "is no process's"}, {uint64(999999999), "no process has pid"}} {
		if _, task := runOp(t, opKill, protocol.Args{killPid: c.pid}); task.ok || !strings.Contains(task.err, c.want) {
			t.Errorf("pid %v: ok %v, %q", c.pid, task.ok, task.err)
		}
	}
}

// Its own process the minion ends as exit ends it: the task ends well, and
// its end delivered is what stops the minion.
func TestKillSelf(t *testing.T) {
	hasProcesses(t)
	r, task := runOp(t, opKill, protocol.Args{killPid: uint64(os.Getpid()), killStarted: startedOf(t, os.Getpid()), killForce: true})
	if !task.ok || !strings.Contains(string(task.out), "is this minion") {
		t.Fatalf("ok %v, %q, %q", task.ok, task.out, task.err)
	}
	if !r.delivered([]any{protocol.ResultItem{T: protocol.ItemResult, Task: task.id, Done: true}}) {
		t.Fatal("its end delivered does not stop the minion")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
