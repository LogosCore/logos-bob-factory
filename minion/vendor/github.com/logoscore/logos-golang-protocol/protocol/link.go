package protocol

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// The control link (docs: contracts/channel-core-connections/, The control
// link and connection sockets: formats): a WebSocket each channel instance
// opens to core, over which core asks the channel to reach bind minions.
// Core sends a call; the channel delivers the sealed message to the target
// and answers with what came back, by the request's id. Each binary frame is
// one LinkFrame.

// LinkPath is core's endpoint for the control link, under core's base URL:
// a WebSocket handshake with HeaderChannelInstance.
const LinkPath = "/api/channel/link"

// Link frames, by LinkFrame.Type.
const (
	// LinkCall (core → channel): deliver Data, sent under ID, to the target
	// Values say, and answer with what came back.
	LinkCall = 1
	// LinkOpen (core → channel): open a bind-persistent connection to the
	// target Values say. Not built yet: a channel answers it with a
	// failure.
	LinkOpen = 2
	// LinkAnswer (channel → core): the answer to request Request.
	LinkAnswer = 3
)

// Kinds of LinkFailure: why a call reached nothing.
const (
	// LinkFailUnreachable: no connection (DNS, refused, no route).
	LinkFailUnreachable = 1
	// LinkFailTimeout: no answer within the deadline.
	LinkFailTimeout = 2
	// LinkFailTLS: the TLS handshake or the certificate failed.
	LinkFailTLS = 3
	// LinkFailOther: anything else (the target's values do not make a
	// request, a proxy failed, the channel draining).
	LinkFailOther = 4
)

var linkFailNames = map[int]string{
	LinkFailUnreachable: "unreachable", LinkFailTimeout: "timeout", LinkFailTLS: "tls", LinkFailOther: "other",
}

// LinkFailName is a failure kind's name, for people.
func LinkFailName(kind int) string {
	if n, ok := linkFailNames[kind]; ok {
		return n
	}
	return fmt.Sprintf("failure %d", kind)
}

// Limits of a link frame.
const (
	// MaxLinkFrameSize bounds a link frame: a minion message's sealed frame
	// with the values and room to spare.
	MaxLinkFrameSize = MaxMinionMessageSize + 128<<10
	// MaxLinkExcerpt bounds what an answer quotes of a response that is not
	// a minion message.
	MaxLinkExcerpt = 512
	// MaxLinkFailureMsg bounds a failure's message.
	MaxLinkFailureMsg = 512
)

// LinkFrame is one frame of the control link.
//
// A call carries Request, Values (the build connection's built-in values
// and the target's, by the keys the channel declared for the kind), ID (the
// id the message is sent under, 16 bytes), Data (the sealed message) and
// Deadline (milliseconds, optional: the channel keeps to the shorter of it
// and its own timeout).
//
// An answer carries Request and one of:
//   - ID and Data: what came back was a minion message in the channel's
//     transport format, unpacked: the id it is sent under and its sealed
//     frame, which core opens. Status is the transport's (the HTTP status).
//   - Failure: nothing came back.
//   - neither: something answered that is not a minion message (an HTTP
//     404, a WAF's page): Status, and Excerpt, the start of what came back.
type LinkFrame struct {
	Type     int            `cbor:"0,keyasint"`
	Request  uint64         `cbor:"1,keyasint"`
	Values   map[string]any `cbor:"2,keyasint,omitempty"`
	ID       []byte         `cbor:"3,keyasint,omitempty"`
	Data     []byte         `cbor:"4,keyasint,omitempty"`
	Deadline uint64         `cbor:"5,keyasint,omitempty"`
	Status   int            `cbor:"6,keyasint,omitempty"`
	Failure  *LinkFailure   `cbor:"7,keyasint,omitempty"`
	Excerpt  []byte         `cbor:"8,keyasint,omitempty"`
}

// LinkFailure is why a call reached nothing: Kind (LinkFailUnreachable,
// ...) and a message for people.
type LinkFailure struct {
	Kind int    `cbor:"1,keyasint"`
	Msg  string `cbor:"2,keyasint,omitempty"`
}

// EncodeLinkFrame encodes a control link frame.
func EncodeLinkFrame(f LinkFrame) ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	b, err := cborEnc.Marshal(f)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxLinkFrameSize {
		return nil, fmt.Errorf("link frame of %d bytes is over %d", len(b), MaxLinkFrameSize)
	}
	return b, nil
}

// DecodeLinkFrame decodes and checks a control link frame.
func DecodeLinkFrame(b []byte) (LinkFrame, error) {
	var f LinkFrame
	if len(b) > MaxLinkFrameSize {
		return f, fmt.Errorf("link frame of %d bytes is over %d", len(b), MaxLinkFrameSize)
	}
	if err := cborDec.Unmarshal(b, &f); err != nil {
		return LinkFrame{}, fmt.Errorf("link frame: %w", err)
	}
	return f, f.Validate()
}

// Validate checks a frame's keys against its type.
func (f LinkFrame) Validate() error {
	if f.Request == 0 {
		return errors.New("link frame: request is 1 or more")
	}
	switch f.Type {
	case LinkCall:
		switch {
		case len(f.ID) != 16:
			return errors.New("link call: the id is 16 bytes")
		case len(f.Data) == 0:
			return errors.New("link call: no message")
		case f.Status != 0 || f.Failure != nil || f.Excerpt != nil:
			return errors.New("link call: an answer's keys")
		}
	case LinkOpen:
		if f.ID != nil || f.Data != nil || f.Status != 0 || f.Failure != nil || f.Excerpt != nil {
			return errors.New("link open: only request, values and deadline")
		}
	case LinkAnswer:
		switch {
		case f.Values != nil || f.Deadline != 0:
			return errors.New("link answer: a request's keys")
		case (f.ID == nil) != (f.Data == nil):
			return errors.New("link answer: id and data come together")
		case f.ID != nil && len(f.ID) != 16:
			return errors.New("link answer: the id is 16 bytes")
		case f.ID != nil && (f.Failure != nil || f.Excerpt != nil):
			return errors.New("link answer: a message, a failure or an excerpt, one of them")
		case f.Failure != nil && (f.Excerpt != nil || f.Status != 0):
			return errors.New("link answer: a failure has no status or excerpt")
		case len(f.Excerpt) > MaxLinkExcerpt:
			return fmt.Errorf("link answer: the excerpt is %d bytes at most", MaxLinkExcerpt)
		case f.Status < 0 || f.Status > 999:
			return errors.New("link answer: status is 0 to 999")
		}
		if f.Failure != nil {
			if _, ok := linkFailNames[f.Failure.Kind]; !ok {
				return fmt.Errorf("link answer: failure kind %d", f.Failure.Kind)
			}
			if len(f.Failure.Msg) > MaxLinkFailureMsg || !utf8.ValidString(f.Failure.Msg) {
				return fmt.Errorf("link answer: a failure's message is UTF-8, %d bytes at most", MaxLinkFailureMsg)
			}
		}
	default:
		return fmt.Errorf("link frame: type %d", f.Type)
	}
	return nil
}

// Excerpt is the start of b, at most MaxLinkExcerpt bytes, for an answer.
func Excerpt(b []byte) []byte {
	if len(b) > MaxLinkExcerpt {
		b = b[:MaxLinkExcerpt]
	}
	return append([]byte{}, b...)
}
