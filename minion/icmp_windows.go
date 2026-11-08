package main

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"syscall"
	"unsafe"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A Windows host's pinger: IcmpSendEcho, which any user may call, one echo
// at a time each (at most pingsAtOnce together); what it answers is an
// echo reply, or the status of an error a router sent (unreachable, the
// TTL expired), from the address that sent it. IPv4 alone: an echo to an
// IPv6 address goes nowhere.

var (
	iphlpapi            = syscall.NewLazyDLL("iphlpapi.dll")
	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho    = iphlpapi.NewProc("IcmpSendEcho")
)

const (
	pingsAtOnce = 64
	pingWaitMs  = 4000
	// IcmpSendEcho's statuses (IP_*): success, then the errors that are an
	// ICMP message.
	ipSuccess         = 0
	ipNetUnreachable  = 11002
	ipHostUnreachable = 11003
	ipProtUnreachable = 11004
	ipPortUnreachable = 11005
	ipTTLExpired      = 11013
)

type winPinger struct {
	deliver func(net.IP, protocol.ICMPMessage)
	mu      sync.RWMutex
	h       uintptr
	closed  bool
	slots   chan struct{}
}

func newPinger(deliver func(net.IP, protocol.ICMPMessage)) (pinger, error) {
	h, _, err := procIcmpCreateFile.Call()
	if h == 0 || h == ^uintptr(0) {
		return nil, err
	}
	return &winPinger{deliver: deliver, h: h, slots: make(chan struct{}, pingsAtOnce)}, nil
}

func (p *winPinger) echo(dst net.IP, seq uint16, payload []byte) error {
	v4 := dst.To4()
	if v4 == nil {
		return errors.New("IPv6 pings are not sent from Windows")
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return errors.New("too many pings at once")
	}
	go func() {
		defer func() { <-p.slots }()
		p.send(v4, seq, payload)
	}()
	return nil
}

func (p *winPinger) send(dst net.IP, seq uint16, payload []byte) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return
	}
	// ICMP_ECHO_REPLY (Address, Status, RoundTripTime, DataSize, Reserved,
	// Data, Options), the payload echoed, and room past it.
	buf := make([]byte, 64+len(payload)+8)
	var data uintptr
	if len(payload) > 0 {
		data = uintptr(unsafe.Pointer(&payload[0]))
	}
	addr := binary.LittleEndian.Uint32(dst)
	n, _, _ := procIcmpSendEcho.Call(p.h, uintptr(addr), data, uintptr(len(payload)), 0, uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)), pingWaitMs)
	if n == 0 {
		// Timed out, or nothing to say.
		return
	}
	from := net.IP(append([]byte(nil), buf[0:4]...))
	status := binary.LittleEndian.Uint32(buf[4:])
	quoted := &protocol.ICMPMessage{Type: protocol.ICMPEchoRequest, Seq: seq}
	switch status {
	case ipSuccess:
		size := int(binary.LittleEndian.Uint16(buf[12:]))
		ptr := *(*uintptr)(unsafe.Pointer(&buf[16]))
		start := int(ptr - uintptr(unsafe.Pointer(&buf[0])))
		var echoed []byte
		if size > 0 && start >= 0 && start+size <= len(buf) {
			echoed = append([]byte(nil), buf[start:start+size]...)
		}
		p.deliver(from, protocol.ICMPMessage{Type: protocol.ICMPEchoReply, Seq: seq, Payload: echoed})
	case ipNetUnreachable, ipHostUnreachable, ipProtUnreachable, ipPortUnreachable:
		code := map[uint32]int{ipNetUnreachable: protocol.ICMPNetUnreachable, ipHostUnreachable: protocol.ICMPHostUnreachable,
			ipProtUnreachable: 2, ipPortUnreachable: protocol.ICMPPortUnreachable}[status]
		p.deliver(from, protocol.ICMPMessage{Type: protocol.ICMPDestUnreachable, Code: code, Quoted: quoted})
	case ipTTLExpired:
		p.deliver(from, protocol.ICMPMessage{Type: protocol.ICMPTimeExceeded, Quoted: quoted})
	}
}

func (p *winPinger) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		_, _, _ = procIcmpCloseHandle.Call(p.h)
	}
}
