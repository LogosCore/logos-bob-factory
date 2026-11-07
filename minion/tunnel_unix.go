//go:build unix

package main

import (
	"errors"
	"syscall"
)

// Why a connect failed, by the host's error (tunnel.go, dialResult).

func refusedErr(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }

func hostUnreachableErr(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.EHOSTDOWN)
}

func netUnreachableErr(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.ENETDOWN)
}

func timedOutErr(err error) bool { return errors.Is(err, syscall.ETIMEDOUT) }

// acceptAgain: an accept that failed for what the host ran short of (files,
// buffers, memory), or a connection that went before it was taken: the
// listener goes on after a wait.
func acceptAgain(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOBUFS) ||
		errors.Is(err, syscall.ENOMEM) || errors.Is(err, syscall.ECONNABORTED)
}
