//go:build !linux && !windows

package main

import (
	"errors"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// hostProcesses: BOB builds minions for Linux and Windows only.
func hostProcesses() ([]protocol.Process, error) {
	return nil, errors.New("processes: not on this OS")
}

// target is a process a kill names: none here.
type target struct {
	pid     int
	name    string
	started int64
}

func openTarget(int) (*target, error) { return nil, errors.New("kill: not on this OS") }

func (*target) end(bool) (string, error) { return "", errors.New("kill: not on this OS") }

func (*target) ended() bool { return false }

func (*target) close() {}
