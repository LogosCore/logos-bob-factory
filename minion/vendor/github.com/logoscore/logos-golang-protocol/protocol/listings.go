package protocol

import (
	"fmt"
	"strings"
)

// Listings as they travel (docs: contracts/minion-messages/tasks-and-results/,
// Listings): a result part's ls carries LsPieces, numbered throughout. An
// entry says its type and the errors their codes by number, its mode as the
// host's own bits, and its owner and group as places in its piece's owners,
// so a name that recurs (every file of a folder is "root") travels once a
// piece. Core turns a piece into a ListingPiece (Named), with the session's
// OS to read the mode bits by, and keeps that.

// Entry types, as an entry says them (LsEntry.Type). A type this version
// does not know reads as other.
const (
	EntryDir   = 1
	EntryFile  = 2
	EntryLink  = 3
	EntryOther = 4
)

// EntryTypeName is an entry type's name (DirEntryDir, ...): other for one
// this version does not know.
func EntryTypeName(t int) string {
	switch t {
	case EntryDir:
		return DirEntryDir
	case EntryFile:
		return DirEntryFile
	case EntryLink:
		return DirEntryLink
	}
	return DirEntryOther
}

// Listing errors: why a directory could not be read (LsPiece.Err).
const (
	ListingErrDenied   = 1
	ListingErrNotFound = 2
	ListingErrOther    = 3
)

// LsPiece is a piece of a listing as it travels: ListingPiece, numbered.
// Owners are the owner and group names its entries name (by place, from 1),
// each once; Err says why the directory could not be read (a ListingErr
// code), ErrMsg more, for a person.
type LsPiece struct {
	ID      uint64    `cbor:"1,keyasint"`
	Part    uint64    `cbor:"2,keyasint"`
	Last    bool      `cbor:"3,keyasint,omitempty"`
	Path    string    `cbor:"4,keyasint,omitempty"`
	Sep     string    `cbor:"5,keyasint,omitempty"`
	Parent  string    `cbor:"6,keyasint,omitempty"`
	Entries []LsEntry `cbor:"7,keyasint,omitempty"`
	Partial bool      `cbor:"8,keyasint,omitempty"`
	Err     int       `cbor:"9,keyasint,omitempty"`
	Owners  []string  `cbor:"10,keyasint,omitempty"`
	ErrMsg  string    `cbor:"11,keyasint,omitempty"`
}

// LsEntry is one entry of a directory as a piece carries it: DirEntry,
// numbered. Type is an Entry type; Mode the host's own bits (st_mode on a
// Unix host, the file attributes on Windows), 0 when the minion does not
// say; Owner and Group places in the piece's Owners, from 1, 0 for none.
type LsEntry struct {
	Name     string         `cbor:"1,keyasint"`
	Type     int            `cbor:"2,keyasint"`
	Size     *uint64        `cbor:"3,keyasint,omitempty"`
	Modified *int64         `cbor:"4,keyasint,omitempty"`
	Mode     uint32         `cbor:"5,keyasint,omitempty"`
	Owner    uint64         `cbor:"6,keyasint,omitempty"`
	Group    uint64         `cbor:"7,keyasint,omitempty"`
	Target   string         `cbor:"8,keyasint,omitempty"`
	Hidden   bool           `cbor:"9,keyasint,omitempty"`
	X        map[string]any `cbor:"10,keyasint,omitempty"`
}

// Owner is name's place in the piece's owners, added the first time: what
// an entry's Owner or Group says. 0 for no name.
func (p *LsPiece) Owner(name string) uint64 {
	if name == "" {
		return 0
	}
	for i, o := range p.Owners {
		if o == name {
			return uint64(i + 1)
		}
	}
	p.Owners = append(p.Owners, name)
	return uint64(len(p.Owners))
}

// Validate checks the piece as a ListingPiece, and that its entries' types
// and owners are ones it can name.
func (p LsPiece) Validate() error {
	for i, e := range p.Entries {
		switch {
		case e.Type <= 0:
			return fmt.Errorf("listing %d: entry %d has no type", p.ID, i)
		case e.Owner > uint64(len(p.Owners)) || e.Group > uint64(len(p.Owners)):
			return fmt.Errorf("listing %d: entry %d names an owner the piece does not have", p.ID, i)
		}
	}
	if p.Err < 0 {
		return fmt.Errorf("listing %d: err is a code", p.ID)
	}
	if p.Err == 0 && p.ErrMsg != "" {
		return fmt.Errorf("listing %d: err_msg comes with err", p.ID)
	}
	return p.Named("").Validate()
}

// Named is the piece as core keeps it: types and the error by name, the mode
// as the host shows it (RenderMode, by the session's os), owners by name.
func (p LsPiece) Named(os string) ListingPiece {
	l := ListingPiece{ID: p.ID, Part: p.Part, Last: p.Last, Path: p.Path, Sep: p.Sep, Parent: p.Parent,
		Partial: p.Partial, Err: listingErrText(p.Err, p.ErrMsg)}
	if p.Entries != nil {
		l.Entries = make([]DirEntry, len(p.Entries))
		for i, e := range p.Entries {
			l.Entries[i] = DirEntry{Name: e.Name, Type: EntryTypeName(e.Type), Size: e.Size, Modified: e.Modified,
				Mode: RenderMode(os, e.Mode), Owner: p.owner(e.Owner), Group: p.owner(e.Group), Target: e.Target,
				Hidden: e.Hidden, X: e.X}
		}
	}
	return l
}

// owner is the name at place i (from 1), "" for 0 or one the piece lacks.
func (p LsPiece) owner(i uint64) string {
	if i == 0 || i > uint64(len(p.Owners)) {
		return ""
	}
	return p.Owners[i-1]
}

// listingErrText is a listing error as a person reads it.
func listingErrText(code int, msg string) string {
	var name string
	switch code {
	case 0:
		return ""
	case ListingErrDenied:
		name = "permission denied"
	case ListingErrNotFound:
		name = "not found"
	default:
		if msg != "" {
			return msg
		}
		return "could not be read"
	}
	if msg != "" {
		return name + ": " + msg
	}
	return name
}

// Unix file type bits of st_mode.
const (
	modeTypeMask = 0o170000
	modeSocket   = 0o140000
	modeLink     = 0o120000
	modeFile     = 0o100000
	modeBlock    = 0o060000
	modeDir      = 0o040000
	modeChar     = 0o020000
	modeFIFO     = 0o010000
)

// Windows file attributes.
const (
	attrReadOnly  = 0x1
	attrHidden    = 0x2
	attrSystem    = 0x4
	attrDirectory = 0x10
	attrArchive   = 0x20
	attrReparse   = 0x400
)

// RenderMode is an entry's mode bits as the host shows them: on a Windows
// host (os "windows") its attributes as PowerShell's Mode does ("d-r---",
// "-a-h--": directory, archive, read-only, hidden, system, reparse point);
// elsewhere st_mode as ls -l does ("drwxr-xr-x", "-rwsr-xr-x"). "" for 0.
func RenderMode(os string, mode uint32) string {
	if mode == 0 {
		return ""
	}
	if os == "windows" {
		var b strings.Builder
		for _, f := range []struct {
			bit uint32
			c   byte
		}{{attrDirectory, 'd'}, {attrArchive, 'a'}, {attrReadOnly, 'r'}, {attrHidden, 'h'}, {attrSystem, 's'}, {attrReparse, 'l'}} {
			if mode&f.bit != 0 {
				b.WriteByte(f.c)
			} else {
				b.WriteByte('-')
			}
		}
		return b.String()
	}
	out := []byte("?rwxrwxrwx")
	switch mode & modeTypeMask {
	case modeFile:
		out[0] = '-'
	case modeDir:
		out[0] = 'd'
	case modeLink:
		out[0] = 'l'
	case modeSocket:
		out[0] = 's'
	case modeBlock:
		out[0] = 'b'
	case modeChar:
		out[0] = 'c'
	case modeFIFO:
		out[0] = 'p'
	}
	for i := 0; i < 9; i++ {
		if mode&(1<<(8-i)) == 0 {
			out[i+1] = '-'
		}
	}
	special := func(bit uint32, at int, set, unset byte) {
		if mode&bit == 0 {
			return
		}
		if out[at] == '-' {
			out[at] = unset
		} else {
			out[at] = set
		}
	}
	special(0o4000, 3, 's', 'S')
	special(0o2000, 6, 's', 'S')
	special(0o1000, 9, 't', 'T')
	return string(out)
}
