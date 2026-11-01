package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"regexp"
	"strings"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// The hello's machine id (docs: contracts/minion-messages/sessions/, Machine
// identity): the first of these the minion can read, all by reading files,
// nothing run, nothing written:
//
//  1. the firmware's UUID (/sys/class/dmi/id/product_uuid: root only), not
//     a placeholder;
//  2. in a container, the container's id: the image's machine-id is every
//     container's;
//  3. the install's machine-id (/etc/machine-id, /var/lib/dbus/machine-id);
//  4. the host name;
//  5. a random id for this process.
//
// It keeps none of its own (a file it would have to write): BOB's minion
// leaves nothing on the host.

// root is where the files are read from: "" on a host, a test's directory
// in a test.
var machineRoot = ""

// machineID is the hello's machine id: its source (a protocol Machine
// source) and value.
func machineID() (int, string) {
	if v := readTrimmed("/sys/class/dmi/id/product_uuid"); v != "" && !protocol.PlaceholderMachineUUID(v) {
		return protocol.MachineSMBIOSUUID, v
	}
	if id := containerID(); id != "" {
		return protocol.MachineContainer, id
	}
	for _, f := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if v := readTrimmed(f); v != "" && !protocol.PlaceholderMachineUUID(v) {
			return protocol.MachineLinuxMachineID, v
		}
	}
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return protocol.MachineHostname, h
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return protocol.MachineEphemeral, hex.EncodeToString(b)
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(machineRoot + path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// containerHexID is a container runtime's id: 64 hex digits.
var containerHexID = regexp.MustCompile(`[0-9a-f]{64}`)

// containerID is the container the minion runs in, from where the runtime
// mounts its files (/proc/self/mountinfo: .../containers/<id>/hostname) or
// its cgroup (v1: .../docker/<id>); "" on a host.
func containerID() string {
	inContainer := false
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(machineRoot + f); err == nil {
			inContainer = true
		}
	}
	cgroup := readTrimmed("/proc/self/cgroup")
	if strings.Contains(cgroup, "docker") || strings.Contains(cgroup, "kubepods") || strings.Contains(cgroup, "containerd") ||
		strings.Contains(cgroup, "libpod") {
		inContainer = true
	}
	if !inContainer {
		return ""
	}
	for _, line := range strings.Split(readTrimmed("/proc/self/mountinfo"), "\n") {
		if strings.Contains(line, "/containers/") && (strings.Contains(line, "/hostname") || strings.Contains(line, "/hosts")) {
			if id := containerHexID.FindString(line); id != "" {
				return id
			}
		}
	}
	return containerHexID.FindString(cgroup)
}
