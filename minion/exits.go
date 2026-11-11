package main

// The minion's exit codes, as BOB's build schema declares them (error_codes,
// cmd/bob-factory/build.go: exits_test.go there checks the two agree). The
// minion exits with one when it cannot start: once running, it goes on until
// stopped, and the exit command ends it with 0.
const (
	// exitFailed: any other failure to start; the log says which.
	exitFailed = 1
	// exitCrashed is Go's own: a panic no task recovered, or a fatal error
	// of the runtime. Declared for an operator to look up; the minion never
	// exits with it itself.
	exitCrashed = 2
	// exitConfig: the config built in does not read.
	exitConfig = 3
	// exitNoKeys: the config has no payload keys.
	exitNoKeys = 4
	// exitNoConnection: none of the config's connections is one the minion
	// speaks.
	exitNoConnection = 5
)
