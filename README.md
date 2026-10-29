# logos-bob-factory

BOB (Body Opponent Bag, the human-shaped boxing dummy) is a bare Logos
factory, the module type that builds minions. It registers with core as
`factory` / `bob`, says hello, keeps heartbeating, and builds **dummy
minions**: Linux binaries that print a JSON description of the minion (its
build id, commands, values, connections and badge) and do nothing else. It is the dummy the factory
contracts are worked out against.

Built on [`logos-golang-factory-core`](https://github.com/logoscore/logos-golang-factory-core).

## Build schema

| | |
|---|---|
| Version | 2 (version 1 also had the JSON-only targets `dummy` and `dummy-lite`) |
| Targets | `linux-elf`: format `elf`, platforms `linux/amd64` and `linux/386`; the artifact is a [binary](#the-linux-elf-binary) that prints the description |
| Fields | `greeting` (string), `kill_date` (datetime), `punches` (enum_list: jab, cross, hook, uppercut), `badge` (file, an image up to 256 KiB, checked and recorded by SHA-256) |
| Commands | `sleep` and `exit` (required), `hello` and `ls` (picked by default), `download`, `shell`. Each declares the params a task of it would take; the dummy minion runs none of them, and its description lists the ones its build picked. |
| Channels | `http` connection schema version 2, with connection parameters `method` (GET, POST, PUT; default POST) and `path_prefix` (default `/`) |
| Crypto | `aes-256-cbc-hmac-sha256` (AES-256-CBC with HMAC-SHA256); the keys core generates go into the artifact |

## The linux-elf binary

For `linux-elf`, BOB compiles a real program: a static Linux binary (no cgo,
stripped) that prints the dummy minion's JSON description and exits. The
description is **baked in at compile time**. The minion's sources are their
own Go module, `minion/` in this repository. For each build BOB copies that
directory, writes the build's description into it as `minion.json`, and runs
`go build` there for the build's platform (`GOOS=linux`, `GOARCH=amd64` or
`386`), with no network: a `vendor/` directory, once the minion has
dependencies, is used as it is. The plan gets a **Compile the binary** step, and the artifact is
`bob-<first 8 of the build id>.bin`, `application/x-executable`, about 1 MiB.

```console
$ ./bob-5b0c4f6e.bin
{
  "format": "logos.bob-dummy.v1",
  "build_id": "5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f",
  "target": "linux-elf",
  "platform": "linux/amd64",
  …
}
```

So BOB needs the **Go toolchain and the minion's sources at run time**. Its
image is built on `golang:1.25-alpine`, keeps the sources in `/app/minion`,
runs as an unprivileged user, and warms its build cache with the standard
library for both platforms, so a compile takes about a second. To work on the
minion, mount `./minion` over `/app/minion` (commented out in
`docker-compose.yml`): the next build compiles the edits without rebuilding
BOB. Run outside the image, BOB uses the `go` on its `PATH` and the sources in
`MINION_SRC`. Without a toolchain a `linux-elf` build fails with
`no_toolchain`, and one that does not compile with `compile_failed` (the
compiler's output is in the build log).

## Icon

BOB registers with its own icon, a boxing dummy kept as a constant in
`cmd/bob-factory/icon.go`; core's UI shows it instead of the default factory
icon. The icon is the lit skin of a shaded picture, which reads right in a
light colour on dark themes, so BOB also registers `iconLight`, the same
picture drawn for light themes, with the outline, lines and shading as the
shape.

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
| `MINION_SRC` | `/app/minion` | The minion module compiled for `linux-elf` builds; `./minion` when running from the repository |

## Run

```bash
MINION_SRC=./minion go run ./cmd/bob-factory   # against a local broker
docker compose up -d --build             # next to logos-core (set RABBITMQ_PASSWORD in .env)
```

## Test

```bash
go test ./...
```

`bob_test.go` runs the real builder in process with `factorytest`, through the
same request checks, job and result as a build core sends.
