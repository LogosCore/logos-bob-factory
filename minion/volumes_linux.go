//go:build linux

package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// hostVolumes are the file systems mounted where the minion sees them (its
// mount namespace: a container's own), from /proc/self/mountinfo, in
// mount order: those that hold files (not /proc, /sys, cgroups and their
// like), a mount point mounted over twice as its top one, and a file
// mounted over a file (a container's /etc/hosts) left out.
func hostVolumes() ([]protocol.Volume, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	mounts, err := parseMountinfo(f)
	if err != nil {
		return nil, err
	}
	labels := diskLabels()
	vols := make([]protocol.Volume, 0, len(mounts))
	for _, m := range mounts {
		if info, err := os.Stat(m.point); err == nil && !info.IsDir() {
			continue
		}
		v := protocol.Volume{Path: m.point, Kind: mountKind(m), FS: m.fstype, ReadOnly: m.readOnly}
		if m.source != "" && m.source != "none" && m.source != m.fstype {
			v.Device = m.source
		}
		if strings.HasPrefix(m.source, "/dev/") {
			if real, err := filepath.EvalSymlinks(m.source); err == nil {
				v.Label = labels[real]
			}
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(m.point, &st); err == nil {
			if st.Blocks == 0 {
				// Holds no files: a kernel's own (one pseudoFS does not name).
				continue
			}
			size, free := st.Blocks*uint64(st.Bsize), st.Bavail*uint64(st.Bsize)
			v.Size, v.Free = &size, &free
		}
		vols = append(vols, v)
	}
	return vols, nil
}

// mount is a line of /proc/self/mountinfo, as a volume needs it.
type mount struct {
	point, fstype, source string
	readOnly              bool
}

// pseudoFS are file systems that hold no files of the host's: the kernel's
// views (proc, sysfs), cgroups, and the like.
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "cgroup": true, "cgroup2": true, "mqueue": true,
	"debugfs": true, "tracefs": true, "securityfs": true, "pstore": true, "bpf": true, "configfs": true, "fusectl": true,
	"hugetlbfs": true, "autofs": true, "binfmt_misc": true, "efivarfs": true, "nsfs": true, "rpc_pipefs": true,
	"selinuxfs": true, "nfsd": true, "fuse.gvfsd-fuse": true, "fuse.portal": true,
}

// parseMountinfo reads /proc/self/mountinfo (proc(5)): each mount's point,
// file system, source and whether it is read only, those of pseudoFS and
// the snaps (squashfs under /snap, one per installed snap revision) left
// out. A point mounted over keeps its place, with the top mount's.
func parseMountinfo(r io.Reader) ([]mount, error) {
	var out []mount
	at := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		// id parent major:minor root point options [optional...] - fstype source super
		fields := strings.Fields(sc.Text())
		dash := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				dash = i
				break
			}
		}
		if len(fields) < 6 || dash < 0 || dash+2 >= len(fields) {
			continue
		}
		m := mount{point: unescapeMount(fields[4]), fstype: fields[dash+1], source: unescapeMount(fields[dash+2])}
		for _, o := range strings.Split(fields[5], ",") {
			m.readOnly = m.readOnly || o == "ro"
		}
		if pseudoFS[m.fstype] || (m.fstype == "squashfs" && strings.HasPrefix(m.point, "/snap/")) {
			continue
		}
		if i, ok := at[m.point]; ok {
			out[i] = m
			continue
		}
		at[m.point] = len(out)
		out = append(out, m)
	}
	return out, sc.Err()
}

// unescapeMount undoes mountinfo's octal escapes (\040 a space, \011 a
// tab, \012 a newline, \134 a backslash).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// networkFS are file systems on another host.
var networkFS = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true, "ncpfs": true, "afs": true, "ceph": true,
	"glusterfs": true, "9p": true, "fuse.sshfs": true, "fuse.rclone": true, "fuse.s3fs": true, "davfs": true,
}

// mountKind is what holds a mount: by its file system, and for a block
// device by the kernel's word (/sys/class/block: removable, on USB).
func mountKind(m mount) string {
	switch {
	case networkFS[m.fstype]:
		return protocol.VolumeNetwork
	case m.fstype == "tmpfs" || m.fstype == "ramfs":
		return protocol.VolumeRAM
	case m.fstype == "iso9660" || m.fstype == "udf":
		return protocol.VolumeOptical
	case m.fstype == "zfs":
		return protocol.VolumeFixed
	case !strings.HasPrefix(m.source, "/dev/"):
		return protocol.VolumeOther
	}
	real, err := filepath.EvalSymlinks(m.source)
	if err != nil {
		return protocol.VolumeFixed
	}
	name := filepath.Base(real)
	if strings.HasPrefix(name, "sr") {
		return protocol.VolumeOptical
	}
	dev, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", name))
	if err != nil {
		return protocol.VolumeFixed
	}
	// A partition's own folder is under its disk's, which says it.
	for _, d := range []string{dev, filepath.Dir(dev)} {
		if b, err := os.ReadFile(filepath.Join(d, "removable")); err == nil && strings.TrimSpace(string(b)) == "1" {
			return protocol.VolumeRemovable
		}
	}
	if strings.Contains(dev, "/usb") {
		return protocol.VolumeRemovable
	}
	return protocol.VolumeFixed
}

// diskLabels are the labels udev names devices by (/dev/disk/by-label),
// by device: none where it has no such folder (a container).
func diskLabels() map[string]string {
	out := map[string]string{}
	dir := "/dev/disk/by-label"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if real, err := filepath.EvalSymlinks(filepath.Join(dir, e.Name())); err == nil {
			out[real] = unescapeLabel(e.Name())
		}
	}
	return out
}

// unescapeLabel undoes udev's \xHH escapes in a label's link name (a space
// is \x20).
func unescapeLabel(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if n, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
