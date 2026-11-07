//go:build !windows

package main

import (
	"context"
	"errors"
	"net"
	"time"
)

// errNoPipes: named pipes are Windows': this minion answers an open of one
// unsupported (its build's target leaves pipes out of what it opens).
var errNoPipes = errors.New("named pipes are Windows'")

func dialPipe(context.Context, string, time.Duration) (net.Conn, error) { return nil, errNoPipes }
