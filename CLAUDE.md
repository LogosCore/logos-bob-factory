# CLAUDE.md

BOB (`github.com/logoscore/logos-bob-factory`): a bare Logos factory (the module type that builds minions). `cmd/bob-factory/main.go` runs factory-core's `factory.Run` as module_name `bob`, with its own management and the live log tail (module-core `logging.New` teed into a `logstream.Hub`). `build.go` holds its build schema (one `dummy` target; one field of each build field type; HTTP connection parameters `method` and `path_prefix`) and the builder, which uploads a JSON description of the minion as the artifact. `management.go` is BOB's own manifest: `build_delay`, `fail_builds` and `log_level` (live), and the `say_hello` action; factory-core adds the builds section (`max_concurrent_builds`, `drain`/`resume`, build counters) next to it, so BOB's manifest cannot use those keys.

Factory plumbing (build requests, progress, results, files, artifacts) stays in `logos-golang-factory-core`; module plumbing in `logos-golang-module-core`. BOB only declares what it builds and builds it.

Build and test: `go build ./... && go vet ./... && go test ./...`. Go 1.25.7. Config is env only (see README). Builder tests use `factorytest.Build`, which runs the real builder through the SDK in process.
