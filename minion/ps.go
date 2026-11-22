package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// ps lists the host's processes (docs: contracts/task-results-and-actions,
// process_list): on Linux those of the minion's PID namespace (/proc), on
// Windows every session's (hostProcesses). A process it may not open has
// what it could read of it: its pid and its name at least. They go as the
// task's data in parts that keep within half a message, as volumes do; core
// joins them, orders and pages them for the view, and marks the minion's
// own by its hello's pid.
func (r *runner) ps(t *task) {
	procs, err := hostProcesses()
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	fitForCore(procs)
	r.mu.Lock()
	budget := r.perMessageLocked() / 2
	r.mu.Unlock()
	for _, part := range processParts(procs, budget) {
		r.write(t, nil, part)
	}
	r.end(t, true, nil, "")
}

// processParts are the processes in parts of at most budget bytes each (one
// process at least, so a part always moves on: its command line cut to half
// a part); one part, with none, when there are none.
func processParts(procs []protocol.Process, budget int) []protocol.ProcessList {
	sep := string(filepath.Separator)
	parts := []protocol.ProcessList{{Sep: sep}}
	used := 16
	for _, p := range procs {
		p.Cmdline = cutLine(p.Cmdline, budget/2)
		size := 96
		if b, err := protocol.WireResultData(protocol.ProcessList{Sep: sep, Processes: []protocol.Process{p}}); err == nil {
			size = len(b)
		}
		last := &parts[len(parts)-1]
		if len(last.Processes) > 0 && used+size > budget {
			parts = append(parts, protocol.ProcessList{Sep: sep})
			last, used = &parts[len(parts)-1], 16
		}
		last.Processes = append(last.Processes, p)
		used += size
	}
	return parts
}

// fitForCore makes the processes pass core's checks of a part, which would
// refuse the part whole: a name each ("(no name)" for one the host gives
// none), and a path only where it is absolute (Windows names a few
// processes' images without one: "Registry").
func fitForCore(procs []protocol.Process) {
	for i := range procs {
		if procs[i].Name == "" {
			procs[i].Name = "(no name)"
		}
		if !absolutePath(procs[i].Path) {
			procs[i].Path = ""
		}
	}
}

// absolutePath: path is absolute as core checks a process list's for the
// host's separator: from the root ("/"), or a drive's (C:\) or a share's
// (\\host\share) on Windows.
func absolutePath(path string) bool {
	if filepath.Separator == '/' {
		return strings.HasPrefix(path, "/")
	}
	return strings.HasPrefix(path, `\\`) || (len(path) >= 3 && path[1] == ':' && path[2] == '\\')
}

// cutLine is s in n bytes at most: cut at a character's start, an ellipsis
// saying so.
func cutLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const more = "…"
	n = max(n-len(more), 0)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + more
}

// endWait is how long a kill waits to see its process end, to say so.
const endWait = 2 * time.Second

// kill ends a process (docs: contracts/task-results-and-actions,
// kill_process). Pids are reused: given the start time a list said, it
// ends the process only if the one with the pid started then (to the
// second), checked on the process it opened (openTarget), which is the one
// it signals; given 0, whatever has the pid. force ends it at once (SIGKILL,
// TerminateProcess); off, it is asked to end (SIGTERM; Windows has no ask
// every process heeds, and ends it at once either way). The minion's own
// process it ends last, as exit does: the task's end goes to core first.
// The output says what it did, and whether the process ended.
func (r *runner) kill(ctx context.Context, t *task, args protocol.Args) {
	pid, _ := protocol.ArgInt(args, killPid)
	started, _ := protocol.ArgInt(args, killStarted)
	force, _ := protocol.ArgBool(args, killForce)
	if pid <= 0 || pid > math.MaxInt32 {
		r.end(t, false, nil, fmt.Sprintf("pid %d is no process's", pid))
		return
	}
	p, err := openTarget(int(pid))
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	defer p.close()
	switch {
	case started != 0 && p.started == 0:
		r.end(t, false, nil, fmt.Sprintf("%s: when it started does not read, so it may not be the one listed: not ended", p))
		return
	case started != 0 && p.started != started:
		r.end(t, false, nil, fmt.Sprintf("pid %d is another process's now: %s, started %s (the list's started %s): not ended",
			pid, p, stamp(p.started), stamp(started)))
		return
	}
	if p.pid == os.Getpid() {
		r.killSelf(t, p)
		return
	}
	how, err := p.end(force)
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	switch {
	case how == "":
		how = "it had ended already"
	case waitEnded(ctx, p, endWait):
		how += ": it ended"
	case force:
		how += fmt.Sprintf(": it still runs after %s (in the kernel, say: it ends once it leaves)", endWait)
	default:
		how += fmt.Sprintf(": it still runs after %s (force ends it at once)", endWait)
	}
	r.write(t, []byte(fmt.Sprintf("%s: %s\n", p, how)), nil)
	r.end(t, true, nil, "")
}

// killSelf ends the minion as exit does: the tasks beside it first, then
// this task's end, whose delivery stops it (exitID).
func (r *runner) killSelf(t *task, p *target) {
	r.stopOthers(t)
	r.mu.Lock()
	r.exitID = t.id
	r.mu.Unlock()
	r.write(t, []byte(fmt.Sprintf("%s is this minion: it exits once core has this task's end\n", p)), nil)
	r.end(t, true, nil, "")
}

// waitEnded waits up to within for the process to end; false when it has
// not, or the task was stopped.
func waitEnded(ctx context.Context, p *target, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if p.ended() {
			return true
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// String is a process as the output names it: its pid and its name.
func (p *target) String() string {
	if p.name == "" {
		return fmt.Sprintf("pid %d", p.pid)
	}
	return fmt.Sprintf("%d (%s)", p.pid, p.name)
}

// stamp is a start time as the output says it: UTC, to the second.
func stamp(unix int64) string {
	if unix == 0 {
		return "unknown"
	}
	return time.Unix(unix, 0).UTC().Format("2006.01.02 15:04:05") + " UTC"
}

// noProcess is the error for a pid no process has.
func noProcess(pid int) error { return fmt.Errorf("no process has pid %d", pid) }
