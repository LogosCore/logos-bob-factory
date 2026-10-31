//go:build !unix

package main

import "io/fs"

// owners is a file's owner and group: none where the host keeps no unix ids.
func owners(fs.FileInfo) (string, string) { return "", "" }
