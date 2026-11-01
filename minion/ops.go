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
	opConnect        = 12
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

	connectConn = 1
	connectFor  = 2
)
