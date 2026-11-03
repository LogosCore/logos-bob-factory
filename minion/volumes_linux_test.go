//go:build linux

package main

import (
	"strings"
	"testing"
)

// mountinfo as a desktop and a container have it: the kernel's own file
// systems and the snaps left out, a point mounted over as its top mount,
// escapes undone. Its disks (vdz) are none a test host has: a disk's kind
// beyond its file system is the host's /sys to say.
const mountinfo = `22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:13 - proc proc rw
23 1 0:22 / /sys rw,nosuid,nodev,noexec,relatime shared:2 - sysfs sysfs rw
26 1 8:2 / / rw,relatime shared:1 - ext4 /dev/vdz2 rw
27 26 8:1 / /boot/efi ro,relatime shared:3 - vfat /dev/vdz1 rw,fmask=0077
28 26 0:25 / /run rw,nosuid,nodev,noexec,relatime shared:5 - tmpfs tmpfs rw,size=1633216k
29 26 0:26 / /sys/fs/cgroup rw,nosuid,nodev shared:9 - cgroup2 cgroup2 rw
30 26 7:3 / /snap/core22/1380 ro,nodev,relatime shared:27 - squashfs /dev/loop3 ro
31 26 0:40 / /mnt/My\040Share rw,relatime shared:44 - cifs //nas/share rw,vers=3.1.1
32 26 0:41 / /mnt/data rw,relatime shared:45 - ext4 /dev/vdz3 rw
33 26 0:42 / /mnt/data rw,relatime shared:46 - nfs4 nas:/export rw
34 26 0:43 / /media/cd ro,relatime shared:47 - iso9660 /dev/sr0 ro
`

func TestParseMountinfo(t *testing.T) {
	mounts, err := parseMountinfo(strings.NewReader(mountinfo))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range mounts {
		ro := ""
		if m.readOnly {
			ro = " ro"
		}
		got = append(got, m.point+" "+m.fstype+" "+m.source+ro+" "+mountKind(m))
	}
	want := []string{
		"/ ext4 /dev/vdz2 fixed",
		"/boot/efi vfat /dev/vdz1 ro fixed",
		"/run tmpfs tmpfs ram",
		"/mnt/My Share cifs //nas/share network",
		"/mnt/data nfs4 nas:/export network",
		"/media/cd iso9660 /dev/sr0 ro optical",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if s := unescapeMount(`a\040b\134c\0`); s != `a b\c\0` {
		t.Fatalf("unescape: %q", s)
	}
	if s := unescapeLabel(`My\x20Disk`); s != "My Disk" {
		t.Fatalf("label: %q", s)
	}
}

// Where the minion runs: / is one of them, with its space.
func TestHostVolumesLinux(t *testing.T) {
	vols, err := hostVolumes()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vols {
		if v.Path == "/" {
			if v.Size == nil || *v.Size == 0 || v.Free == nil || *v.Free > *v.Size || v.Kind == "" {
				t.Fatalf("/: %+v", v)
			}
			return
		}
		if v.Path == "/proc" || v.Path == "/sys" {
			t.Fatalf("a kernel's own: %+v", v)
		}
	}
	t.Fatalf("no /: %+v", vols)
}
