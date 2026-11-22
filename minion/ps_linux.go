//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// hostProcesses are the processes of the minion's PID namespace, read from
// /proc: each one's stat (its name, state, parent, threads, start, CPU time
// and resident memory), its status (who it runs as: its effective uid), its
// command line, its args joined by spaces, and its executable (another
// user's only for root). No session and no architecture: an executable's
// would take opening it, which can hang on a network file system. One that
// ends while it is read is left out.
func hostProcesses() ([]protocol.Process, error) {
	c, err := readClock()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("processes: %w", err)
	}
	page, users := uint64(os.Getpagesize()), map[uint32]string{}
	var out []protocol.Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if p, ok := readProcess(pid, c, page, users); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// readProcess is a process as /proc has it; false when it ended meanwhile.
func readProcess(pid int, c procClock, page uint64, users map[uint32]string) (protocol.Process, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	st, ok := readStat(pid)
	if !ok {
		return protocol.Process{}, false
	}
	started, cpu, mem := c.started(st.start), st.ticks*1000/uint64(c.hz), st.rss*page
	p := protocol.Process{PID: uint64(pid), State: stateName(st.state), Started: &started, Threads: &st.threads, CPUTime: &cpu, Memory: &mem}
	// 0: none (init, kthreadd), for no process has it.
	if st.ppid != 0 {
		p.PPID = &st.ppid
	}
	exe, _ := os.Readlink(dir + "/exe")
	args, _ := os.ReadFile(dir + "/cmdline")
	argv := strings.Split(strings.TrimRight(string(args), "\x00"), "\x00")
	p.Path, p.Cmdline = exe, strings.Join(argv, " ")
	p.Name = nameOf(st.comm, strings.TrimSuffix(exe, " (deleted)"), argv[0])
	if uid, ok := euid(dir); ok {
		p.User = userName(uid, users)
		level := protocol.IntegrityUser
		if uid == 0 {
			level = protocol.IntegrityRoot
		}
		p.Integrity = protocol.IntegrityName(level)
	}
	return p, true
}

// stat is what /proc/<pid>/stat says that a list takes.
type stat struct {
	comm    string
	state   byte
	ppid    uint64
	ticks   uint64 // utime and stime, clock ticks
	threads uint64
	start   uint64 // clock ticks after boot
	rss     uint64 // pages
}

// readStat reads a process's stat; false when it has none (it ended).
func readStat(pid int) (stat, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return stat{}, false
	}
	return parseStat(b)
}

// parseStat reads a stat: the name in parentheses (which may hold spaces
// and parentheses itself: it ends at the last ")"), then the fields by
// their numbers in proc(5).
func parseStat(b []byte) (stat, bool) {
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return stat{}, false
	}
	f := strings.Fields(string(b[end+1:]))
	// f[0] is field 3, the state: field n is f[n-3].
	if len(f) < 22 || len(f[0]) != 1 {
		return stat{}, false
	}
	num := func(n int) uint64 {
		v, _ := strconv.ParseUint(f[n-3], 10, 64)
		return v
	}
	return stat{comm: string(b[open+1 : end]), state: f[0][0], ppid: num(4), ticks: num(14) + num(15), threads: num(20),
		start: num(22), rss: num(24)}, true
}

// stateName is a stat's state letter as a process list names it; "" for
// one it has no name for (X, dead, is gone before anyone reads it).
func stateName(c byte) string {
	switch c {
	case 'R':
		return protocol.ProcessRunning
	case 'S', 'I': // I: a kernel thread, idle
		return protocol.ProcessSleeping
	case 'D':
		return protocol.ProcessWaiting
	case 'T', 't': // t: stopped by its tracer
		return protocol.ProcessStopped
	case 'Z':
		return protocol.ProcessZombie
	}
	return ""
}

// nameOf is a process's name: its comm, which the kernel cuts at 15 bytes;
// when it may have been cut, the longer name of its executable or of its
// first arg that begins with it (the arg before a title the process gave
// itself: "systemd-userwork: waiting...").
func nameOf(comm, exe, argv0 string) string {
	if len(comm) < 15 {
		return comm
	}
	arg, _, _ := strings.Cut(argv0, ": ")
	for _, n := range []string{filepath.Base(exe), filepath.Base(arg)} {
		if len(n) > len(comm) && strings.HasPrefix(n, comm) {
			return n
		}
	}
	return comm
}

// euid is the effective uid a process's status says.
func euid(dir string) (uint32, bool) {
	b, err := os.ReadFile(dir + "/status")
	if err != nil {
		return 0, false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Uid:"); ok {
			// Real, effective, saved, file system.
			if f := strings.Fields(v); len(f) >= 2 {
				id, err := strconv.ParseUint(f[1], 10, 32)
				return uint32(id), err == nil
			}
		}
	}
	return 0, false
}

// userName is a uid's name (from /etc/passwd: os/user without cgo), its
// number when it has none; each looked up once a list.
func userName(uid uint32, names map[uint32]string) string {
	if n, ok := names[uid]; ok {
		return n
	}
	id := strconv.FormatUint(uint64(uid), 10)
	n := id
	if u, err := user.LookupId(id); err == nil && u.Username != "" {
		n = u.Username
	}
	names[uid] = n
	return n
}

// procClock is how /proc counts time: clock ticks a second, and when the
// host booted, unix seconds.
type procClock struct{ hz, boot int64 }

// readClock reads it: the boot time from /proc/stat (btime).
func readClock() (procClock, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return procClock{}, fmt.Errorf("processes: %w", err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if boot, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return procClock{hz: clockTicks(), boot: boot}, nil
			}
		}
	}
	return procClock{}, errors.New("processes: /proc/stat says no boot time")
}

// started is a start in clock ticks after boot as unix seconds: what a
// list says, and what a kill checks.
func (c procClock) started(ticks uint64) int64 { return c.boot + int64(ticks)/c.hz }

// clockTicks is the clock ticks a second /proc counts in: AT_CLKTCK in the
// minion's auxiliary vector (where sysconf reads it); 100, which every
// Linux the minion is built for says, when it does not read.
func clockTicks() int64 {
	b, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 100
	}
	w := int(unsafe.Sizeof(uintptr(0)))
	word := func(b []byte) uint64 {
		if w == 8 {
			return binary.NativeEndian.Uint64(b)
		}
		return uint64(binary.NativeEndian.Uint32(b))
	}
	const atNull, atClkTck = 0, 17
	for i := 0; i+2*w <= len(b); i += 2 * w {
		switch word(b[i:]) {
		case atNull:
			return 100
		case atClkTck:
			if hz := word(b[i+w:]); hz > 0 && hz < 1e6 {
				return int64(hz)
			}
		}
	}
	return 100
}

// The pidfd calls, numbered alike on every architecture (as every call
// since Linux 5.1 is).
const (
	sysPidfdSendSignal = 424
	sysPidfdOpen       = 434
)

// target is a process a kill names, opened as a pidfd where the kernel has
// them (Linux 5.3): signalled through it, a pid reused after the check is
// never the one signalled.
type target struct {
	pid     int
	name    string
	started int64
	fd      int // -1: no pidfd, kill(2) by pid
	clock   procClock
}

// openTarget opens the process with pid, and reads when it started.
func openTarget(pid int) (*target, error) {
	c, err := readClock()
	if err != nil {
		return nil, err
	}
	fd, _, errno := syscall.Syscall(sysPidfdOpen, uintptr(pid), 0, 0)
	if errno == syscall.ESRCH {
		return nil, noProcess(pid)
	}
	p := &target{pid: pid, fd: int(fd), clock: c}
	if errno != 0 {
		p.fd = -1
	}
	st, ok := readStat(pid)
	if !ok {
		p.close()
		return nil, noProcess(pid)
	}
	p.name, p.started = st.comm, c.started(st.start)
	return p, nil
}

// end signals it: SIGKILL with force, else SIGTERM. "" when it had ended
// already (a zombie, its parent yet to reap it, has).
func (p *target) end(force bool) (string, error) {
	if p.ended() {
		return "", nil
	}
	sig, how := syscall.SIGTERM, "asked it to end (SIGTERM)"
	if force {
		sig, how = syscall.SIGKILL, "ended it (SIGKILL)"
	}
	err := error(syscall.ENOSYS)
	if p.fd >= 0 {
		if _, _, errno := syscall.Syscall6(sysPidfdSendSignal, uintptr(p.fd), uintptr(sig), 0, 0, 0, 0); errno != 0 {
			err = errno
		} else {
			err = nil
		}
	}
	// No pidfd, or a seccomp profile that refuses its calls: by pid, as
	// kill(1) does.
	if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EPERM) {
		err = syscall.Kill(p.pid, sig)
	}
	switch {
	case errors.Is(err, syscall.ESRCH):
		return "", nil
	case errors.Is(err, syscall.EPERM):
		return "", fmt.Errorf("the minion may not end %s: %v", p, err)
	case err != nil:
		return "", fmt.Errorf("%s: %v", p, err)
	}
	return how, nil
}

// ended: the process is gone, a zombie, or its pid another's.
func (p *target) ended() bool {
	st, ok := readStat(p.pid)
	return !ok || st.state == 'Z' || st.state == 'X' || p.clock.started(st.start) != p.started
}

func (p *target) close() {
	if p.fd >= 0 {
		_ = syscall.Close(p.fd)
		p.fd = -1
	}
}
