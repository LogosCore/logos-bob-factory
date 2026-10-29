package protocol

import "net/http"

// The data-plane sync call: a channel posts a minion's ciphertext to core as
// the raw HTTP body, with the rest of the message in headers, and core answers
// the same way (200 with ciphertext for the minion, or 204 with nothing).
const (
	// SyncPath is core's sync endpoint, under core's base URL.
	SyncPath = "/api/channel/sync"
	// SyncContentType is the content type of a request or reply body.
	SyncContentType = "application/octet-stream"

	HeaderMessageID       = "Logos-Message-Id"
	HeaderBuildID         = "Logos-Build-Id"
	HeaderChannelInstance = "Logos-Channel-Instance"
	// HeaderTraceParent is W3C Trace Context's header.
	HeaderTraceParent = "Traceparent"
)

// InboundMinionMessage is one minion message a channel relays to core.
type InboundMinionMessage struct {
	// MessageID is unique per request, a UUIDv4 (NewID). Core dedups on it.
	MessageID string
	// BuildID is the id of the build that made the minion.
	BuildID string
	// ChannelInstance is the sending channel's registered instance id.
	ChannelInstance string
	// TraceParent is an optional W3C traceparent value.
	TraceParent string
	// Data is the minion's ciphertext, as the minion sent it once the channel
	// has undone its own transport encoding. Never empty.
	Data []byte
}

// OutboundMinionMessage is core's reply for one inbound message.
type OutboundMinionMessage struct {
	// MessageID is core's own id for the reply, a UUIDv4.
	MessageID string
	// BuildID is the inbound message's BuildID.
	BuildID string
	// TraceParent echoes the inbound message's, when it had one.
	TraceParent string
	// Data is ciphertext for the minion. Empty when core has nothing for it,
	// sent as 204 No Content.
	Data []byte
}

// SetHeaders writes m's header fields to h.
func (m InboundMinionMessage) SetHeaders(h http.Header) {
	h.Set("Content-Type", SyncContentType)
	h.Set(HeaderMessageID, m.MessageID)
	h.Set(HeaderBuildID, m.BuildID)
	h.Set(HeaderChannelInstance, m.ChannelInstance)
	if m.TraceParent != "" {
		h.Set(HeaderTraceParent, m.TraceParent)
	}
}

// InboundFromHeaders reads an inbound message's header fields from h. Data
// is the caller's to fill from the body.
func InboundFromHeaders(h http.Header) InboundMinionMessage {
	return InboundMinionMessage{
		MessageID:       h.Get(HeaderMessageID),
		BuildID:         h.Get(HeaderBuildID),
		ChannelInstance: h.Get(HeaderChannelInstance),
		TraceParent:     h.Get(HeaderTraceParent),
	}
}

// SetHeaders writes m's header fields to h, with the content type when m has
// data.
func (m OutboundMinionMessage) SetHeaders(h http.Header) {
	if len(m.Data) > 0 {
		h.Set("Content-Type", SyncContentType)
	}
	h.Set(HeaderMessageID, m.MessageID)
	h.Set(HeaderBuildID, m.BuildID)
	if m.TraceParent != "" {
		h.Set(HeaderTraceParent, m.TraceParent)
	}
}

// OutboundFromHeaders reads a reply's header fields from h. Data is the
// caller's to fill from the body.
func OutboundFromHeaders(h http.Header) OutboundMinionMessage {
	return OutboundMinionMessage{
		MessageID:   h.Get(HeaderMessageID),
		BuildID:     h.Get(HeaderBuildID),
		TraceParent: h.Get(HeaderTraceParent),
	}
}
