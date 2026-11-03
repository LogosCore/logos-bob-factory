//go:build !windows

package main

import (
	"os"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// integrityLevel is the hello's integrity: root or user.
func integrityLevel() int {
	if os.Geteuid() == 0 {
		return protocol.IntegrityRoot
	}
	return protocol.IntegrityUser
}

// hostDomain is the hello's domain: a Windows host's only.
func hostDomain() string { return "" }
