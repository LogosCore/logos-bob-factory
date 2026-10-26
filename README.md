# logos-bob-factory

BOB (Body Opponent Bag, the human-shaped boxing dummy) is a bare Logos
factory, the module type that builds minions. It registers with core as
`factory` / `bob`, says hello, keeps heartbeating, and builds **dummy
minions**: its one target, `dummy`, produces a JSON description of the minion
(its id, values, connections and badge) with nothing to run. It is the dummy
the factory contracts are worked out against.

Built on [`logos-golang-factory-core`](https://github.com/logoscore/logos-golang-factory-core).

## Build schema

| | |
|---|---|
| Target | `dummy`: format `json`, platform `any/any` |
| Fields | `greeting` (string), `kill_date` (datetime), `punches` (enum_list: jab, cross, hook, uppercut), `badge` (file, an image up to 256 KiB, checked and recorded by SHA-256) |
| Channels | `http` connection schema version 2, with connection parameters `method` (GET, POST, PUT; default POST) and `path_prefix` (default `/`) |
| Crypto | none |

## Icon

BOB registers with its own icon, a boxing dummy kept as a constant in
`cmd/bob-factory/icon.go`; core's UI shows it instead of the default factory
icon.

## Management

BOB's module page in core has the Configuration, Actions, Activity and Logs
tabs. Next to the builds section every factory gets from factory-core
(`max_concurrent_builds`; `drain`, `resume`; build counters, the last failure
and `draining`), BOB declares:

| | |
|---|---|
| Properties (live) | `build_delay` (duration, 0s to 10m: each build takes punches that long first, so its progress can be watched and it can be cancelled), `fail_builds` (fail every build with code `knocked_out`), `log_level` |
| Actions | `say_hello`: BOB logs a hello at info, to check actions and the live log tail |

## Configuration

| Env | Default | |
|---|---|---|
| `FACTORY_ID` | `bob-local` | Instance id, unique per running process |
| `RABBITMQ_HOST` / `_PORT` / `_USER` / `_PASSWORD` / `_VHOST` | `localhost` / `5672` / `guest` / `guest` / `/` | Core's broker |
| `LOG_FORMAT` | `json` | `json` or `text`, on stderr |

## Run

```bash
go run ./cmd/bob-factory                 # against a local broker
docker compose up -d --build             # next to logos-core (set RABBITMQ_PASSWORD in .env)
```

## Test

```bash
go test ./...
```

`bob_test.go` runs the real builder in process with `factorytest`, through the
same request checks, job and result as a build core sends.
