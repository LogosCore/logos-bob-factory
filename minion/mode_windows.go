//go:build windows

package main

import (
	"io/fs"
	"syscall"
)

// modeBits is a file's attributes, as a listing sends them
// (protocol.LsEntry.Mode).
func modeBits(info fs.FileInfo) uint32 {
	if a, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return a.FileAttributes
	}
	return 0
}
