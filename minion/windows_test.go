package main

import (
	"encoding/binary"
	"testing"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// The pieces of the Windows minion that are plain code, tested on any host:
// the SMBIOS system UUID, the integrity levels, cmd.exe's command line.

// smbiosData is raw SMBIOS data as Windows gives it, of version major.minor:
// a BIOS structure, a System Information one with uuid, the end.
func smbiosData(major, minor byte, uuid []byte) []byte {
	var table []byte
	bios := make([]byte, 0x18)
	bios[0], bios[1] = 0, 0x18
	table = append(table, bios...)
	table = append(table, "Vendor\x00Ver 1\x00\x00"...)
	if uuid != nil {
		sys := make([]byte, 0x1b)
		sys[0], sys[1], sys[4], sys[5] = 1, 0x1b, 1, 2
		copy(sys[8:], uuid)
		table = append(table, sys...)
		table = append(table, "Maker\x00Box\x00\x00"...)
	}
	table = append(table, 127, 4, 0, 0, 0, 0)
	raw := []byte{0, major, minor, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(raw[4:], uint32(len(table)))
	return append(raw, table...)
}

func TestSMBIOSUUID(t *testing.T) {
	dell := []byte{0x44, 0x45, 0x4c, 0x4c, 0x42, 0x00, 0x10, 0x10, 0x80, 0x48, 0xb7, 0xc0, 0x4f, 0x4d, 0x31, 0x32}
	for name, c := range map[string]struct {
		raw  []byte
		want string
	}{
		"3.4: the first fields little-endian, as Windows shows them": {smbiosData(3, 4, dell), "4c4c4544-0042-1010-8048-b7c04f4d3132"},
		"2.4: as stored":     {smbiosData(2, 4, dell), "44454c4c-4200-1010-8048-b7c04f4d3132"},
		"no system UUID":     {smbiosData(3, 4, nil), ""},
		"truncated":          {smbiosData(3, 4, dell)[:30], ""},
		"nothing":            {nil, ""},
		"a header, no table": {[]byte{0, 3, 4, 0, 0, 0, 0, 0}, ""},
	} {
		if got := smbiosUUID(c.raw); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	// Firmware that left the UUID unset says a placeholder: skipped.
	if v := smbiosUUID(smbiosData(3, 4, make([]byte, 16))); !protocol.PlaceholderMachineUUID(v) {
		t.Errorf("all zeros %q is not a placeholder", v)
	}
}

func TestIntegrityOfRID(t *testing.T) {
	for rid, want := range map[uint32]int{
		0x0000: protocol.IntegrityLow, 0x1000: protocol.IntegrityLow, 0x2000: protocol.IntegrityMedium,
		0x2100: protocol.IntegrityMedium, 0x3000: protocol.IntegrityHigh, 0x4000: protocol.IntegritySystem,
		0x5000: protocol.IntegritySystem,
	} {
		if got := integrityOfRID(rid); got != want {
			t.Errorf("0x%x: %d, want %d", rid, got, want)
		}
	}
}

// The command reaches cmd.exe as typed: quoted for /s, which strips the
// outer quotes, its own quotes kept; no AutoRun (/d).
func TestCmdLine(t *testing.T) {
	got := cmdLine(`C:\Windows\system32\cmd.exe`, `dir "C:\Program Files" & whoami /all`)
	want := `"C:\Windows\system32\cmd.exe" /d /s /c "dir "C:\Program Files" & whoami /all"`
	if got != want {
		t.Fatalf("%s\nwant %s", got, want)
	}
}
