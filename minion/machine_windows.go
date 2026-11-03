//go:build windows

package main

import (
	"strings"
	"syscall"
	"unsafe"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// hostMachineID is the host's own machine id (machine.go): the SMBIOS system
// UUID, else the install's MachineGuid; none when neither reads.
func hostMachineID() (int, string) {
	if v := smbiosUUID(firmwareSMBIOS()); v != "" && !protocol.PlaceholderMachineUUID(v) {
		return protocol.MachineSMBIOSUUID, v
	}
	if v := machineGUID(); v != "" && !protocol.PlaceholderMachineUUID(v) {
		return protocol.MachineWinMachineGUID, v
	}
	return 0, ""
}

var (
	kernel32                          = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemFirmwareTable        = kernel32.NewProc("GetSystemFirmwareTable")
	firmwareProviderRSMB       uint32 = 'R'<<24 | 'S'<<16 | 'M'<<8 | 'B'
)

// firmwareSMBIOS is the raw SMBIOS data (RawSMBIOSData) Windows gives any
// user; nil when it gives none.
func firmwareSMBIOS() []byte {
	if procGetSystemFirmwareTable.Find() != nil {
		return nil
	}
	n, _, _ := procGetSystemFirmwareTable.Call(uintptr(firmwareProviderRSMB), 0, 0, 0)
	if n == 0 || n > 1<<24 {
		return nil
	}
	buf := make([]byte, n)
	got, _, _ := procGetSystemFirmwareTable.Call(uintptr(firmwareProviderRSMB), 0, uintptr(unsafe.Pointer(&buf[0])), n)
	if got == 0 || got > n {
		return nil
	}
	return buf[:got]
}

// machineGUID is HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid from the
// registry's 64-bit view (a 32-bit minion would read the 32-bit one's
// otherwise), lowercased; "" when it does not read.
func machineGUID() string {
	key, err := syscall.UTF16PtrFromString(`SOFTWARE\Microsoft\Cryptography`)
	if err != nil {
		return ""
	}
	var h syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, key, 0, syscall.KEY_QUERY_VALUE|keyWOW6464Key, &h) != nil {
		return ""
	}
	defer func() { _ = syscall.RegCloseKey(h) }()
	name, _ := syscall.UTF16PtrFromString("MachineGuid")
	buf := make([]uint16, 64)
	n := uint32(len(buf) * 2)
	var typ uint32
	if syscall.RegQueryValueEx(h, name, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil || typ != syscall.REG_SZ {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(syscall.UTF16ToString(buf[:n/2])))
}

// keyWOW6464Key opens a key in the registry's 64-bit view (KEY_WOW64_64KEY).
const keyWOW6464Key = 0x0100
