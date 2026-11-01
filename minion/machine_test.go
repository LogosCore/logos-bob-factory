package main

import (
	"github.com/logoscore/logos-golang-protocol/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The order: the firmware's UUID (not a placeholder), a container's id, the
// install's machine-id, the host name.
func TestMachineID(t *testing.T) {
	write := func(root, path, body string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defer func(r string) { machineRoot = r }(machineRoot)
	id := strings.Repeat("ab", 32)

	machineRoot = t.TempDir()
	write(machineRoot, "/sys/class/dmi/id/product_uuid", "4C4C4544-0042-1010-8048-B7C04F4D3132\n")
	write(machineRoot, "/etc/machine-id", "5f3e9a00aa\n")
	if got := joinedMachineID(); got != "smbios-uuid:4c4c4544-0042-1010-8048-b7c04f4d3132" {
		t.Errorf("firmware: %q", got)
	}

	machineRoot = t.TempDir()
	write(machineRoot, "/sys/class/dmi/id/product_uuid", "03000200-0400-0500-0006-000700080009")
	write(machineRoot, "/.dockerenv", "")
	write(machineRoot, "/proc/self/mountinfo", "601 590 0:48 /var/lib/docker/containers/"+id+"/hostname /etc/hostname rw - ext4 /dev/sda1 rw")
	write(machineRoot, "/etc/machine-id", "5f3e9a00aa\n")
	if got := joinedMachineID(); got != "container:"+id {
		t.Errorf("container, before the image's machine-id: %q", got)
	}

	machineRoot = t.TempDir()
	write(machineRoot, "/var/lib/dbus/machine-id", "5F3E9A00AA\n")
	if got := joinedMachineID(); got != "linux-machine-id:5f3e9a00aa" {
		t.Errorf("machine-id: %q", got)
	}

	machineRoot = t.TempDir()
	if got := joinedMachineID(); !strings.HasPrefix(got, "hostname:") {
		t.Errorf("host name: %q", got)
	}
}

// joinedMachineID is the machine id as core keeps it: "<source>:<value>".
func joinedMachineID() string {
	source, value := machineID()
	return protocol.MachineID(source, value)
}
