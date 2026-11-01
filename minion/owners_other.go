//go:build !unix

package main

import "io/fs"

// owners is a file's owner and group: none where the host keeps no unix ids.
func owners(fs.FileInfo) (string, string) { return "", "" }

// device is the file system a file is on: not known here.
func device(fs.FileInfo) (uint64, bool) { return 0, false }
