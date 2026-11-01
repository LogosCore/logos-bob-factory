package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

func newTestRunner() *runner {
	s := &schedule{interval: time.Second}
	return newRunner(description{Ops: []uint64{opShell, opLs, opHello}, Greeting: "hi"}, s)
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
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: id, Cmd: opShell, Args: protocol.Args{shellCommand: "echo started; sleep 30; echo never"}})
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
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: make([]byte, 16), Cmd: opShell,
		Args: protocol.Args{shellCommand: "sleep 30", shellTimeout: uint64(300)}})
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
	hello := protocol.TaskItem{T: protocol.ItemTask, Task: id, Cmd: opHello}
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
	got := map[string]entry{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		got[e.Name()] = dirEntry(dir, e)
	}
	if e := got[".empty"]; e.Type != protocol.EntryFile || !e.Hidden || e.Size == nil || *e.Size != 0 || e.Modified == nil || e.owner == "" ||
		protocol.RenderMode("linux", e.Mode) != "-rw-------" {
		t.Errorf(".empty: %+v", e)
	}
	if e := got["sub"]; e.Type != protocol.EntryDir || e.Size != nil || e.Hidden || protocol.RenderMode("linux", e.Mode) != "drwx------" {
		t.Errorf("sub: %+v", e)
	}
	if e := got["link"]; e.Type != protocol.EntryLink || e.Target != "sub" || protocol.RenderMode("linux", e.Mode)[0] != 'l' {
		t.Errorf("link: %+v", e)
	}
}

// drain takes every result part the runner has to send, message by message.
func drain(r *runner) []protocol.ResultItem {
	var parts []protocol.ResultItem
	for range 100 {
		items := r.outgoing()
		r.mu.Lock()
		r.inflight = nil
		r.mu.Unlock()
		got := false
		for _, it := range items {
			if p, ok := it.(protocol.ResultItem); ok {
				parts = append(parts, p)
				got = true
			}
		}
		if !got {
			break
		}
	}
	return parts
}

// ls sends its directory as a listing in pieces, no data: the first names
// it, the last says so, and every entry comes once.
func TestLsSendsAListing(t *testing.T) {
	dir := t.TempDir()
	for i := range 1200 {
		if err := os.WriteFile(filepath.Join(dir, "f"+strings.Repeat("0", 4-len(itoa(i)))+itoa(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := newTestRunner()
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: make([]byte, 16), Cmd: opLs, Args: protocol.Args{lsPath: dir}})
	waitEnd(t, r, 5*time.Second)
	parts := drain(r)
	var pieces []protocol.LsPiece
	for i, p := range parts {
		if len(p.Data) > 0 {
			t.Fatalf("part %d has data", i)
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		pieces = append(pieces, p.Ls...)
	}
	if !parts[len(parts)-1].Done || !*parts[len(parts)-1].OK {
		t.Fatalf("the last part is not a done ok: %+v", parts[len(parts)-1])
	}
	if len(pieces) != 3 || pieces[0].Path != dir || pieces[0].Sep != "/" || !pieces[2].Last || pieces[1].Last {
		t.Fatalf("pieces: %d, first %+v", len(pieces), pieces[0].Path)
	}
	n := 0
	for i, p := range pieces {
		if p.ID != 0 || p.Part != uint64(i) || p.Partial || p.Err != 0 {
			t.Fatalf("piece %d: id %d part %d", i, p.ID, p.Part)
		}
		// Every entry is the test's: one owner, named once a piece.
		if len(p.Owners) != 1 {
			t.Fatalf("piece %d: owners %v", i, p.Owners)
		}
		n += len(p.Entries)
	}
	if n != 1200 {
		t.Fatalf("%d entries", n)
	}
}

// A directory ls may not read is a listing that says so, and the task fails.
func TestLsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := filepath.Join(t.TempDir(), "closed")
	if err := os.Mkdir(dir, 0); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner()
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: make([]byte, 16), Cmd: opLs, Args: protocol.Args{lsPath: dir}})
	waitEnd(t, r, 5*time.Second)
	parts := drain(r)
	last := parts[len(parts)-1]
	if !last.Done || *last.OK {
		t.Fatalf("the task did not fail: %+v", last)
	}
	var pieces []protocol.LsPiece
	for _, p := range parts {
		pieces = append(pieces, p.Ls...)
	}
	if len(pieces) != 1 || pieces[0].Err != protocol.ListingErrDenied || !pieces[0].Last || pieces[0].Path != dir {
		t.Fatalf("pieces %+v", pieces)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// asCore is a part's data as core keeps it: named (protocol.NamedResultData).
func asCore(t *testing.T, result string, data []byte) []byte {
	t.Helper()
	named, err := protocol.NamedResultData(result, data)
	if err != nil {
		t.Fatalf("%s data: %v", result, err)
	}
	return named
}
