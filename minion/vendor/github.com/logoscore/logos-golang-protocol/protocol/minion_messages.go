package protocol

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/fxamacker/cbor/v2"
)

// Minion messages (docs: contracts/minion-messages.md): what a minion and
// core say inside a sealed frame, one CBOR map each. A minion message goes
// minion → core, a core message core → minion; each carries an id when it
// starts an exchange or re when it answers one, and items.

// MinionMessageV1 is the v of both formats.
const MinionMessageV1 = 1

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

// Item types.
const (
	ItemStage         = "stage"
	ItemStageResponse = "stage_response"
	ItemHello         = "hello"
	ItemHelloResponse = "hello_response"
	ItemGetTasks      = "get_tasks"
	ItemSleepUpdate   = "sleep_update"
	ItemTask          = "task"
	ItemCancel        = "cancel"
	ItemResult        = "result"
	ItemAck           = "ack"
	ItemErr           = "err"
)

// MinionMessage is a message minion → core.
type MinionMessage struct {
	V     int               `cbor:"v"`
	ID    []byte            `cbor:"id,omitempty"`
	Re    []byte            `cbor:"re,omitempty"`
	Items []cbor.RawMessage `cbor:"items,omitempty"`
}

// CoreMessage is a message core → minion.
type CoreMessage struct {
	V     int               `cbor:"v"`
	ID    []byte            `cbor:"id,omitempty"`
	Re    []byte            `cbor:"re,omitempty"`
	Items []cbor.RawMessage `cbor:"items,omitempty"`
}

// StageItem is a minion's first message: its RSA public key (DER
// SubjectPublicKeyInfo) and a random correlation id.
type StageItem struct {
	T    string `cbor:"t"`
	Key  []byte `cbor:"key"`
	Corr []byte `cbor:"corr"`
}

// StageResponseItem is core's answer to a stage: the temporary id (16 raw
// UUID bytes) to send the next message under, new keys for the build's
// suite encrypted to the minion's key (EncryptStageKeys), and the stage's
// correlation id.
type StageResponseItem struct {
	T    string `cbor:"t"`
	TID  []byte `cbor:"tid"`
	Keys []byte `cbor:"keys"`
	Corr []byte `cbor:"corr"`
}

// HelloItem is the minion's host and process: it opens a session, and
// under a session id it replaces what the session recorded (a rehello).
// PID, OS and Arch are required; OS and Arch use Go's names, as build
// platforms do. X holds the factory's own fields.
type HelloItem struct {
	T         string   `cbor:"t"`
	PID       uint64   `cbor:"pid"`
	OS        string   `cbor:"os"`
	Arch      string   `cbor:"arch"`
	Host      string   `cbor:"host,omitempty"`
	User      string   `cbor:"user,omitempty"`
	Domain    string   `cbor:"domain,omitempty"`
	Proc      string   `cbor:"proc,omitempty"`
	Integrity string   `cbor:"integrity,omitempty"`
	Addrs     []string `cbor:"addrs,omitempty"`
	Version   string   `cbor:"version,omitempty"`
	// MachineID tells apart machines with the same host and user:
	// "<source>:<value>" (machine_id.go). Every minion sends one; core takes
	// a hello without it from minions built before it was asked for.
	MachineID string         `cbor:"machine_id,omitempty"`
	X         map[string]any `cbor:"x,omitempty"`
	// SID is the persisted session the minion claims, raw bytes; nil for a
	// new session.
	SID []byte `cbor:"sid,omitempty"`
	// Sleep is the check-in schedule the minion starts with; nil when it
	// does not say.
	Sleep *Sleep `cbor:"sleep,omitempty"`
}

// Sleep is a minion's check-in schedule: Interval milliseconds between
// check-ins, spread by Jitter percent of it either way. Core marks a
// session late once a check-in is overdue by it.
type Sleep struct {
	Interval uint64 `cbor:"interval"`
	Jitter   uint64 `cbor:"jitter"`
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
	T        string `cbor:"t"`
	Interval uint64 `cbor:"interval"`
	Jitter   uint64 `cbor:"jitter"`
}

// Sleep is the schedule the item says.
func (u SleepUpdateItem) Sleep() Sleep { return Sleep{Interval: u.Interval, Jitter: u.Jitter} }

// HelloIntegrity holds the integrity values a hello may carry: Windows'
// integrity levels, or root and user elsewhere.
var HelloIntegrity = []string{"low", "medium", "high", "system", "root", "user"}

// Validate checks the required fields and the integrity value.
func (h HelloItem) Validate() error {
	switch {
	case h.PID == 0:
		return errors.New("hello: pid is required")
	case h.OS == "":
		return errors.New("hello: os is required")
	case h.Arch == "":
		return errors.New("hello: arch is required")
	case h.Integrity != "" && !slices.Contains(HelloIntegrity, h.Integrity):
		return fmt.Errorf("hello: integrity %q is not one of %v", h.Integrity, HelloIntegrity)
	case h.SID != nil && len(h.SID) != 16:
		return errors.New("hello: sid is 16 bytes")
	case h.MachineID != "" && ValidateMachineID(h.MachineID) != nil:
		return fmt.Errorf("hello: %w", ValidateMachineID(h.MachineID))
	case h.Sleep != nil:
		return h.Sleep.Validate()
	}
	return nil
}

// HelloResponseItem is core's answer to a hello: the session id, raw bytes.
type HelloResponseItem struct {
	T   string `cbor:"t"`
	SID []byte `cbor:"sid"`
}

// GetTasksItem says the minion takes new tasks, and how many at most: nil
// for no limit of the minion's. A message without one takes none.
type GetTasksItem struct {
	T   string  `cbor:"t"`
	Max *uint64 `cbor:"max,omitempty"`
}

// Validate refuses a max of 0.
func (g GetTasksItem) Validate() error {
	if g.Max != nil && *g.Max == 0 {
		return errors.New("get_tasks: max is at least 1")
	}
	return nil
}

// TaskItem is a command for the minion to run (core → minion): Task is its
// id (16 random bytes), Cmd one of the build's commands, Args its params'
// values, defaults filled in by core.
type TaskItem struct {
	T    string         `cbor:"t"`
	Task []byte         `cbor:"task"`
	Cmd  string         `cbor:"cmd"`
	Args map[string]any `cbor:"args,omitempty"`
}

// Validate checks the task id and the command's id.
func (t TaskItem) Validate() error {
	switch {
	case len(t.Task) != 16:
		return errors.New("task: task is 16 bytes")
	case !commandIDPattern.MatchString(t.Cmd):
		return fmt.Errorf("task: cmd %q is not a command id", t.Cmd)
	}
	return nil
}

// CancelItem asks the minion to stop a task (core → minion). The minion
// still sends the task's final result.
type CancelItem struct {
	T    string `cbor:"t"`
	Task []byte `cbor:"task"`
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
	T    string `cbor:"t"`
	Task []byte `cbor:"task"`
	Seq  uint64 `cbor:"seq"`
	Done bool   `cbor:"done,omitempty"`
	OK   *bool  `cbor:"ok,omitempty"`
	Out  []byte `cbor:"out,omitempty"`
	Err  string `cbor:"err,omitempty"`
	// Data is the command's structured output, a CBOR map passed through
	// as it came: its shape is the command's result's
	// (task_results.go). ResultData encodes one.
	Data cbor.RawMessage `cbor:"data,omitempty"`
	Exit *int64          `cbor:"exit,omitempty"`
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
	return nil
}

// AckItem says what its sender received: tasks (minion → core), or how far
// a file transfer got (Xfer with Next, the offset it has every byte before).
type AckItem struct {
	T     string   `cbor:"t"`
	Tasks [][]byte `cbor:"tasks,omitempty"`
	Xfer  []byte   `cbor:"xfer,omitempty"`
	Next  *uint64  `cbor:"next,omitempty"`
	// Hold, with Xfer: send nothing more of the transfer until an ack
	// without it (a stream waiting for its downloader; docs:
	// contracts/minion-messages.md, Streaming a file).
	Hold bool `cbor:"hold,omitempty"`
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
	T    string `cbor:"t"`
	Code string `cbor:"code"`
	Msg  string `cbor:"msg,omitempty"`
	Item *int   `cbor:"item,omitempty"`
}

// Error codes of ErrItem.
const (
	ErrItemBadMessage  = "bad_message"
	ErrItemUnknownItem = "unknown_item"
	// ErrItemUnknownTask: a cancel (or chunk) for a task the minion does not
	// have.
	ErrItemUnknownTask = "unknown_task"
	ErrItemTooLarge    = "too_large"
	// ErrItemSessionClosed: the session the message was sent under is
	// closed. The one code a minion starts over on.
	ErrItemSessionClosed = "session_closed"
	// ErrItemSessionReplaced: another process claimed the persisted session
	// the message was sent under. Start over without claiming it.
	ErrItemSessionReplaced = "session_replaced"
)

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
	// The subset both sides accept: text keys once each, definite lengths,
	// no tags, and limits checked before a length is trusted.
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

// ItemType is the t of an item, "" when it has none.
func ItemType(item cbor.RawMessage) string {
	var head struct {
		T string `cbor:"t"`
	}
	if err := cborDec.Unmarshal(item, &head); err != nil {
		return ""
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
	case v != MinionMessageV1:
		return fmt.Errorf("v must be %d", MinionMessageV1)
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
