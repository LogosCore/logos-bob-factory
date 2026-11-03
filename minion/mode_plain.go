//go:build !unix && !windows

package main

import "io/fs"

// hiddenFile: hidden by its name alone, here.
func hiddenFile(fs.FileInfo) bool { return false }

// modeBits is a file's mode bits: none the protocol reads, here.
func modeBits(fs.FileInfo) uint32 { return 0 }
