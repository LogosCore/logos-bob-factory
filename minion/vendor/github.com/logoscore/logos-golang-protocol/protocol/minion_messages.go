package protocol

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"reflect"

	"github.com/fxamacker/cbor/v2"
)

// Minion messages (docs: contracts/minion-messages/): what a minion and
// core say inside a sealed frame, one CBOR map each. A minion message goes
// minion → core, a core message core → minion; each carries an id when it
// starts an exchange or re when it answers one, and items.
//
// Every key of a message, of its items and of the structures inside them
// (a sleep, a listing piece and its entries, a file offer, stage keys, the
// data of the results this package defines) is a small unsigned integer,
// and so are item types, err codes and the closed sets of values (an
// integrity level, a machine id's source, an entry's type, a listing's
// error). A task names its command by the number the build schema gives it
// (BuildCommand.Op) and its args by their params' numbers (FieldSpec.Num).
// The registry is the cbor tags below (key 0 of an item is its type, its
// fields count from 1) and the docs' Wire format page. A number is never
// reused for something else. What is the factory's own (the data of a
// result it shapes, an x) keeps text keys.

// MinionMessageV3 is the v of both formats: numbered keys, values and
// commands. v 1 and 2 are no longer taken.
const MinionMessageV3 = 3

// Limits a reader enforces before it trusts a length. A message carries at
// most MaxMinionMessageItems items (ValidateMessageHead); an array or map
// inside one (a result's data: an ls of a big directory) at most
// MaxMinionNestedElements, the message's size bounding them anyway.
const (
	MaxMinionMessageSize    = 768 << 10
	MaxMinionMessageItems   = 256
	MaxMinionNestedElements = 1 << 16
	maxMinionNesting        = 16
)

// Item types: an item's key 0. 0 is none.
const (
	ItemStage         = 1
	ItemStageResponse = 2
	ItemHello         = 3
	ItemHelloResponse = 4
	ItemSleepUpdate   = 5
	// 6 is rehello (planned): core asking for a new hello.
	ItemGetTasks = 7
	ItemTask     = 8
	ItemCancel   = 9
	ItemResult   = 10
	ItemAck      = 11
	ItemChunk    = 12
	ItemErr      = 13
)

// itemNames are the item types' names, for people (logs, docs).
var itemNames = map[int]string{
	ItemStage: "stage", ItemStageResponse: "stage_response", ItemHello: "hello",
	ItemHelloResponse: "hello_response", ItemSleepUpdate: "sleep_update", 6: "rehello",
	ItemGetTasks: "get_tasks", ItemTask: "task", ItemCancel: "cancel", ItemResult: "result",
	ItemAck: "ack", ItemChunk: "chunk", ItemErr: "err",
}

// ItemName is an item type's name, for people: "item 42" for one it does
// not know.
func ItemName(t int) string {
	if n, ok := itemNames[t]; ok {
		return n
	}
	return fmt.Sprintf("item %d", t)
}

// MinionMessage is a message minion → core.
type MinionMessage struct {
	V     int               `cbor:"0,keyasint"`
	ID    []byte            `cbor:"1,keyasint,omitempty"`
	Re    []byte            `cbor:"2,keyasint,omitempty"`
	Items []cbor.RawMessage `cbor:"3,keyasint,omitempty"`
}

// CoreMessage is a message core → minion.
type CoreMessage struct {
	V     int               `cbor:"0,keyasint"`
	ID    []byte            `cbor:"1,keyasint,omitempty"`
	Re    []byte            `cbor:"2,keyasint,omitempty"`
	Items []cbor.RawMessage `cbor:"3,keyasint,omitempty"`
}

// StageItem is a minion's first message: its RSA public key (DER
// SubjectPublicKeyInfo) and a random correlation id.
type StageItem struct {
	T    int    `cbor:"0,keyasint"`
	Key  []byte `cbor:"1,keyasint"`
	Corr []byte `cbor:"2,keyasint"`
}

// StageResponseItem is core's answer to a stage: the temporary id (16 raw
// UUID bytes) to send the next message under, new keys for the build's
// suite encrypted to the minion's key (EncryptStageKeys), and the stage's
// correlation id.
type StageResponseItem struct {
	T    int    `cbor:"0,keyasint"`
	TID  []byte `cbor:"1,keyasint"`
	Keys []byte `cbor:"2,keyasint"`
	Corr []byte `cbor:"3,keyasint"`
}

// HelloItem is the minion's host and process: it opens a session, and
// under a session id it replaces what the session recorded (a rehello).
// PID, OS and Arch are required; OS and Arch use Go's names, as build
// platforms do. X holds the factory's own fields.
type HelloItem struct {
	T      int    `cbor:"0,keyasint"`
	PID    uint64 `cbor:"1,keyasint"`
	OS     string `cbor:"2,keyasint"`
	Arch   string `cbor:"3,keyasint"`
	Host   string `cbor:"4,keyasint,omitempty"`
	User   string `cbor:"5,keyasint,omitempty"`
	Domain string `cbor:"6,keyasint,omitempty"`
	Proc   string `cbor:"7,keyasint,omitempty"`
	// Integrity is one of the Integrity levels; 0 when the minion does not
	// say.
	Integrity int `cbor:"8,keyasint,omitempty"`
	// Addrs are the host's IP addresses, raw: 4 bytes or 16.
	Addrs   []net.IP `cbor:"9,keyasint,omitempty"`
	Version string   `cbor:"10,keyasint,omitempty"`
	// MachineSource and MachineValue tell apart machines with the same
	// host and user (machine_id.go): where the value came from (one of the
	// Machine sources) and the value. MachineID joins them as core keeps
	// them.
	MachineSource int            `cbor:"11,keyasint,omitempty"`
	MachineValue  string         `cbor:"15,keyasint,omitempty"`
	X             map[string]any `cbor:"12,keyasint,omitempty"`
	// SID is the persisted session the minion claims, raw bytes; nil for a
	// new session.
	SID []byte `cbor:"13,keyasint,omitempty"`
	// Sleep is the check-in schedule the minion starts with; nil when it
	// does not say.
	Sleep *Sleep `cbor:"14,keyasint,omitempty"`
}

// Sleep is a minion's check-in schedule: Interval milliseconds between
// check-ins, spread by Jitter percent of it either way. Core marks a
// session late once a check-in is overdue by it.
type Sleep struct {
	Interval uint64 `cbor:"1,keyasint"`
	Jitter   uint64 `cbor:"2,keyasint"`
}

// Validate refuses an interval of 0 and a jitter over 100.
func (s Sleep) Validate() error {
	switch {
	case s.Interval == 0:
		return errors.New("sleep: interval is at least 1 ms")
	case s.Jitter > 100:
		return errors.New("sleep: jitter is 0 to 100 percent")
	}
	return nil
}

// SleepUpdateItem says the minion's check-in schedule changed.
type SleepUpdateItem struct {
	T        int    `cbor:"0,keyasint"`
	Interval uint64 `cbor:"1,keyasint"`
	Jitter   uint64 `cbor:"2,keyasint"`
}

// Sleep is the schedule the item says.
func (u SleepUpdateItem) Sleep() Sleep { return Sleep{Interval: u.Interval, Jitter: u.Jitter} }

// Integrity levels a hello may say: Windows' integrity levels, or root and
// user elsewhere.
const (
	IntegrityLow    = 1
	IntegrityMedium = 2
	IntegrityHigh   = 3
	IntegritySystem = 4
	IntegrityRoot   = 5
	IntegrityUser   = 6
)

// integrityNames are the levels' names, as core keeps and shows them.
var integrityNames = map[int]string{
	IntegrityLow: "low", IntegrityMedium: "medium", IntegrityHigh: "high", IntegritySystem: "system",
	IntegrityRoot: "root", IntegrityUser: "user",
}

// IntegrityName is a level's name: "" for none, "level 7" for one this
// version does not know.
func IntegrityName(level int) string {
	if level == 0 {
		return ""
	}
	if n, ok := integrityNames[level]; ok {
		return n
	}
	return fmt.Sprintf("level %d", level)
}

// MachineID is the hello's machine id as core keeps it, "<source>:<value>"
// (MachineID in machine_id.go); "" when the hello has none.
func (h HelloItem) MachineID() string {
	if h.MachineValue == "" {
		return ""
	}
	return MachineID(h.MachineSource, h.MachineValue)
}

// AddrStrings are the hello's addresses as text.
func (h HelloItem) AddrStrings() []string {
	if len(h.Addrs) == 0 {
		return nil
	}
	out := make([]string, len(h.Addrs))
	for i, a := range h.Addrs {
		out[i] = a.String()
	}
	return out
}

// Validate checks the required fields, the addresses and the machine id. An
// integrity level or machine source this version does not know is taken:
// core shows its number.
func (h HelloItem) Validate() error {
	switch {
	case h.PID == 0:
		return errors.New("hello: pid is required")
	case h.OS == "":
		return errors.New("hello: os is required")
	case h.Arch == "":
		return errors.New("hello: arch is required")
	case h.Integrity < 0:
		return errors.New("hello: integrity is a level")
	case h.SID != nil && len(h.SID) != 16:
		return errors.New("hello: sid is 16 bytes")
	case (h.MachineValue == "") != (h.MachineSource == 0), h.MachineSource < 0:
		return errors.New("hello: a machine id is a source and a value")
	case h.MachineValue != "" && ValidateMachineID(h.MachineID()) != nil:
		return fmt.Errorf("hello: %w", ValidateMachineID(h.MachineID()))
	case h.Sleep != nil:
		return h.Sleep.Validate()
	}
	for _, a := range h.Addrs {
		if len(a) != net.IPv4len && len(a) != net.IPv6len {
			return errors.New("hello: an address is 4 bytes or 16")
		}
	}
	return nil
}

// HelloResponseItem is core's answer to a hello: the session id, raw bytes.
type HelloResponseItem struct {
	T   int    `cbor:"0,keyasint"`
	SID []byte `cbor:"1,keyasint"`
}

// GetTasksItem says the minion takes new tasks, and how many at most: nil
// for no limit of the minion's. A message without one takes none.
type GetTasksItem struct {
	T   int     `cbor:"0,keyasint"`
	Max *uint64 `cbor:"1,keyasint,omitempty"`
}

// Validate refuses a max of 0.
func (g GetTasksItem) Validate() error {
	if g.Max != nil && *g.Max == 0 {
		return errors.New("get_tasks: max is at least 1")
	}
	return nil
}

// TaskItem is a command for the minion to run (core → minion): Task is its
// id (16 random bytes), Cmd the number the build schema gives the command
// (BuildCommand.Op), Args its params' values by their numbers
// (FieldSpec.Num), defaults filled in by core.
type TaskItem struct {
	T    int    `cbor:"0,keyasint"`
	Task []byte `cbor:"1,keyasint"`
	Cmd  uint64 `cbor:"2,keyasint"`
	Args Args   `cbor:"3,keyasint,omitempty"`
}

// Args are a task's args: its params' numbers (FieldSpec.Num) to their
// values (WireArgs). A file or stream param's value is a FileOffer,
// numbered keys as every protocol map has: decoded, it stays the CBOR it
// came as (a cbor.RawMessage), which ArgFile reads.
type Args map[uint64]any

// UnmarshalCBOR decodes args, keeping an integer-keyed map as it came.
func (a *Args) UnmarshalCBOR(data []byte) error {
	var raw map[uint64]cbor.RawMessage
	if err := cborDec.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		*a = nil
		return nil
	}
	out := make(Args, len(raw))
	for k, v := range raw {
		if isIntKeyedMap(v) {
			out[k] = v
			continue
		}
		var x any
		if err := cborDec.Unmarshal(v, &x); err != nil {
			return fmt.Errorf("arg %d: %w", k, err)
		}
		out[k] = x
	}
	*a = out
	return nil
}

// isIntKeyedMap reports whether b is a CBOR map whose first key is an
// unsigned integer.
func isIntKeyedMap(b []byte) bool {
	if !isCBORMap(b) {
		return false
	}
	head := map[byte]int{24: 2, 25: 3, 26: 5, 27: 9}[b[0]&0x1f]
	if b[0]&0x1f < 24 {
		head = 1
	}
	return head > 0 && len(b) > head && b[head]>>5 == 0
}

// Validate checks the task id and the command's number.
func (t TaskItem) Validate() error {
	switch {
	case len(t.Task) != 16:
		return errors.New("task: task is 16 bytes")
	case t.Cmd == 0:
		return errors.New("task: cmd is a command's number, at least 1")
	}
	return nil
}

// CancelItem asks the minion to stop a task (core → minion). The minion
// still sends the task's final result.
type CancelItem struct {
	T    int    `cbor:"0,keyasint"`
	Task []byte `cbor:"1,keyasint"`
}

// Validate checks the task id.
func (c CancelItem) Validate() error {
	if len(c.Task) != 16 {
		return errors.New("cancel: task is 16 bytes")
	}
	return nil
}

// ResultItem is one part of a task's output (minion → core), Seq 0, 1, 2…;
// the last has Done, and OK with it. Out is output as raw bytes in the
// host's own encoding, Data structured output in the command's own shape
// (each part's adds to the earlier parts'), Err what went wrong, Exit the
// exit code of the process the task ran, on the done part only.
type ResultItem struct {
	T    int    `cbor:"0,keyasint"`
	Task []byte `cbor:"1,keyasint"`
	Seq  uint64 `cbor:"2,keyasint"`
	Done bool   `cbor:"3,keyasint,omitempty"`
	OK   *bool  `cbor:"4,keyasint,omitempty"`
	Out  []byte `cbor:"5,keyasint,omitempty"`
	Err  string `cbor:"6,keyasint,omitempty"`
	// Data is the command's structured output, a CBOR map passed through
	// as it came: its shape is the command's result's
	// (task_results.go). ResultData encodes one.
	Data cbor.RawMessage `cbor:"7,keyasint,omitempty"`
	Exit *int64          `cbor:"8,keyasint,omitempty"`
	// Ls is what the task saw of the host's file system: listings, in
	// pieces (LsPiece), beside or instead of out and data.
	Ls []LsPiece `cbor:"9,keyasint,omitempty"`
}

// Validate checks the task id, and ok and exit against done.
func (r ResultItem) Validate() error {
	switch {
	case len(r.Task) != 16:
		return errors.New("result: task is 16 bytes")
	case r.Done && r.OK == nil:
		return errors.New("result: the done part says ok")
	case !r.Done && r.OK != nil:
		return errors.New("result: only the done part says ok")
	case !r.Done && r.Exit != nil:
		return errors.New("result: only the done part has an exit code")
	case len(r.Data) > 0 && !isCBORMap(r.Data):
		return errors.New("result: data is a map")
	}
	for _, p := range r.Ls {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("result: %w", err)
		}
	}
	return nil
}

// AckItem says what its sender received: tasks (minion → core), or how far
// a file transfer got (Xfer with Next, the offset it has every byte before).
type AckItem struct {
	T     int      `cbor:"0,keyasint"`
	Tasks [][]byte `cbor:"1,keyasint,omitempty"`
	Xfer  []byte   `cbor:"2,keyasint,omitempty"`
	Next  *uint64  `cbor:"3,keyasint,omitempty"`
	// Hold, with Xfer: send nothing more of the transfer until an ack
	// without it (a stream waiting for its downloader; docs:
	// contracts/minion-messages.md, Streaming a file).
	Hold bool `cbor:"4,keyasint,omitempty"`
}

// Validate checks the ids, and that a transfer comes with its offset.
func (a AckItem) Validate() error {
	for _, t := range a.Tasks {
		if len(t) != 16 {
			return errors.New("ack: a task is 16 bytes")
		}
	}
	switch {
	case (a.Xfer == nil) != (a.Next == nil):
		return errors.New("ack: xfer and next come together")
	case a.Xfer != nil && len(a.Xfer) != 16:
		return errors.New("ack: xfer is 16 bytes")
	case a.Hold && a.Xfer == nil:
		return errors.New("ack: hold comes with xfer")
	}
	return nil
}

// ErrItem reports a problem with a message or one of its items.
type ErrItem struct {
	T    int    `cbor:"0,keyasint"`
	Code int    `cbor:"1,keyasint"`
	Msg  string `cbor:"2,keyasint,omitempty"`
	Item *int   `cbor:"3,keyasint,omitempty"`
}

// Error codes of ErrItem.
const (
	ErrItemBadMessage  = 1
	ErrItemUnknownItem = 2
	// ErrItemUnknownTask: a cancel (or chunk) for a task the minion does not
	// have.
	ErrItemUnknownTask = 3
	ErrItemTooLarge    = 4
	// ErrItemSessionClosed: the session the message was sent under is
	// closed. The one code a minion starts over on.
	ErrItemSessionClosed = 5
	// ErrItemSessionReplaced: another process claimed the persisted session
	// the message was sent under. Start over without claiming it.
	ErrItemSessionReplaced = 6
)

// errCodeNames are the err codes' names, for people.
var errCodeNames = map[int]string{
	ErrItemBadMessage: "bad_message", ErrItemUnknownItem: "unknown_item", ErrItemUnknownTask: "unknown_task",
	ErrItemTooLarge: "too_large", ErrItemSessionClosed: "session_closed", ErrItemSessionReplaced: "session_replaced",
}

// ErrCodeName is an err code's name, for people: "code 42" for one it does
// not know.
func ErrCodeName(code int) string {
	if n, ok := errCodeNames[code]; ok {
		return n
	}
	return fmt.Sprintf("code %d", code)
}

// StageKeySize is the RSA key size a stage's key must have.
const StageKeySize = 2048

var (
	cborEnc cbor.EncMode
	cborDec cbor.DecMode
)

func init() {
	var err error
	// Preferred serialization: the shortest form of every integer, float
	// and length.
	if cborEnc, err = (cbor.EncOptions{
		Sort: cbor.SortNone, ShortestFloat: cbor.ShortestFloat16, IndefLength: cbor.IndefLengthForbidden,
		TagsMd: cbor.TagsForbidden,
	}).EncMode(); err != nil {
		panic(err)
	}
	// The subset both sides accept: keys once each, definite lengths, no
	// tags, and limits checked before a length is trusted.
	if cborDec, err = (cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  maxMinionNesting,
		MaxArrayElements: MaxMinionNestedElements,
		MaxMapPairs:      MaxMinionNestedElements,
		UTF8:             cbor.UTF8RejectInvalid,
		// Maps decoded into any (a hello's x) are map[string]any, so they
		// store and print as JSON objects; a key that is not text fails.
		DefaultMapType: reflect.TypeOf(map[string]any(nil)),
	}).DecMode(); err != nil {
		panic(err)
	}
}

// MarshalCBOR encodes v as the formats require.
func MarshalCBOR(v any) ([]byte, error) { return cborEnc.Marshal(v) }

// UnmarshalCBOR decodes data into v with the formats' rules and limits.
func UnmarshalCBOR(data []byte, v any) error {
	if len(data) > MaxMinionMessageSize {
		return fmt.Errorf("message of %d bytes is over %d", len(data), MaxMinionMessageSize)
	}
	return cborDec.Unmarshal(data, v)
}

// ItemType is the type of an item (its key 0), 0 when it has none.
func ItemType(item cbor.RawMessage) int {
	var head struct {
		T int `cbor:"0,keyasint"`
	}
	if err := cborDec.Unmarshal(item, &head); err != nil {
		return 0
	}
	return head.T
}

// NewMessageID is a fresh random message id.
func NewMessageID() []byte {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return id
}

// ValidateMessageHead checks what every message has: v, exactly one of id
// and re (16 bytes), and at most MaxMinionMessageItems items.
func ValidateMessageHead(v int, id, re []byte, items int) error {
	switch {
	case v != MinionMessageV3:
		return fmt.Errorf("v must be %d", MinionMessageV3)
	case (id == nil) == (re == nil):
		return errors.New("a message has exactly one of id and re")
	case id != nil && len(id) != 16, re != nil && len(re) != 16:
		return errors.New("id and re are 16 bytes")
	case items > MaxMinionMessageItems:
		return fmt.Errorf("more than %d items", MaxMinionMessageItems)
	}
	return nil
}

// ParseStageKey reads a stage's key: an RSA public key of StageKeySize bits,
// DER SubjectPublicKeyInfo.
func ParseStageKey(der []byte) (*rsa.PublicKey, error) {
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("stage key: %w", err)
	}
	pub, ok := k.(*rsa.PublicKey)
	if !ok || pub.N.BitLen() != StageKeySize {
		return nil, fmt.Errorf("stage key must be RSA-%d", StageKeySize)
	}
	return pub, nil
}

// EncryptStageKeys encrypts a suite's key material (as CBOR) to a minion's
// stage key, with RSA-OAEP and SHA-256.
func EncryptStageKeys(pub *rsa.PublicKey, material []byte) ([]byte, error) {
	return rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, material, nil)
}

// DecryptStageKeys undoes EncryptStageKeys with the minion's private key.
func DecryptStageKeys(priv *rsa.PrivateKey, ciphertext []byte) ([]byte, error) {
	return priv.Decrypt(rand.Reader, ciphertext, &rsa.OAEPOptions{Hash: crypto.SHA256})
}
