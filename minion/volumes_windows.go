//go:build windows

package main

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

var (
	procGetLogicalDrives      = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW         = kernel32.NewProc("GetDriveTypeW")
	procGetVolumeInformationW = kernel32.NewProc("GetVolumeInformationW")
	procGetDiskFreeSpaceExW   = kernel32.NewProc("GetDiskFreeSpaceExW")
	procSetThreadErrorMode    = kernel32.NewProc("SetThreadErrorMode")
	mpr                       = syscall.NewLazyDLL("mpr.dll")
	procWNetGetConnectionW    = mpr.NewProc("WNetGetConnectionW")
)

// GetDriveType's answers, and the rest of what Windows says of a drive.
const (
	driveNoRootDir = 1
	driveRemovable = 2
	driveFixed     = 3
	driveRemote    = 4
	driveCDROM     = 5
	driveRAMDisk   = 6

	fileReadOnlyVolume    = 0x00080000 // GetVolumeInformation's file system flags
	semFailCriticalErrors = 0x0001     // no "insert a disk" box for an empty drive
	errorNotReady         = 21         // a drive with no medium
)

// hostVolumes are the drives Windows gives the minion's logon session, A:
// to Z: (its mapped network drives with them): each one's kind, label, file
// system and space; a drive with nothing in it (an empty DVD drive) says
// so. Volumes mounted into a folder rather than a letter are left out.
func hostVolumes() ([]protocol.Volume, error) {
	if err := procGetLogicalDrives.Find(); err != nil {
		return nil, err
	}
	// An empty removable drive asks the user to insert a disk: not from a
	// minion. The mode is the thread's, so the goroutine keeps to it.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var old uint32
	if procSetThreadErrorMode.Find() == nil {
		_, _, _ = procSetThreadErrorMode.Call(semFailCriticalErrors, uintptr(unsafe.Pointer(&old)))
		defer func() { _, _, _ = procSetThreadErrorMode.Call(uintptr(old), 0) }()
	}
	mask, _, err := procGetLogicalDrives.Call()
	if mask == 0 {
		return nil, err
	}
	var vols []protocol.Volume
	for i := range 26 {
		if mask&(1<<i) == 0 {
			continue
		}
		letter := string(rune('A' + i))
		if v, ok := drive(letter); ok {
			vols = append(vols, v)
		}
	}
	return vols, nil
}

// drive is what Windows says of the drive of a letter; false for a letter
// with no drive behind it.
func drive(letter string) (protocol.Volume, bool) {
	root := letter + `:\`
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return protocol.Volume{}, false
	}
	t, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(p)))
	v := protocol.Volume{Path: root, Kind: driveKind(uint32(t))}
	if t == driveNoRootDir {
		return v, false
	}
	if t == driveRemote {
		v.Device = networkShare(letter + ":")
	}
	label, fsName := make([]uint16, syscall.MAX_PATH+1), make([]uint16, syscall.MAX_PATH+1)
	var serial, maxName, flags uint32
	ok, _, err := procGetVolumeInformationW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&label[0])), uintptr(len(label)),
		uintptr(unsafe.Pointer(&serial)), uintptr(unsafe.Pointer(&maxName)), uintptr(unsafe.Pointer(&flags)),
		uintptr(unsafe.Pointer(&fsName[0])), uintptr(len(fsName)))
	if ok == 0 {
		var errno syscall.Errno
		v.NoMedia = errors.As(err, &errno) && errno == errorNotReady
		return v, true
	}
	v.Label, v.FS = syscall.UTF16ToString(label), syscall.UTF16ToString(fsName)
	v.ReadOnly = flags&fileReadOnlyVolume != 0
	var avail, total, totalFree uint64
	if ok, _, _ := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree))); ok != 0 {
		v.Size, v.Free = &total, &avail
	}
	return v, true
}

// driveKind is a drive's kind, from GetDriveType's answer.
func driveKind(t uint32) string {
	switch t {
	case driveFixed:
		return protocol.VolumeFixed
	case driveRemovable:
		return protocol.VolumeRemovable
	case driveRemote:
		return protocol.VolumeNetwork
	case driveCDROM:
		return protocol.VolumeOptical
	case driveRAMDisk:
		return protocol.VolumeRAM
	}
	return protocol.VolumeOther
}

// networkShare is the share a mapped drive (Z:) is: \\server\share; "" when
// Windows does not say.
func networkShare(local string) string {
	if procWNetGetConnectionW.Find() != nil {
		return ""
	}
	l, err := syscall.UTF16PtrFromString(local)
	if err != nil {
		return ""
	}
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if r, _, _ := procWNetGetConnectionW.Call(uintptr(unsafe.Pointer(l)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r != 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}
