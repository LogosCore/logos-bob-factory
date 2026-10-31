package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

func newTestRunner() *runner {
	s := &schedule{interval: time.Second}
	return newRunner(description{Commands: []string{"shell", "ls", "hello"}, Greeting: "hi"}, s)
}

// waitEnd waits for a task to end, and returns it.
func waitEnd(t *testing.T, r *runner, within time.Duration) *task {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		if len(r.tasks) == 1 && r.tasks[0].ended {
			task := r.tasks[0]
			r.mu.Unlock()
			return task
		}
		r.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no end within %s", within)
	return nil
}

// A cancel stops a shell command and what it started: its children held
// the output open, so the task never ended.
func TestShellCancelKillsTheGroup(t *testing.T) {
	r := newTestRunner()
	id := make([]byte, 16)
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: id, Cmd: "shell", Args: map[string]any{"command": "echo started; sleep 30; echo never"}})
	time.Sleep(300 * time.Millisecond)
	r.stop(id, 0)
	task := waitEnd(t, r, 5*time.Second)
	if task.ok || task.err != "cancelled" || !strings.Contains(string(task.out), "started") || strings.Contains(string(task.out), "never") {
		t.Fatalf("ok %v err %q out %q", task.ok, task.err, task.out)
	}
}

// The timeout stops it the same way.
func TestShellTimeout(t *testing.T) {
	r := newTestRunner()
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: make([]byte, 16), Cmd: "shell",
		Args: map[string]any{"command": "sleep 30", "timeout": uint64(300)}})
	task := waitEnd(t, r, 5*time.Second)
	if task.ok || !strings.Contains(task.err, "timeout") {
		t.Fatalf("ok %v err %q", task.ok, task.err)
	}
}

// A task id seen before is acked again but not run again; a cancel for a
// task the minion does not have gets unknown_task.
func TestTaskOnceAndUnknownCancel(t *testing.T) {
	r := newTestRunner()
	id := make([]byte, 16)
	hello := protocol.TaskItem{T: protocol.ItemTask, Task: id, Cmd: "hello"}
	r.start(hello)
	waitEnd(t, r, 2*time.Second)
	r.start(hello)
	r.mu.Lock()
	tasks, acks := len(r.tasks), len(r.acks)
	r.mu.Unlock()
	if tasks != 1 || acks != 2 {
		t.Fatalf("tasks %d acks %d", tasks, acks)
	}
	other := make([]byte, 16)
	other[0] = 9
	r.stop(other, 3)
	if len(r.errs) != 1 || r.errs[0].Code != protocol.ErrItemUnknownTask || *r.errs[0].Item != 3 {
		t.Fatalf("errs %+v", r.errs)
	}
}

// An entry as a dir_listing has it: a link as the link with its target, a
// dot name hidden, an empty file with its size of 0.
func TestDirEntry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	got := map[string]protocol.DirEntry{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		got[e.Name()] = dirEntry(dir, e)
	}
	if e := got[".empty"]; e.Type != protocol.DirEntryFile || !e.Hidden || e.Size == nil || *e.Size != 0 || e.Modified == nil || e.Owner == "" {
		t.Errorf(".empty: %+v", e)
	}
	if e := got["sub"]; e.Type != protocol.DirEntryDir || e.Size != nil || e.Hidden {
		t.Errorf("sub: %+v", e)
	}
	if e := got["link"]; e.Type != protocol.DirEntryLink || e.Target != "sub" {
		t.Errorf("link: %+v", e)
	}
}
