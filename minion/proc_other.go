//go:build !unix && !windows

package main

import "errors"

// No processes here: an open of one fails.

func hostShell() string { return "sh" }

func startTerminal(string, []string, int, int) (*proc, error) { return nil, errNoTerminal }

func startPiped(string, []string) (*proc, error) { return nil, errors.New("no processes on this host") }
