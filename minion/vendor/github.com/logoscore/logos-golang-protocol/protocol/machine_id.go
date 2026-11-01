package protocol

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// A hello's machine id (docs: contracts/minion-messages/sessions/, Machine
// identity): what tells apart two machines with the same host and user,
// as "<source>:<value>". The source says where the value came from, so two
// sources never collide by chance and an operator sees what kind of
// identity it is. Which sources a minion reads is its factory's choice; the
// docs recommend the first of these it can read quietly, as any user (the
// same machine gets the same id whoever runs the minion), skipping
// placeholders (PlaceholderMachineUUID):
//
//  1. a firmware or hardware UUID: MachineSMBIOSUUID (Windows, through
//     GetSystemFirmwareTable; not Linux's, which only root reads),
//     MachineMacUUID, MachineBSDHostUUID;
//  2. in a container, the container's id (MachineContainer): an image's
//     install id is every container's;
//  3. the OS install's id: MachineLinuxMachineID, MachineWinMachineGUID;
//  4. the host name: MachineHostname;
//  5. a random id the minion keeps where every user reads it (a file, a
//     key): MachineKept;
//  6. a random id for the process alone: MachineEphemeral.
//
// MachineDomainSID is not recommended (it asks LSA or a domain controller);
// its number stays.
//
// On the wire a hello says the source by its number (HelloItem.MachineSource)
// and the value apart; core keeps them joined, by the source's name.
const (
	MachineSMBIOSUUID     = 1
	MachineMacUUID        = 2
	MachineBSDHostUUID    = 3
	MachineLinuxMachineID = 4
	MachineWinMachineGUID = 5
	MachineContainer      = 6
	MachineDomainSID      = 7
	MachineHostname       = 8
	MachineKept           = 9
	MachineEphemeral      = 10
)

// machineSourceNames are the sources' names, as core keeps them.
var machineSourceNames = map[int]string{
	MachineSMBIOSUUID: "smbios-uuid", MachineMacUUID: "mac-platform-uuid", MachineBSDHostUUID: "bsd-host-uuid",
	MachineLinuxMachineID: "linux-machine-id", MachineWinMachineGUID: "win-machine-guid", MachineContainer: "container",
	MachineDomainSID: "domain-sid", MachineHostname: "hostname", MachineKept: "kept", MachineEphemeral: "ephemeral",
}

// MachineSourceName is a source's name: "source-42" for one this version
// does not know.
func MachineSourceName(source int) string {
	if n, ok := machineSourceNames[source]; ok {
		return n
	}
	return fmt.Sprintf("source-%d", source)
}

// MaxMachineIDLength bounds a machine_id.
const MaxMachineIDLength = 256

var machineSourcePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// MachineID is a machine_id from its source and value, as core keeps it:
// "<source's name>:<value>", the value lowercased and trimmed.
func MachineID(source int, value string) string {
	return MachineSourceName(source) + ":" + strings.ToLower(strings.TrimSpace(value))
}

// ValidateMachineID checks a machine_id's shape: a source of lowercase
// letters, digits and dashes, a colon, and a value with no spaces or
// control characters; MaxMachineIDLength in all. The source need not be
// one this version names.
func ValidateMachineID(id string) error {
	source, value, ok := strings.Cut(id, ":")
	switch {
	case !ok:
		return errors.New(`machine_id is "<source>:<value>"`)
	case len(id) > MaxMachineIDLength:
		return errors.New("machine_id is too long")
	case !machineSourcePattern.MatchString(source):
		return errors.New("machine_id's source is lowercase letters, digits and dashes")
	case value == "":
		return errors.New("machine_id has no value")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("machine_id's value has a space or a control character")
		}
	}
	return nil
}

// placeholderUUIDs are firmware UUIDs boards ship with instead of their
// own: many machines share each.
var placeholderUUIDs = map[string]bool{
	"00000000-0000-0000-0000-000000000000": true,
	"ffffffff-ffff-ffff-ffff-ffffffffffff": true,
	"03000200-0400-0500-0006-000700080009": true,
	"00020003-0004-0005-0006-000700080009": true,
	"12345678-1234-5678-90ab-cddeefaabbcc": true,
}

// PlaceholderMachineUUID reports whether a firmware UUID (or serial) is a
// placeholder rather than the machine's own: empty, all one digit, a value
// boards ship with, or a vendor's "fill me" text.
func PlaceholderMachineUUID(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || placeholderUUIDs[v] {
		return true
	}
	if hex := strings.ReplaceAll(v, "-", ""); hex != "" && strings.Count(hex, hex[:1]) == len(hex) {
		return true
	}
	for _, s := range []string{"to be filled", "not settable", "not specified", "default string", "none", "system serial number", "o.e.m."} {
		if strings.Contains(v, s) {
			return true
		}
	}
	return false
}
