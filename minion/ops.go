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
	opSocksProxy     = 15
	opPortForward    = 16
	// opReversePortForward listens on the minion's host: its streams are
	// the minion's (tunnel.go).
	opReversePortForward = 17
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

	socksProxyPort       = 1
	socksProxyTimeout    = 2
	socksProxyMaxStreams = 3

	// A forward's host and to are core's: each stream's tunnel item names
	// the destination.
	portForwardPort       = 1
	portForwardHost       = 2
	portForwardTo         = 3
	portForwardTimeout    = 4
	portForwardMaxStreams = 5

	// A reverse forward's bind and listen are the minion's: where it
	// listens. Its destination (host, to) is core's alone: those params
	// carry no num, and never come.
	reversePortForwardBind       = 1
	reversePortForwardListen     = 2
	reversePortForwardTimeout    = 3
	reversePortForwardMaxStreams = 4
	// reversePortForwardUDP: listen for datagrams, a stream for each
	// address they come from.
	reversePortForwardUDP = 5
)
