//go:build !unix && !windows

package main

import (
	"errors"
	"net"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A host with no pinger of the minion's: an open of pings is denied.
func newPinger(func(net.IP, protocol.ICMPMessage)) (pinger, error) {
	return nil, errors.New("this host gives the minion no ICMP socket")
}
