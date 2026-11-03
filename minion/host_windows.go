//go:build windows

package main

import (
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// tokenIntegrityLevel is the TokenIntegrityLevel class of
// GetTokenInformation.
const tokenIntegrityLevel = 25

// integrityLevel is the hello's integrity: the process token's mandatory
// label (low, medium, high, system); user when it does not read.
func integrityLevel() int {
	p, err := syscall.GetCurrentProcess()
	if err != nil {
		return protocol.IntegrityUser
	}
	var tok syscall.Token
	if syscall.OpenProcessToken(p, syscall.TOKEN_QUERY, &tok) != nil {
		return protocol.IntegrityUser
	}
	defer func() { _ = tok.Close() }()
	var n uint32
	_ = syscall.GetTokenInformation(tok, tokenIntegrityLevel, nil, 0, &n)
	if n == 0 {
		return protocol.IntegrityUser
	}
	buf := make([]byte, n)
	if syscall.GetTokenInformation(tok, tokenIntegrityLevel, &buf[0], n, &n) != nil {
		return protocol.IntegrityUser
	}
	// TOKEN_MANDATORY_LABEL: one SID_AND_ATTRIBUTES, its SID S-1-16-<rid>.
	label := (*syscall.SIDAndAttributes)(unsafe.Pointer(&buf[0]))
	sid, err := label.Sid.String()
	if err != nil {
		return protocol.IntegrityUser
	}
	rid, err := strconv.ParseUint(sid[strings.LastIndexByte(sid, '-')+1:], 10, 32)
	if err != nil {
		return protocol.IntegrityUser
	}
	return integrityOfRID(uint32(rid))
}

var (
	netapi32                  = syscall.NewLazyDLL("netapi32.dll")
	procNetGetJoinInformation = netapi32.NewProc("NetGetJoinInformation")
	procNetAPIBufferFree      = netapi32.NewProc("NetApiBufferFree")
)

// The join statuses NetGetJoinInformation says.
const (
	netSetupWorkgroupName = 2
	netSetupDomainName    = 3
)

// hostDomain is the hello's domain: the Windows domain the host is joined
// to, or its workgroup; "" when it is neither, or does not read.
func hostDomain() string {
	if procNetGetJoinInformation.Find() != nil {
		return ""
	}
	var name *uint16
	var status uint32
	r, _, _ := procNetGetJoinInformation.Call(0, uintptr(unsafe.Pointer(&name)), uintptr(unsafe.Pointer(&status)))
	if r != 0 || name == nil {
		return ""
	}
	defer func() { _, _, _ = procNetAPIBufferFree.Call(uintptr(unsafe.Pointer(name))) }()
	if status != netSetupWorkgroupName && status != netSetupDomainName {
		return ""
	}
	return utf16z(name)
}

// utf16z reads a zero-ended UTF-16 string.
func utf16z(p *uint16) string {
	var s []uint16
	for ptr := unsafe.Pointer(p); ; ptr = unsafe.Add(ptr, 2) {
		c := *(*uint16)(ptr)
		if c == 0 || len(s) > 1024 {
			break
		}
		s = append(s, c)
	}
	return syscall.UTF16ToString(s)
}
