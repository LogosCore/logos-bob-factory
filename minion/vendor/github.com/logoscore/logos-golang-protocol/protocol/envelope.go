package protocol

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file defines the shared **control-plane** AMQP envelope used by every
// control-plane message (RPC requests, RPC replies, and events) between core
// and modules. It is distinct from the data-plane minion-sync messages in
// messages.go: the data-plane is HTTP and carries opaque minion blobs, while
// the control-plane is AMQP and carries module-lifecycle / management
// operations. Both share this repo so there is a single source of truth.

// EnvelopeVersionV1 is the current control-plane envelope version (MAJOR.MINOR).
const EnvelopeVersionV1 = "1.0"

// Service identifiers for Envelope.Source.Service.
const (
	ServiceCore    = "core"
	ServiceChannel = "channel"
	ServiceFactory = "factory"
)

// RPC reply status values.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// Control-plane RPC error codes (stable identifiers, safe to branch on). These
// are distinct from the data-plane ValidationError codes in errors.go.
const (
	CodeValidationFailed   = "validation_failed"
	CodeUnsupportedVersion = "unsupported_version"
	CodeUnknownInstance    = "unknown_instance"
	// CodeInstanceBlocked refuses a module.register from an instance an
	// operator blocked: core takes neither its registrations nor its traffic
	// until it is unblocked.
	CodeInstanceBlocked = "instance_blocked"
	CodeInternalError   = "internal_error"
)

// timestampFormat is UTC RFC3339 with millisecond precision, the canonical
// control-plane time format.
const timestampFormat = "2006-01-02T15:04:05.000Z07:00"

// Source is the origin descriptor on a control-plane envelope: the service kind
// (core / channel / factory) and the deployment instance id.
type Source struct {
	Service  string `json:"service"`
	Instance string `json:"instance"`
}

// Envelope is the shared base envelope for any control-plane message (request
// or event). Payload is raw JSON so the transport layer can route on Type
// without knowing the concrete payload schema; handlers decode it.
type Envelope struct {
	MessageID     string          `json:"message_id"`
	CorrelationID string          `json:"correlation_id,omitempty"` // RPC only
	Type          string          `json:"type"`
	Version       string          `json:"version"`
	Timestamp     string          `json:"timestamp"`
	Source        Source          `json:"source"`
	Payload       json.RawMessage `json:"payload"`
}

// EnvelopeError is the reply error block, present only when Status == error.
type EnvelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ReplyEnvelope is the RPC reply: the base envelope plus a result block. The
// reply's CorrelationID always equals the request's CorrelationID.
type ReplyEnvelope struct {
	MessageID     string          `json:"message_id"`
	CorrelationID string          `json:"correlation_id"`
	Type          string          `json:"type"`
	Version       string          `json:"version"`
	Timestamp     string          `json:"timestamp"`
	Source        Source          `json:"source"`
	Status        string          `json:"status"`
	Error         *EnvelopeError  `json:"error"`
	Payload       json.RawMessage `json:"payload"`
}

// NewID returns a fresh random UUID (version 4) in its canonical lowercase
// form. It is the one format of the ids Logos mints: message and correlation
// ids, invocation ids, log stream ids, build ids. A receiver treats an id as
// an opaque string; it reads nothing, not even an order, from it.
func NewID() string {
	var b [16]byte
	// Read never fails: crypto/rand crashes the program instead.
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // the RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NowTimestamp renders the current time in the canonical UTC ms RFC3339 format.
func NowTimestamp() string {
	return time.Now().UTC().Format(timestampFormat)
}

// MajorVersion parses the MAJOR from a "MAJOR.MINOR" version string. The second
// return is false when the input is empty or non-numeric.
func MajorVersion(version string) (int, bool) {
	if version == "" {
		return 0, false
	}
	majorStr, _, found := strings.Cut(version, ".")
	if !found {
		majorStr = version
	}
	major, err := strconv.Atoi(majorStr)
	if err != nil {
		return 0, false
	}
	return major, true
}

// marshalPayload encodes a payload to raw JSON. A nil payload becomes "{}",
// per the contract ("payload ... may be {}").
func marshalPayload(payload any) (json.RawMessage, error) {
	if payload == nil {
		return json.RawMessage("{}"), nil
	}
	if raw, ok := payload.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(payload)
}

// NewReply builds a success reply for req from source, echoing correlation_id
// and type and stamping a fresh message_id and ms-RFC3339 UTC timestamp.
func NewReply(req Envelope, source Source, payload any) (ReplyEnvelope, error) {
	raw, err := marshalPayload(payload)
	if err != nil {
		return ReplyEnvelope{}, err
	}
	return ReplyEnvelope{
		MessageID:     NewID(),
		CorrelationID: req.CorrelationID,
		Type:          req.Type,
		Version:       req.Version,
		Timestamp:     NowTimestamp(),
		Source:        source,
		Status:        StatusOK,
		Error:         nil,
		Payload:       raw,
	}, nil
}

// NewErrorReply builds an error reply for req from source with the given code
// and message.
func NewErrorReply(req Envelope, source Source, code, message string) ReplyEnvelope {
	return ReplyEnvelope{
		MessageID:     NewID(),
		CorrelationID: req.CorrelationID,
		Type:          req.Type,
		Version:       req.Version,
		Timestamp:     NowTimestamp(),
		Source:        source,
		Status:        StatusError,
		Error:         &EnvelopeError{Code: code, Message: message},
		Payload:       json.RawMessage("{}"),
	}
}

// NewEvent builds a fire-and-forget event envelope (no correlation_id) from
// source for the given type/version with the supplied payload.
func NewEvent(eventType, version string, source Source, payload any) (Envelope, error) {
	raw, err := marshalPayload(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		MessageID: NewID(),
		Type:      eventType,
		Version:   version,
		Timestamp: NowTimestamp(),
		Source:    source,
		Payload:   raw,
	}, nil
}
