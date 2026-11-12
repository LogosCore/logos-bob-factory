package main

// What the minion reads its config by (minion.cfg, docs: minion-build, The
// minion config): numbers alone, as it reads tasks by ops.go's. BOB's own
// are its build schema's (cmd/bob-factory/build.go: the channels, the build
// fields, the connection parameters); the channels' fields are their
// connection schemas' (the HTTP channel's version 3, the TCP channel's
// version 1), which a channel never gives another field. Both are checked
// by cmd/bob-factory/fields_test.go.

// BOB's numbers for the channels its minions speak.
const (
	chanHTTP uint64 = 1
	chanTCP  uint64 = 2
)

// BOB's build fields the minion reads.
const (
	fieldGreeting uint64 = 1
	fieldKillDate uint64 = 2
	fieldPunches  uint64 = 3
	fieldBadge    uint64 = 4 // a file: never in a config, numbered all the same
	fieldGoarm    uint64 = 5 // linux/arm's ARM version: the compiler's, never read
)

// BOB's connection parameters for the HTTP channel's kinds.
const (
	paramMethod     uint64 = 1
	paramPathPrefix uint64 = 2
)

// The HTTP channel's fields.
const (
	httpBaseURL            uint64 = 1
	httpExtraBaseURLs      uint64 = 2
	httpHostHeader         uint64 = 3
	httpSNI                uint64 = 4
	httpProxy              uint64 = 5
	httpTLSVerify          uint64 = 6
	httpTLSPinSHA256       uint64 = 7
	httpUserAgent          uint64 = 8
	httpHeaders            uint64 = 9
	httpRequestTimeout     uint64 = 10
	httpCheckinInterval    uint64 = 11
	httpCheckinJitter      uint64 = 12
	httpWSReconnectBackoff uint64 = 13
)

// The TCP channel's fields.
const (
	tcpHost             uint64 = 1
	tcpPort             uint64 = 2
	tcpConnectTimeout   uint64 = 3
	tcpKeepalive        uint64 = 4
	tcpReconnectBackoff uint64 = 5
	tcpRequestTimeout   uint64 = 6
	tcpCheckinInterval  uint64 = 7
	tcpCheckinJitter    uint64 = 8
	tcpListenPort       uint64 = 9
	tcpListenAddress    uint64 = 10
	tcpMaxConnections   uint64 = 11
	tcpProxy            uint64 = 12 // a target's: the channel's alone
)
