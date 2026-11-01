//go:build !unix && !windows

package main

import "io/fs"

// modeBits is a file's mode bits: none the protocol reads, here.
func modeBits(fs.FileInfo) uint32 { return 0 }
