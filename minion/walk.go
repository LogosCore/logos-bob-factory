package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// walkBacklog is how many listing pieces a walk lets wait to be sent before
// it reads on: it goes as fast as the channel takes them, and no faster.
const walkBacklog = 16

// walkReport is how often a walk says where it stands (its data).
const walkReport = 2 * time.Second

// walk lists a directory and the folders under it, breadth first (the top
// levels first), each a listing as it is read (docs:
// contracts/minion-messages.md, Listings), and says where it stands as a
// dir_walk: the folders and entries so far, those not read, those not
// entered (deeper than depth, past max_folders, another file system,
// hidden when not wanted). Links are not followed. A depth or max_folders
// of 0 is no limit.
func (r *runner) walk(ctx context.Context, t *task, args protocol.Args) {
	path, _ := protocol.ArgString(args, walkPath)
	if path == "" {
		path = "."
	}
	depth, _ := protocol.ArgInt(args, walkDepth)
	// 0, or none: no limit.
	most, _ := protocol.ArgInt(args, walkMaxFolders)
	hidden := argBool(args, walkHidden, true)
	oneFS := argBool(args, walkOneFS, true)

	root, err := filepath.Abs(path)
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	info, err := os.Stat(root)
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	if !info.IsDir() {
		r.end(t, false, nil, root+" is not a directory")
		return
	}
	rootDev, haveDev := device(info)
	sep := string(filepath.Separator)

	type folder struct {
		path  string
		level int64
	}
	queue := []folder{{root, 0}}
	w := protocol.DirWalk{Root: root}
	reported := time.Now()
	for len(queue) > 0 {
		if !r.waitBacklog(ctx, t) {
			w.Current = ""
			r.write(t, nil, w)
			r.end(t, false, nil, "cancelled")
			return
		}
		if most > 0 && int64(w.Folders+w.Unread) >= most {
			w.Skipped += uint64(len(queue))
			break
		}
		f := queue[0]
		queue = queue[1:]
		w.Current = f.path
		parent := filepath.Dir(f.path)
		if parent == f.path {
			parent = "" // a root
		}
		dir, err := os.ReadDir(f.path)
		if err != nil && len(dir) == 0 {
			// Gone since its folder was read is no listing; one it may
			// not read says so.
			w.Unread++
			if errors.Is(err, fs.ErrPermission) {
				r.list(t, f.path, sep, parent, nil, false, protocol.ListingErrDenied)
			}
			continue
		}
		entries := make([]entry, 0, len(dir))
		for _, e := range dir {
			entries = append(entries, dirEntry(f.path, e))
			// A link to a folder is a link: not followed.
			if !e.IsDir() {
				continue
			}
			switch {
			case !hidden && strings.HasPrefix(e.Name(), "."),
				depth > 0 && f.level+1 > depth:
				w.Skipped++
				continue
			}
			if oneFS && haveDev {
				if fi, err := e.Info(); err == nil {
					if d, ok := device(fi); ok && d != rootDev {
						w.Skipped++
						continue
					}
				}
			}
			queue = append(queue, folder{filepath.Join(f.path, e.Name()), f.level + 1})
		}
		r.list(t, f.path, sep, parent, entries, err != nil, 0)
		w.Folders++
		w.Entries += uint64(len(entries))
		if time.Since(reported) >= walkReport {
			r.write(t, nil, w)
			reported = time.Now()
		}
	}
	w.Current = ""
	r.write(t, nil, w)
	r.end(t, true, nil, "")
}

// waitBacklog waits while the task has walkBacklog pieces or more waiting
// to be sent; false when the task is cancelled.
func (r *runner) waitBacklog(ctx context.Context, t *task) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		r.mu.Lock()
		waiting := len(t.pieces)
		r.mu.Unlock()
		if waiting < walkBacklog {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// argBool is a bool arg, or def when the task has none.
func argBool(args protocol.Args, key uint64, def bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return def
}
