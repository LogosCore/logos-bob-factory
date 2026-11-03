//go:build !windows

package main

import "path/filepath"

// hostPath is a path an operator gave, made absolute.
func hostPath(p string) (string, error) { return filepath.Abs(p) }
