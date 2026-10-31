package protocol

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// Task result handlers (docs: contracts/task-result-handlers.md): named
// shapes of a task's data, defined here and the same for every factory. A
// build command names one (BuildCommand.Handler); its minion sends the
// data in that shape, and core checks the parts, joins them, and shows
// them with the handler's view.

// HandlerDirListing is one directory's entries (DirListing).
const HandlerDirListing = "dir_listing"

// handlerIDPattern is what a handler's id matches.
var handlerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// handlerParams are each handler's own params: what core gives when it
// runs a command of the handler itself (a folder opened in a listing).
var handlerParams = map[string]map[string]FieldType{
	HandlerDirListing: {"path": FieldString},
}

// KnownHandler reports whether this version of the protocol defines the
// handler. A command may name one it does not: its data is then shown raw.
func KnownHandler(id string) bool {
	_, ok := handlerParams[id]
	return ok
}

// validateHandler checks a command's handler: its id, and for a handler
// this version defines, the command's params against its contract. The
// command has the handler's params, of their types; any other param it
// takes has a default, so core can run it with the handler's params alone.
func validateHandler(c BuildCommand) error {
	if c.Handler == "" {
		return nil
	}
	if !handlerIDPattern.MatchString(c.Handler) {
		return fmt.Errorf("handler must match %s", handlerIDPattern)
	}
	own, known := handlerParams[c.Handler]
	if !known {
		return nil
	}
	keys := make([]string, 0, len(own))
	for k := range own {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		i := slices.IndexFunc(c.Params, func(p FieldSpec) bool { return p.Key == k })
		if i < 0 || c.Params[i].Type != own[k] {
			return fmt.Errorf("handler %q takes a %s param %q", c.Handler, own[k], k)
		}
	}
	for _, p := range c.Params {
		if _, ok := own[p.Key]; !ok && p.Default == nil {
			return fmt.Errorf("param %q needs a default: core runs a %s command with the handler's params alone", p.Key, c.Handler)
		}
	}
	return nil
}

// DirListing is a dir_listing part's data: Path, Sep and Parent in the
// first part only, Entries in any. A minion encodes it with MarshalCBOR.
type DirListing struct {
	// Path is the directory listed, absolute, as the host writes it.
	Path string `cbor:"path,omitempty" json:"path,omitempty"`
	// Sep is the host's path separator: "/" or "\".
	Sep string `cbor:"sep,omitempty" json:"sep,omitempty"`
	// Parent is the parent directory; empty at a root.
	Parent  string     `cbor:"parent,omitempty" json:"parent,omitempty"`
	Entries []DirEntry `cbor:"entries,omitempty" json:"entries,omitempty"`
	// X holds a factory's own fields.
	X map[string]any `cbor:"x,omitempty" json:"x,omitempty"`
}

// DirEntry is one entry of a directory; "." and ".." are none.
type DirEntry struct {
	// Name is the entry's name in the directory: not empty, no "/".
	Name string `cbor:"name" json:"name"`
	// Type is the entry itself (DirEntryDir, ...), a link not followed.
	Type string `cbor:"type" json:"type"`
	// Size is in bytes, for files (a link: its own size).
	Size *uint64 `cbor:"size,omitempty" json:"size,omitempty"`
	// Modified is unix seconds, negative before 1970.
	Modified *int64 `cbor:"modified,omitempty" json:"modified,omitempty"`
	// Mode is as the host shows it: "-rw-r--r--", or Windows attributes.
	Mode  string `cbor:"mode,omitempty" json:"mode,omitempty"`
	Owner string `cbor:"owner,omitempty" json:"owner,omitempty"`
	Group string `cbor:"group,omitempty" json:"group,omitempty"`
	// Target is a link's target, as the link holds it.
	Target string `cbor:"target,omitempty" json:"target,omitempty"`
	// Hidden is the host's own rule: a dot name, the hidden attribute.
	Hidden bool           `cbor:"hidden,omitempty" json:"hidden,omitempty"`
	X      map[string]any `cbor:"x,omitempty" json:"x,omitempty"`
}

// Entry types. A later version may add types: a reader takes one it does
// not know as DirEntryOther.
const (
	DirEntryDir   = "dir"
	DirEntryFile  = "file"
	DirEntryLink  = "link"
	DirEntryOther = "other"
)

// ReadDirListing decodes a dir_listing part's data and checks it: the
// first part (seq 0) names the directory, later ones only add entries.
// Keys it does not know are ignored.
func ReadDirListing(seq uint64, data cbor.RawMessage) (*DirListing, error) {
	var l DirListing
	if err := cborDec.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("dir_listing: %w", err)
	}
	if seq == 0 {
		switch {
		case l.Path == "":
			return nil, errors.New("dir_listing: the first part names the directory (path)")
		case l.Sep != "/" && l.Sep != `\`:
			return nil, errors.New(`dir_listing: sep is "/" or "\"`)
		}
	} else if l.Path != "" || l.Sep != "" || l.Parent != "" {
		return nil, errors.New("dir_listing: only the first part has path, sep and parent")
	}
	for i, e := range l.Entries {
		switch {
		case e.Name == "" || e.Name == "." || e.Name == "..":
			return nil, fmt.Errorf("dir_listing: entry %d: a name, not %q", i, e.Name)
		case strings.ContainsAny(e.Name, "/\x00"):
			return nil, fmt.Errorf("dir_listing: entry %d: a name without / (%q)", i, e.Name)
		case e.Type == "":
			return nil, fmt.Errorf("dir_listing: entry %d (%q): no type", i, e.Name)
		}
	}
	return &l, nil
}

// ResultData encodes v as a result's data: a map, or a struct (a
// DirListing) encoded as one.
func ResultData(v any) (cbor.RawMessage, error) {
	b, err := MarshalCBOR(v)
	if err != nil {
		return nil, err
	}
	if !isCBORMap(b) {
		return nil, errors.New("result data is a map")
	}
	return b, nil
}

// isCBORMap reports whether b starts a CBOR map (major type 5).
func isCBORMap(b []byte) bool {
	return len(b) > 0 && b[0]>>5 == 5
}
