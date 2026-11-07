//go:build windows

package main

import (
	"errors"
	"syscall"
)

// Why a connect failed, by Winsock's error (tunnel.go, dialResult): Go
// gives the WSAE* numbers as they come, which syscall's Unix-like names do
// not match on Windows.
const (
	wsaeNetDown     = 10050
	wsaeNetUnreach  = 10051
	wsaeTimedOut    = 10060
	wsaeConnRefused = 10061
	wsaeHostDown    = 10064
	wsaeHostUnreach = 10065
)

func winsock(err error, codes ...syscall.Errno) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	for _, c := range codes {
		if errno == c {
			return true
		}
	}
	return false
}

func refusedErr(err error) bool { return winsock(err, wsaeConnRefused) }

func hostUnreachableErr(err error) bool {
	return winsock(err, wsaeHostUnreach, wsaeHostDown, errorBadNetpath)
}

func netUnreachableErr(err error) bool { return winsock(err, wsaeNetUnreach, wsaeNetDown) }

func timedOutErr(err error) bool { return winsock(err, wsaeTimedOut) }

// Why a pipe did not open, by Windows' error: another host's refused the
// minion's logon (its rights), or it is not there to reach.
const (
	errorLogonFailure = 1326
	errorBadNetpath   = 53
)

func deniedErr(err error) bool { return winsock(err, errorLogonFailure) }

// Why an accept failed, by Winsock's error.
const (
	wsaeConnAborted = 10053
	wsaeConnReset   = 10054
	wsaeMFile       = 10024
	wsaeNoBufs      = 10055
)

// acceptAgain: an accept that failed for what the host ran short of
// (sockets, buffers), or a connection that went before it was taken: the
// listener goes on after a wait.
func acceptAgain(err error) bool {
	return winsock(err, wsaeMFile, wsaeNoBufs, wsaeConnAborted, wsaeConnReset)
}
