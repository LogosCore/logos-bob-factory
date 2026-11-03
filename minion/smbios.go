package main

import "fmt"

// smbiosUUID is the system UUID in raw SMBIOS data as Windows gives it
// (RawSMBIOSData: a calling method, the SMBIOS version, a DMI revision, the
// table's length, then the table), formatted as a UUID, lowercase; "" when
// the data holds none. From SMBIOS 2.6 on its first three fields are
// little-endian, as Windows shows them; before, they are as stored.
func smbiosUUID(raw []byte) string {
	if len(raw) < 8 {
		return ""
	}
	major, minor := raw[1], raw[2]
	n := int(uint32(raw[4]) | uint32(raw[5])<<8 | uint32(raw[6])<<16 | uint32(raw[7])<<24)
	table := raw[8:]
	if n < len(table) {
		table = table[:n]
	}
	for p := 0; p+4 <= len(table); {
		typ, length := table[p], int(table[p+1])
		if length < 4 || p+length > len(table) {
			return ""
		}
		// Type 1, System Information: the UUID at offset 8, 16 bytes.
		if typ == 1 && length >= 0x19 {
			u := table[p+8 : p+24]
			if major > 2 || (major == 2 && minor >= 6) {
				return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%x-%x", u[3], u[2], u[1], u[0], u[5], u[4], u[7], u[6], u[8:10], u[10:])
			}
			return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:])
		}
		if typ == 127 {
			return ""
		}
		// The formatted part, then its strings, which end with two zeros.
		q := p + length
		for q+1 < len(table) && (table[q] != 0 || table[q+1] != 0) {
			q++
		}
		p = q + 2
	}
	return ""
}
