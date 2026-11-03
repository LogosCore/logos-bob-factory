//go:build !linux && !windows

package main

import (
	"errors"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// hostVolumes: BOB builds minions for Linux and Windows only.
func hostVolumes() ([]protocol.Volume, error) {
	return nil, errors.New("volumes: not on this OS")
}
