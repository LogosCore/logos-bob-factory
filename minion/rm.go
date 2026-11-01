package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// rm removes a file, a link (itself, never what it points to) or a folder
// (docs: contracts/task-results-and-actions.md, delete_path): a folder with
// what is in it only when recursive says so; never a filesystem's root. The
// output says what went.
func (r *runner) rm(t *task, args protocol.Args) {
	path, _ := protocol.ArgString(args, rmPath)
	recursive, _ := args[rmRecursive].(bool)
	if path == "" {
		r.end(t, false, nil, "no path")
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	if isRoot(abs) {
		r.end(t, false, nil, abs+" is a filesystem's root: not removed")
		return
	}
	info, err := os.Lstat(abs)
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	kind := "file"
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		kind = "link"
	case info.IsDir():
		kind = "folder"
		if !recursive {
			if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
				r.end(t, false, nil, fmt.Sprintf("%s is not empty (%d entries): remove it with recursive", abs, len(entries)))
				return
			}
		}
	case !info.Mode().IsRegular():
		kind = "other"
	}
	if info.IsDir() && recursive {
		err = os.RemoveAll(abs)
	} else {
		err = os.Remove(abs)
	}
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	r.write(t, []byte(fmt.Sprintf("removed %s (%s)\n", abs, kind)), nil)
	r.end(t, true, nil, "")
}

// isRoot: path is a filesystem's root: / on Unix, a drive's (C:\) or a
// share's on Windows.
func isRoot(abs string) bool {
	return filepath.Dir(abs) == abs
}
