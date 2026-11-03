//go:build !windows

package main

import (
	"os"
	"regexp"
	"strings"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// root is where the files are read from: "" on a host, a test's directory
// in a test.
var machineRoot = ""

// hostMachineID is the host's own machine id (machine.go): a container's
// id, else the install's machine-id; none when it has neither.
func hostMachineID() (int, string) {
	if id := containerID(); id != "" {
		return protocol.MachineContainer, id
	}
	for _, f := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if v := readTrimmed(f); v != "" && !protocol.PlaceholderMachineUUID(v) {
			return protocol.MachineLinuxMachineID, v
		}
	}
	return 0, ""
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
