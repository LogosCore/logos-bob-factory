package main

import (
	"path/filepath"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// volumes lists the host's volumes (docs: contracts/task-results-and-actions,
// volumes): its drives on Windows, the file systems it has mounted on
// Linux (hostVolumes). They go as the task's data in parts that keep
// within the connection's message size, as a listing's pieces do; core
// joins them, and makes each a folder of the machine's file browser (a
// drive one of its roots).
func (r *runner) volumes(t *task) {
	vols, err := hostVolumes()
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	r.mu.Lock()
	budget := r.perMessageLocked() / 2
	r.mu.Unlock()
	for _, part := range volumeParts(vols, budget) {
		r.write(t, nil, part)
	}
	r.end(t, true, nil, "")
}

// volumeParts are the volumes in parts of at most budget bytes each (one
// volume at least, so a part always moves on); one part, with none, when
// the host has none.
func volumeParts(vols []protocol.Volume, budget int) []protocol.Volumes {
	sep := string(filepath.Separator)
	parts := []protocol.Volumes{{Sep: sep}}
	used := 16
	for _, v := range vols {
		size := 64
		if b, err := protocol.WireResultData(protocol.Volumes{Sep: sep, Volumes: []protocol.Volume{v}}); err == nil {
			size = len(b)
		}
		last := &parts[len(parts)-1]
		if len(last.Volumes) > 0 && used+size > budget {
			parts = append(parts, protocol.Volumes{Sep: sep})
			last, used = &parts[len(parts)-1], 16
		}
		last.Volumes = append(last.Volumes, v)
		used += size
	}
	return parts
}
