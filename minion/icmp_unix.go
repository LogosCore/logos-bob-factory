//go:build unix

package main

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A Unix host's pinger: an unprivileged ICMP socket (Linux's ping sockets,
// as net.ipv4.ping_group_range lets the minion's group; macOS's), the
// kernel's the identifier and the replies' filter, errors not delivered;
// else a raw one, when the minion runs as root, which sees every ICMP
// message of the host: the minion's identifier picks its own, errors
// among them. IPv6's opens as its first echo goes.
type unixPinger struct {
	deliver func(net.IP, protocol.ICMPMessage)
	id      uint16
	mu      sync.Mutex
	v4, v6  *pingConn
	closed  bool
}

type pingConn struct {
	pc  net.PacketConn
	raw bool
	v6  bool
}

func newPinger(deliver func(net.IP, protocol.ICMPMessage)) (pinger, error) {
	var b [2]byte
	_, _ = rand.Read(b[:])
	p := &unixPinger{deliver: deliver, id: binary.BigEndian.Uint16(b[:])}
	c, err := listenICMP(false)
	if err != nil {
		return nil, err
	}
	p.v4 = c
	go p.read(c)
	return p, nil
}

// listenICMP opens an ICMP socket: an unprivileged one, else a raw one.
func listenICMP(v6 bool) (*pingConn, error) {
	family, proto, network := syscall.AF_INET, syscall.IPPROTO_ICMP, "ip4:icmp"
	if v6 {
		family, proto, network = syscall.AF_INET6, syscall.IPPROTO_ICMPV6, "ip6:ipv6-icmp"
	}
	fd, err := syscall.Socket(family, syscall.SOCK_DGRAM, proto)
	if err == nil {
		syscall.CloseOnExec(fd)
		f := os.NewFile(uintptr(fd), "icmp")
		pc, ferr := net.FilePacketConn(f)
		_ = f.Close()
		if ferr == nil {
			return &pingConn{pc: pc, v6: v6}, nil
		}
		err = ferr
	}
	raw, rerr := net.ListenPacket(network, "")
	if rerr == nil {
		return &pingConn{pc: raw, raw: true, v6: v6}, nil
	}
	return nil, fmt.Errorf("no ICMP socket: unprivileged: %v; raw: %v", err, rerr)
}

func (p *unixPinger) conn(v6 bool) (*pingConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, net.ErrClosed
	}
	if !v6 {
		return p.v4, nil
	}
	if p.v6 == nil {
		c, err := listenICMP(true)
		if err != nil {
			return nil, err
		}
		p.v6 = c
		go p.read(c)
	}
	return p.v6, nil
}

func (p *unixPinger) echo(dst net.IP, seq uint16, payload []byte) error {
	v6 := dst.To4() == nil
	c, err := p.conn(v6)
	if err != nil {
		return err
	}
	req, _ := protocol.ICMPEchoTypes(v6)
	msg := protocol.ICMPMessage{Type: req, ID: p.id, Seq: seq, Payload: payload}.Bytes(v6)
	var to net.Addr = &net.UDPAddr{IP: dst}
	if c.raw {
		to = &net.IPAddr{IP: dst}
	}
	_, err = c.pc.WriteTo(msg, to)
	return err
}

// read hands back what answers the minion's echoes, until the socket
// closes.
func (p *unixPinger) read(c *pingConn) {
	buf := make([]byte, 64<<10)
	for {
		n, from, err := c.pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		b := buf[:n]
		if !c.v6 && len(b) >= 20 && b[0]>>4 == 4 {
			// A socket that hands the IP header too (macOS's).
			b = b[int(b[0]&0x0f)*4:]
		}
		m, ok := answer(b, c.v6)
		if !ok {
			continue
		}
		if c.raw {
			id := m.ID
			if m.Quoted != nil {
				id = m.Quoted.ID
			}
			if id != p.id {
				// Another's echo, on a socket that sees them all.
				continue
			}
		}
		var ip net.IP
		switch a := from.(type) {
		case *net.UDPAddr:
			ip = a.IP
		case *net.IPAddr:
			ip = a.IP
		default:
			continue
		}
		p.deliver(ip, m)
	}
}

func (p *unixPinger) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, c := range []*pingConn{p.v4, p.v6} {
		if c != nil {
			_ = c.pc.Close()
		}
	}
}
