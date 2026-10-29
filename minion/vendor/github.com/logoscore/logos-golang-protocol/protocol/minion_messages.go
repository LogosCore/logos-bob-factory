package protocol

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// Minion messages (docs: contracts/minion-messages.md): what a minion and
// core say inside a sealed frame, one CBOR map each. A minion message goes
// minion → core, a core message core → minion; each carries an id when it
// starts an exchange or re when it answers one, and items.

// MinionMessageV1 is the v of both formats.
const MinionMessageV1 = 1

// Limits a reader enforces before it trusts a length.
const (
	MaxMinionMessageSize  = 768 << 10
	MaxMinionMessageItems = 256
	maxMinionNesting      = 16
)

// Item types.
const (
	ItemStage         = "stage"
	ItemStageResponse = "stage_response"
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
		MaxArrayElements: MaxMinionMessageItems,
		MaxMapPairs:      MaxMinionMessageItems,
		UTF8:             cbor.UTF8RejectInvalid,
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
