//go:build windows

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

var (
	ntdll                          = syscall.NewLazyDLL("ntdll.dll")
	procNtQuerySystemInformation   = ntdll.NewProc("NtQuerySystemInformation")
	procNtQueryInformationProcess  = ntdll.NewProc("NtQueryInformationProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procIsWow64Process2            = kernel32.NewProc("IsWow64Process2")
	procIsWow64Process             = kernel32.NewProc("IsWow64Process")
)

const (
	processQueryLimitedInformation = 0x1000
	// NtQuerySystemInformation's class for every process, and
	// NtQueryInformationProcess's for a process's command line.
	systemProcessInformation      = 5
	processCommandLineInformation = 60
	// The NTSTATUS values that ask for a bigger buffer.
	statusInfoLengthMismatch = 0xC0000004
	statusBufferOverflow     = 0x80000005
	statusBufferTooSmall     = 0xC0000023
	// filetimeUnix is the Unix epoch in FILETIME's 100 ns since 1601.
	filetimeUnix = 116444736000000000
	// errorInvalidParameter is what OpenProcess says of a pid no process has.
	errorInvalidParameter syscall.Errno = 87
)

// unicodeString is UNICODE_STRING: its Length in bytes, not ended by a 0.
type unicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

func (s unicodeString) String() string {
	if s.Buffer == nil || s.Length == 0 {
		return ""
	}
	return string(utf16.Decode(unsafe.Slice(s.Buffer, s.Length/2)))
}

// systemProcessInfo is SYSTEM_PROCESS_INFORMATION up to the fields a list
// reads. Its Go types lay it out as C does on 32 and 64 bits alike (its
// 64-bit fields fall on offsets of 8 either way; ps_windows_test.go checks
// the offsets).
type systemProcessInfo struct {
	NextEntryOffset              uint32
	NumberOfThreads              uint32
	WorkingSetPrivateSize        int64
	HardFaultCount               uint32
	NumberOfThreadsHighWatermark uint32
	CycleTime                    uint64
	CreateTime                   int64
	UserTime                     int64
	KernelTime                   int64
	ImageName                    unicodeString
	BasePriority                 int32
	UniqueProcessID              uintptr
	InheritedFromUniqueProcessID uintptr
	HandleCount                  uint32
	SessionID                    uint32
	UniqueProcessKey             uintptr
	PeakVirtualSize              uintptr
	VirtualSize                  uintptr
	PageFaultCount               uint32
	PeakWorkingSetSize           uintptr
	WorkingSetSize               uintptr
}

// hostProcesses are every session's processes, from one
// NtQuerySystemInformation (as Task Manager reads them: each one's name,
// parent, session, threads, start, CPU time and working set, without
// opening any), then what opening each one gives (fillFromHandle). No
// state: Windows' processes have none of their own.
func hostProcesses() ([]protocol.Process, error) {
	buf, err := systemProcesses()
	if err != nil {
		return nil, err
	}
	users := map[string]string{}
	var out []protocol.Process
	base, size := unsafe.Pointer(&buf[0]), uintptr(len(buf))*8
	for off := uintptr(0); off+unsafe.Sizeof(systemProcessInfo{}) <= size; {
		e := (*systemProcessInfo)(unsafe.Add(base, off))
		out = append(out, processOf(e, users))
		if e.NextEntryOffset == 0 {
			break
		}
		off += uintptr(e.NextEntryOffset)
	}
	return out, nil
}

// systemProcesses is SystemProcessInformation's buffer, read again bigger
// while processes start faster than it grows.
func systemProcesses() ([]uint64, error) {
	size := uint32(512 << 10)
	for range 8 {
		buf := make([]uint64, size/8)
		var need uint32
		r, _, _ := procNtQuerySystemInformation.Call(systemProcessInformation, uintptr(unsafe.Pointer(&buf[0])), uintptr(size),
			uintptr(unsafe.Pointer(&need)))
		switch uint32(r) {
		case 0:
			return buf, nil
		case statusInfoLengthMismatch:
			size = max(size*2, need+64<<10)
			continue
		}
		return nil, fmt.Errorf("processes: NtQuerySystemInformation: status %#x", uint32(r))
	}
	return nil, errors.New("processes: the list kept growing")
}

// processOf is a process as the system's list has it, and as opening it adds.
func processOf(e *systemProcessInfo, users map[string]string) protocol.Process {
	pid, ppid, session := uint64(e.UniqueProcessID), uint64(e.InheritedFromUniqueProcessID), uint64(e.SessionID)
	threads, mem, cpu := uint64(e.NumberOfThreads), uint64(e.WorkingSetSize), uint64(e.UserTime+e.KernelTime)/10000
	p := protocol.Process{PID: pid, PPID: &ppid, Session: &session, Threads: &threads, Memory: &mem, CPUTime: &cpu, Name: e.ImageName.String()}
	if p.Name == "" && pid == 0 {
		p.Name = "System Idle Process"
	}
	if e.CreateTime > filetimeUnix {
		s := (e.CreateTime - filetimeUnix) / 1e7
		p.Started = &s
	}
	if pid != 0 {
		fillFromHandle(&p, users)
	}
	return p
}

// fillFromHandle adds what opening the process gives
// (PROCESS_QUERY_LIMITED_INFORMATION, which an administrator has for nearly
// every process and a user for their own): its executable, its command
// line, who it runs as and its integrity level, and its architecture when
// WOW64 runs it.
func fillFromHandle(p *protocol.Process, users map[string]string) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(p.PID))
	if err != nil {
		return
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	p.Path, p.Cmdline, p.Arch = imagePath(h), processCommandLine(h), wow64Arch(h)
	p.User, p.Integrity = tokenOf(h, users)
}

// imagePath is the process's executable as Win32 writes it (C:\…).
func imagePath(h syscall.Handle) string {
	for _, n := range []uint32{syscall.MAX_PATH, 32768} {
		buf := make([]uint16, n)
		size := n
		r, _, err := procQueryFullProcessImageNameW.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
		if r != 0 {
			return syscall.UTF16ToString(buf[:size])
		}
		if !errors.Is(err, syscall.ERROR_INSUFFICIENT_BUFFER) {
			return ""
		}
	}
	return ""
}

// processCommandLine is the process's command line, its own
// (ProcessCommandLineInformation: no reading of its memory); "" when it
// does not read (a protected process's).
func processCommandLine(h syscall.Handle) string {
	size := uint32(4096)
	for range 5 {
		buf := make([]uint64, (size+7)/8)
		var need uint32
		r, _, _ := procNtQueryInformationProcess.Call(uintptr(h), processCommandLineInformation, uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)*8), uintptr(unsafe.Pointer(&need)))
		switch uint32(r) {
		case 0:
			return (*unicodeString)(unsafe.Pointer(&buf[0])).String()
		case statusInfoLengthMismatch, statusBufferOverflow, statusBufferTooSmall:
			size = max(size*2, need)
			continue
		}
		return ""
	}
	return ""
}

// tokenOf is who a process runs as (DOMAIN\user, each SID looked up once a
// list) and its integrity level, from its token; "" for what does not read.
func tokenOf(h syscall.Handle, users map[string]string) (string, string) {
	var tok syscall.Token
	if syscall.OpenProcessToken(h, syscall.TOKEN_QUERY, &tok) != nil {
		return "", ""
	}
	defer func() { _ = tok.Close() }()
	var who, integrity string
	if tu, err := tok.GetTokenUser(); err == nil {
		if sid, err := tu.User.Sid.String(); err == nil {
			name, ok := users[sid]
			if !ok {
				name = sid
				if account, domain, _, err := tu.User.Sid.LookupAccount(""); err == nil {
					name = account
					if domain != "" {
						name = domain + `\` + account
					}
				}
				users[sid] = name
			}
			who = name
		}
	}
	if level, ok := tokenIntegrity(tok); ok {
		integrity = protocol.IntegrityName(level)
	}
	return who, integrity
}

// wow64Arch is the architecture of a process WOW64 runs (a 32-bit one on
// 64-bit Windows), in GOARCH's words; "" for one that runs natively, as the
// host's.
func wow64Arch(h syscall.Handle) string {
	if procIsWow64Process2.Find() == nil {
		var process, native uint16
		r, _, _ := procIsWow64Process2.Call(uintptr(h), uintptr(unsafe.Pointer(&process)), uintptr(unsafe.Pointer(&native)))
		switch {
		case r == 0:
		case process == 0x014c: // IMAGE_FILE_MACHINE_I386
			return "386"
		case process == 0x01c4: // IMAGE_FILE_MACHINE_ARMNT
			return "arm"
		}
		return ""
	}
	// Before Windows 10 1709 WOW64 ran x86 alone.
	var wow int32
	if procIsWow64Process.Find() == nil {
		if r, _, _ := procIsWow64Process.Call(uintptr(h), uintptr(unsafe.Pointer(&wow))); r != 0 && wow != 0 {
			return "386"
		}
	}
	return ""
}

// target is a process a kill names, opened: checked and ended through the
// same handle, so a pid reused meanwhile is never the one ended.
type target struct {
	pid     int
	name    string
	started int64
	h       syscall.Handle
}

// openTarget opens the process with pid to end it, and reads when it
// started (GetProcessTimes).
func openTarget(pid int) (*target, error) {
	h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE|processQueryLimitedInformation|syscall.SYNCHRONIZE, false, uint32(pid))
	switch {
	case errors.Is(err, errorInvalidParameter):
		return nil, noProcess(pid)
	case errors.Is(err, syscall.ERROR_ACCESS_DENIED):
		return nil, fmt.Errorf("the minion may not end pid %d: %v", pid, err)
	case err != nil:
		return nil, fmt.Errorf("pid %d: %v", pid, err)
	}
	p := &target{pid: pid, h: h}
	if path := imagePath(h); path != "" {
		p.name = filepath.Base(path)
	}
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) == nil {
		if ft := int64(created.HighDateTime)<<32 | int64(created.LowDateTime); ft > filetimeUnix {
			p.started = (ft - filetimeUnix) / 1e7
		}
	}
	return p, nil
}

// end ends it at once (TerminateProcess), force or not: Windows has no ask
// that every process heeds. "" when it had ended already.
func (p *target) end(bool) (string, error) {
	if p.ended() {
		return "", nil
	}
	if err := syscall.TerminateProcess(p.h, 1); err != nil {
		switch {
		case p.ended():
			return "", nil
		case errors.Is(err, syscall.ERROR_ACCESS_DENIED):
			return "", fmt.Errorf("the minion may not end %s: %v", p, err)
		}
		return "", fmt.Errorf("%s: %v", p, err)
	}
	return "ended it (TerminateProcess)", nil
}

// ended: the process has exited (its handle signalled).
func (p *target) ended() bool {
	ev, err := syscall.WaitForSingleObject(p.h, 0)
	return err == nil && ev == syscall.WAIT_OBJECT_0
}

func (p *target) close() { _ = syscall.CloseHandle(p.h) }
