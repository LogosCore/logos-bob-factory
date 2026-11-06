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
