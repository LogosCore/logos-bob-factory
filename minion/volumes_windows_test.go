//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// The drives Windows gives the minion: the system's a fixed one, with its
// file system and space; every drive a root, as core's file browser takes
// one (C:\).
func TestHostVolumesWindows(t *testing.T) {
	vols, err := hostVolumes()
	if err != nil {
		t.Fatal(err)
	}
	system := strings.ToUpper(filepath.VolumeName(os.Getenv("SystemRoot"))) + `\`
	found := false
	for _, v := range vols {
		if len(v.Path) != 3 || v.Path[1:] != `:\` || v.Kind == "" {
			t.Errorf("not a drive's root: %+v", v)
		}
		t.Logf("%s %s %q %s size %v free %v ro %v no media %v %s", v.Path, v.Kind, v.Label, v.FS, deref(v.Size), deref(v.Free), v.ReadOnly, v.NoMedia, v.Device)
		if v.Path == system {
			found = true
			if v.Kind != protocol.VolumeFixed || v.FS == "" || v.Size == nil || v.Free == nil || *v.Free > *v.Size || v.NoMedia {
				t.Errorf("the system's drive: %+v", v)
			}
		}
	}
	if !found {
		t.Fatalf("no %s among %+v", system, vols)
	}
	if driveKind(driveCDROM) != protocol.VolumeOptical || driveKind(99) != protocol.VolumeOther {
		t.Fatal("drive kinds")
	}
}

func deref(p *uint64) any {
	if p == nil {
		return nil
	}
	return *p
}
