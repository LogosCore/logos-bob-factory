# CLAUDE.md

BOB (`github.com/logoscore/logos-bob-factory`): a bare Logos factory (the module type that builds minions). `cmd/bob-factory/main.go` runs module-core's `module.Run` as module_type `factory`, module_name `bob`. For now it only registers, logs a hello and heartbeats: no management, no builds.

Control-plane plumbing stays in `logos-golang-module-core`; factory-specific SDK code (build contracts) will go in a future factory SDK, not here.

Build and test: `go build ./... && go vet ./...`. Go 1.25.7. Config is env only (see README).
