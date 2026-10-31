package protocol

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
)

// A hello's machine_id (docs: contracts/minion-messages.md, Machine
// identity): what tells apart two machines with the same host and user,
// as "<source>:<value>". The source says where the value came from, so two
// sources never collide by chance and an operator sees what kind of
// identity it is. A minion takes the first of these it can read quietly,
// skipping placeholders (PlaceholderMachineUUID):
//
//  1. a firmware or hardware UUID: MachineSMBIOSUUID (Linux product_uuid,
//     as root; Windows through GetSystemFirmwareTable), MachineMacUUID,
//     MachineBSDHostUUID;
//  2. in a container, the container's id (MachineContainer): an image's
//     install id is every container's;
//  3. the OS install's id: MachineLinuxMachineID, MachineWinMachineGUID;
//  4. a domain machine's identity: MachineDomainSID;
//  5. the host name: MachineHostname;
//  6. a random id the minion keeps (a file, a key): MachineKept;
//  7. a random id for the process alone: MachineEphemeral.
const (
	MachineSMBIOSUUID     = "smbios-uuid"
	MachineMacUUID        = "mac-platform-uuid"
	MachineBSDHostUUID    = "bsd-host-uuid"
	MachineLinuxMachineID = "linux-machine-id"
	MachineWinMachineGUID = "win-machine-guid"
	MachineContainer      = "container"
	MachineDomainSID      = "domain-sid"
	MachineHostname       = "hostname"
	MachineKept           = "kept"
	MachineEphemeral      = "ephemeral"
)

// MaxMachineIDLength bounds a machine_id.
const MaxMachineIDLength = 256

var machineSourcePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// MachineID is a machine_id from its source and value, lowercased and
// trimmed.
func MachineID(source, value string) string {
	return strings.ToLower(source) + ":" + strings.ToLower(strings.TrimSpace(value))
}

// ValidateMachineID checks a machine_id's shape: a source of lowercase
// letters, digits and dashes, a colon, and a value with no spaces or
// control characters; MaxMachineIDLength in all. The source need not be
// one of the Machine constants: a factory may have its own.
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
