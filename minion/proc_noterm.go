//go:build unix && !linux

package main

// startTerminal: no PTYs here (BOB builds Linux and Windows minions); the
// process runs on pipes.
func startTerminal(string, []string, int, int) (*proc, error) { return nil, errNoTerminal }
