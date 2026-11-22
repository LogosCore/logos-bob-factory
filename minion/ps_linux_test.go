//go:build linux

package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A stat's name may hold spaces and parentheses (it ends at the last ")"),
// and its fields go by their numbers in proc(5); one cut short does not
// read.
func TestParseStat(t *testing.T) {
	line := "4242 (we (are) one) S 1 4242 4242 0 -1 4194560 100 0 0 0 7 3 0 0 20 0 5 0 123456 1000 250 18446744073709551615\n"
	st, ok := parseStat([]byte(line))
	if !ok || st.comm != "we (are) one" || st.state != 'S' || st.ppid != 1 || st.ticks != 10 || st.threads != 5 ||
		st.start != 123456 || st.rss != 250 {
		t.Fatalf("%+v %v", st, ok)
	}
	for _, bad := range []string{"4242 (short) S 1 2 3", "no parentheses", "4242 (x) SS 1 4242 4242 0 -1 4194560 100 0 0 0 7 3 0 0 20 0 5 0 123456 1000 250"} {
		if _, ok := parseStat([]byte(bad)); ok {
			t.Errorf("%q read", bad)
		}
	}
}

// Each state letter by its name, the idle and the traced too; one with no
// name, none.
func TestStateName(t *testing.T) {
	for c, want := range map[byte]string{'R': protocol.ProcessRunning, 'S': protocol.ProcessSleeping, 'I': protocol.ProcessSleeping,
		'D': protocol.ProcessWaiting, 'T': protocol.ProcessStopped, 't': protocol.ProcessStopped, 'Z': protocol.ProcessZombie, 'X': ""} {
		if got := stateName(c); got != want {
			t.Errorf("%c: %q, want %q", c, got, want)
		}
	}
}

// A name the kernel cut at 15 bytes comes back whole from the executable
// or the first arg that begins with it; a short one is as it is.
func TestNameOf(t *testing.T) {
	for _, c := range []struct{ comm, exe, argv0, want string }{
		{"bash", "/usr/bin/bash", "-bash", "bash"},
		{"gnome-shell-cal", "/usr/libexec/gnome-shell-calendar-server", "/usr/libexec/gnome-shell-calendar-server", "gnome-shell-calendar-server"},
		{"python3-script1", "/usr/bin/python3.12", "/opt/python3-script1-long.py", "python3-script1-long.py"},
		{"kworker/0:0-eve", "", "", "kworker/0:0-eve"},
		{"systemd-userwor", "", "systemd-userwork: waiting...", "systemd-userwork"},
	} {
		if got := nameOf(c.comm, c.exe, c.argv0); got != c.want {
			t.Errorf("%q: %q, want %q", c.comm, got, c.want)
		}
	}
}

// /proc counts in AT_CLKTCK ticks, 100 on every Linux the minion is built
// for; a process's start, read so, is the second it started.
func TestProcClock(t *testing.T) {
	if hz := clockTicks(); hz != 100 {
		t.Errorf("AT_CLKTCK %d", hz)
	}
	c, err := readClock()
	if err != nil {
		t.Fatal(err)
	}
	st, ok := readStat(os.Getpid())
	if started := time.Unix(c.started(st.start), 0); !ok || time.Since(started) > time.Hour || time.Until(started) > 2*time.Second {
		t.Fatalf("this process started %s", started)
	}
}

// Without force a process may stay (one that ignores SIGTERM): the output
// says so; force ends it.
func TestKillForce(t *testing.T) {
	pid, done := sleeper(t, "sh", "-c", `trap "" TERM; exec sleep 30`)
	// Once sh has run its trap: sleep took its place.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if st, ok := readStat(pid); ok && st.comm == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sh never ran sleep")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started := startedOf(t, pid)
	_, task := runOp(t, opKill, protocol.Args{killPid: uint64(pid), killStarted: started})
	if !task.ok || !strings.Contains(string(task.out), "SIGTERM") || !strings.Contains(string(task.out), "still runs") {
		t.Fatalf("without force: ok %v, %q, %q", task.ok, task.out, task.err)
	}
	_, task = runOp(t, opKill, protocol.Args{killPid: uint64(pid), killStarted: started, killForce: true})
	if !task.ok || !strings.Contains(string(task.out), "SIGKILL") || !strings.Contains(string(task.out), ": it ended") {
		t.Fatalf("with force: ok %v, %q, %q", task.ok, task.out, task.err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still running")
	}
}
