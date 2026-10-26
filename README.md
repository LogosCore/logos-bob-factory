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

## Configuration

| Env | Default | |
|---|---|---|
| `FACTORY_ID` | `bob-local` | Instance id, unique per running process |
| `RABBITMQ_HOST` / `_PORT` / `_USER` / `_PASSWORD` / `_VHOST` | `localhost` / `5672` / `guest` / `guest` / `/` | Core's broker |

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
