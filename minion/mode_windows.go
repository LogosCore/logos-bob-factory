//go:build windows

package main

import (
	"io/fs"
	"syscall"
)

// hiddenFile reports whether a file has Windows' hidden attribute
// (FILE_ATTRIBUTE_HIDDEN).
func hiddenFile(info fs.FileInfo) bool {
	a, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && a.FileAttributes&syscall.FILE_ATTRIBUTE_HIDDEN != 0
}

// modeBits is a file's attributes, as a listing sends them
// (protocol.LsEntry.Mode).
func modeBits(info fs.FileInfo) uint32 {
	if a, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return a.FileAttributes
	}
	return 0
}
