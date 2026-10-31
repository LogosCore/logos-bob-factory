//go:build unix

package main

import (
	"io/fs"
	"os/user"
	"strconv"
	"sync"
	"syscall"
)

// names caches the user and group names of ids, looked up once each (from
// /etc/passwd and /etc/group: the minion is built without cgo).
var names sync.Map

// owners is a file's owner and group by name, or by id where the host has
// no name for it.
func owners(info fs.FileInfo) (string, string) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ""
	}
	return name("u", st.Uid), name("g", st.Gid)
}

func name(kind string, id uint32) string {
	key := kind + strconv.FormatUint(uint64(id), 10)
	if n, ok := names.Load(key); ok {
		return n.(string)
	}
	s := strconv.FormatUint(uint64(id), 10)
	n := s
	if kind == "u" {
		if u, err := user.LookupId(s); err == nil {
			n = u.Username
		}
	} else if g, err := user.LookupGroupId(s); err == nil {
		n = g.Name
	}
	names.Store(key, n)
	return n
}
