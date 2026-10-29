package protocol

import (
	"fmt"
	"regexp"
	"time"
)

// This file defines live log streaming: core asks a module (on its rpc_queue)
// to stream its log records for a limited time, and the module publishes them
// in batches to LogsExchange with routing key = stream id. Core relays them
// to whoever is watching and stores nothing. A module advertises support with
// CapabilityLogs in its management manifest.
//
// The stream is leased: the module stops by itself once the lease expires, so
// a core that crashes or loses the viewer never leaves a stream running.
// Core renews it by sending mgmt.logs.start again with the same stream id.

// CapabilityLogs marks a module that serves mgmt.logs.start / mgmt.logs.stop.
const CapabilityLogs = "logs"

// Core → module log streaming RPC operation types.
const (
	TypeMgmtLogsStart = "mgmt.logs.start"
	TypeMgmtLogsStop  = "mgmt.logs.stop"
)

// LogsExchange is the direct exchange modules publish LogBatch messages to,
// with routing key = stream id.
const LogsExchange = "logos.module.logs"

// LogLevel is a log record's severity.
type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// Rank orders levels (debug lowest); an unknown level ranks as info.
func (l LogLevel) Rank() int {
	switch l {
	case LogDebug:
		return 0
	case LogWarn:
		return 2
	case LogError:
		return 3
	default:
		return 1
	}
}

// Valid reports whether l is a known level.
func (l LogLevel) Valid() bool {
	switch l {
	case LogDebug, LogInfo, LogWarn, LogError:
		return true
	}
	return false
}

// Log streaming limits.
const (
	DefaultLogLeaseSeconds = 30
	MaxLogLeaseSeconds     = 120
	MaxLogBacklog          = 1000
	MaxLogMessageBytes     = 8 << 10
	// MaxLogValueBytes caps one attr value (a string, or the JSON of a
	// structured value); longer ones are truncated.
	MaxLogValueBytes   = 4 << 10
	MaxLogAttrs        = 128
	MaxLogBatchRecords = 500
)

// Stream end reasons (LogBatch.EndReason).
const (
	LogEndStopped      = "stopped"       // mgmt.logs.stop
	LogEndLeaseExpired = "lease_expired" // core stopped renewing
	LogEndShutdown     = "shutdown"      // the module is going away
	LogEndReplaced     = "replaced"      // a newer start took the stream slot
)

var streamIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// LogsStartRequest is the mgmt.logs.start payload. Sending it again for an
// active StreamID renews the lease (and may change MinLevel) without a new
// backlog.
type LogsStartRequest struct {
	StreamID string `json:"stream_id"`
	// MinLevel filters at the source; empty means info.
	MinLevel LogLevel `json:"min_level,omitempty"`
	// LeaseSeconds is how long the module streams without a renewal; 0 means
	// DefaultLogLeaseSeconds. Capped at MaxLogLeaseSeconds.
	LeaseSeconds int `json:"lease_seconds,omitempty"`
	// Backlog is how many recent records (at or above MinLevel) to send first;
	// capped at MaxLogBacklog and by what the module kept.
	Backlog int    `json:"backlog,omitempty"`
	Actor   *Actor `json:"actor,omitempty"`
}

// Validate checks a start request's fields.
func (r LogsStartRequest) Validate() error {
	if !streamIDPattern.MatchString(r.StreamID) {
		return fmt.Errorf("stream_id must match %s", streamIDPattern)
	}
	if r.MinLevel != "" && !r.MinLevel.Valid() {
		return fmt.Errorf("unknown min_level %q", r.MinLevel)
	}
	if r.LeaseSeconds < 0 || r.LeaseSeconds > MaxLogLeaseSeconds {
		return fmt.Errorf("lease_seconds must be 0..%d", MaxLogLeaseSeconds)
	}
	if r.Backlog < 0 || r.Backlog > MaxLogBacklog {
		return fmt.Errorf("backlog must be 0..%d", MaxLogBacklog)
	}
	return nil
}

// LogsStartReply is the mgmt.logs.start reply payload.
type LogsStartReply struct {
	StreamID     string `json:"stream_id"`
	LeaseSeconds int    `json:"lease_seconds"` // the granted lease
}

// LogsStopRequest is the mgmt.logs.stop payload. Stopping an unknown or
// already ended stream succeeds.
type LogsStopRequest struct {
	StreamID string `json:"stream_id"`
}

// LogRecord is one log line. Attrs is the module's own structure, nested
// objects included; core and the UI show it as is and never interpret keys.
// MaxLogAttrs bounds the number of leaf values.
type LogRecord struct {
	Time    time.Time      `json:"time"`
	Level   LogLevel       `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// LogBatch is what a module publishes to LogsExchange. Seq increases by one
// per batch of a stream, so a gap means batches were lost in transit. Seq
// starts again at 1 when the module recreates the stream (a start for an id
// it no longer has: after a reconnect or an ended lease).
// Dropped counts records the module discarded (rate limit or slow publish)
// since the previous batch. The last batch of a stream has End set.
type LogBatch struct {
	StreamID string      `json:"stream_id"`
	Instance string      `json:"instance"`
	Seq      int64       `json:"seq"`
	Backlog  bool        `json:"backlog,omitempty"` // records predate the start
	Records  []LogRecord `json:"records,omitempty"`
	Dropped  int64       `json:"dropped,omitempty"`
	End      bool        `json:"end,omitempty"`
	// EndReason is one of the LogEnd* constants when End is set.
	EndReason string `json:"end_reason,omitempty"`
}
