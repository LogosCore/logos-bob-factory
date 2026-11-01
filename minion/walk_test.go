package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// walkTree makes root/a/a1/a11, root/b, root/.h/h1, a folder it may not
// read (root/closed) and a link to a folder (root/link → a).
func walkTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"a/a1/a11", "b", ".h/h1", "closed"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a", "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "closed"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "closed"), 0o755) })
	return root
}

// runWalk walks with args, and returns its listings' folders (in the order
// sent), the folders not read, and where it stood last.
func runWalk(t *testing.T, args protocol.Args) ([]string, []string, protocol.DirWalk, bool) {
	t.Helper()
	r := newRunner(description{Ops: []uint64{opWalk}}, &schedule{interval: time.Second})
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: make([]byte, 16), Cmd: opWalk, Args: args})
	// The walk waits while its pieces are not sent: send them as it goes.
	var parts []protocol.ResultItem
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		parts = append(parts, drain(r)...)
		if n := len(parts); n > 0 && parts[n-1].Done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var folders, unread []string
	var last protocol.DirWalk
	for _, p := range parts {
		if err := p.Validate(); err != nil {
			t.Fatalf("part %d: %v", p.Seq, err)
		}
		for _, l := range p.Ls {
			if l.Part != 0 {
				continue
			}
			if l.Err != 0 {
				unread = append(unread, l.Path)
			} else {
				folders = append(folders, l.Path)
			}
		}
		if len(p.Data) > 0 {
			w, err := protocol.ReadDirWalk(p.Seq, asCore(t, protocol.ResultDirWalk, p.Data))
			if err != nil {
				t.Fatal(err)
			}
			last = *w
		}
	}
	return folders, unread, last, *parts[len(parts)-1].OK
}

func TestWalk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	root := walkTree(t)
	rel := func(paths []string) []string {
		out := make([]string, len(paths))
		for i, p := range paths {
			out[i], _ = filepath.Rel(root, p)
		}
		return out
	}

	// Two levels down, breadth first: root, its folders, then theirs; the
	// link is an entry, not a folder walked; a11 is deeper than it goes.
	folders, unread, w, ok := runWalk(t, protocol.Args{walkPath: root, walkDepth: int64(2)})
	if !ok {
		t.Fatal("the walk failed")
	}
	if got := rel(folders); !slices.Equal(got, []string{".", ".h", "a", "b", ".h/h1", "a/a1"}) {
		t.Fatalf("folders %v", got)
	}
	if got := rel(unread); !slices.Equal(got, []string{"closed"}) {
		t.Fatalf("unread %v", got)
	}
	if w.Root != root || w.Folders != 6 || w.Unread != 1 || w.Skipped != 1 || w.Current != "" || w.Entries != 9 {
		t.Fatalf("where it stood: %+v", w)
	}

	// No limits at all: every folder but the one it may not read.
	folders, _, w, _ = runWalk(t, protocol.Args{walkPath: root, walkDepth: int64(0), walkMaxFolders: int64(0)})
	if len(folders) != 7 || w.Skipped != 0 {
		t.Fatalf("no limits: %v %+v", rel(folders), w)
	}

	// Without hidden folders, and at most two folders.
	folders, _, w, _ = runWalk(t, protocol.Args{walkPath: root, walkDepth: int64(0), walkHidden: false, walkMaxFolders: int64(2)})
	if got := rel(folders); !slices.Equal(got, []string{".", "a"}) || w.Folders != 2 || w.Skipped == 0 {
		t.Fatalf("hidden off, two at most: %v %+v", got, w)
	}
}
