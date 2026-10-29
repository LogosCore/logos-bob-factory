// Package protocol defines canonical channel<->Logos message contracts for Logos.
//
// It is the single source of truth shared by core and all modules, covering both
// planes:
//
//   - data-plane: the HTTP sync call, a minion's ciphertext as the raw body
//     with the message's ids in headers (messages.go)
//   - control-plane: the AMQP envelope used by RPC requests, RPC replies, and
//     events between core and modules (envelope.go)
//
// It provides:
//   - the sync call's message structs, headers and their validation
//   - the shared control-plane Envelope / ReplyEnvelope and helpers
//   - constants for canonical type, version, status, and error-code fields
//   - validation helpers with typed, machine-readable errors
//   - what modules declare at registration and the operations that follow:
//     the management manifest and its RPC payloads (management.go), live log
//     streaming (logs.go), a channel's minion connection schema
//     (connection.go), and a factory's build schema (build.go) with the build
//     operations (build_ops.go). They share one field description, FieldSpec,
//     and one validator for its values.
package protocol
