package main

import "strings"

// Paths operators give the minion (ls, walk, rm, download, upload) are
// read the host's way: hostPath (paths_windows.go, paths_other.go).

// bareDrive reports whether p is a Windows drive alone, "C:": to Windows
// the current folder on that drive, where an operator means its root.
func bareDrive(p string) bool {
	return len(p) == 2 && p[1] == ':' && ('a' <= p[0] && p[0] <= 'z' || 'A' <= p[0] && p[0] <= 'Z')
}

// splitAfterVolume is an absolute Windows path's volume root (C:\,
// \\server\share\) and its parts after it.
func splitAfterVolume(abs, volume string) (string, []string) {
	root := volume + `\`
	var parts []string
	for _, s := range strings.Split(strings.TrimPrefix(abs[len(volume):], `\`), `\`) {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return root, parts
}
