package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// The hello's machine id (docs: contracts/minion-messages/sessions/, Machine
// identity): the first source the minion can read, quietly (a file, a
// registry value, a system call: nothing run, nothing written) and the same
// whoever runs it, so the same machine gets the same id from a minion run
// as an administrator and one run as a user.
//
// On Linux and other Unix hosts (machine_unix.go):
//
//  1. in a container, the container's id: the image's machine-id is every
//     container's;
//  2. the install's machine-id (/etc/machine-id, /var/lib/dbus/machine-id).
//
// Not the firmware's UUID (/sys/class/dmi/id/product_uuid): only root reads
// it, so a minion run as root and one run as a user would name one machine
// twice.
//
// On Windows (machine_windows.go):
//
//  1. the SMBIOS system UUID, from the firmware table any user reads
//     (GetSystemFirmwareTable): the hardware's, or the VM's;
//  2. the install's MachineGuid, from the registry's 64-bit view.
//
// Then, anywhere, the host name, and last a random id for this process.
//
// It keeps none of its own (a file it would have to write): BOB's minion
// leaves nothing on the host.

// machineID is the hello's machine id: its source (a protocol Machine
// source) and value.
func machineID() (int, string) {
	if src, v := hostMachineID(); v != "" {
		return src, v
	}
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return protocol.MachineHostname, h
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return protocol.MachineEphemeral, hex.EncodeToString(b)
}
