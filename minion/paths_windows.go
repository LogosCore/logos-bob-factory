//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"syscall"
)

// hostPath is a path an operator gave, as the host writes it: absolute (/
// read as \); a bare drive (C:) its root, not Windows' current folder on it
// (rm C: would otherwise remove that folder); and each existing part in the
// case the file system gives it (c:\users is C:\Users), so core's file
// browser shows a listing as the host spells it.
func hostPath(p string) (string, error) {
	if bareDrive(p) {
		p += `\`
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return trueCase(abs), nil
}

// trueCase is abs with each part that exists spelled as its folder holds it
// (FindFirstFile's name); the parts from the first that does not are kept
// as given. A drive letter is upper case.
func trueCase(abs string) string {
	volume := filepath.VolumeName(abs)
	if volume == "" {
		return abs
	}
	if len(volume) == 2 && volume[1] == ':' {
		volume = strings.ToUpper(volume)
	}
	at, parts := splitAfterVolume(abs, filepath.VolumeName(abs))
	at = volume + at[len(volume):]
	for i, part := range parts {
		next := filepath.Join(at, part)
		name, ok := foundName(next)
		if !ok {
			return filepath.Join(append([]string{at}, parts[i:]...)...)
		}
		at = filepath.Join(at, name)
	}
	return at
}

// foundName is the name of the file at path as its folder spells it.
func foundName(path string) (string, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil || strings.ContainsAny(path[len(filepath.VolumeName(path)):], "*?") {
		return "", false
	}
	var fd syscall.Win32finddata
	h, err := syscall.FindFirstFile(p, &fd)
	if err != nil {
		return "", false
	}
	_ = syscall.FindClose(h)
	return syscall.UTF16ToString(fd.FileName[:]), true
}
