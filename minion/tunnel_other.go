//go:build !unix && !windows

package main

// Why a connect failed: no host errors to tell apart here (tunnel.go,
// dialResult).

func refusedErr(error) bool         { return false }
func hostUnreachableErr(error) bool { return false }
func netUnreachableErr(error) bool  { return false }
func timedOutErr(error) bool        { return false }
