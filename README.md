# logos-bob-factory

BOB (Body Opponent Bag, the human-shaped boxing dummy) is a bare Logos
factory, the module type that builds minions. It registers with core as
`factory` / `bob`, says hello, keeps heartbeating, and builds **dummy
minions**: a JSON description of the minion (its build id, commands, values,
connections and badge) with nothing to run. It is the dummy the factory
contracts are worked out against.

Built on [`logos-golang-factory-core`](https://github.com/logoscore/logos-golang-factory-core).

## Build schema

| | |
|---|---|
| Targets | `dummy` and `dummy-lite` (no shell): format `json`, platform `any/any`, the artifact is the JSON description; `linux-elf`: format `elf`, platforms `linux/amd64` and `linux/386`, the artifact is a [binary](#the-linux-elf-binary) that prints it |
| Fields | `greeting` (string), `kill_date` (datetime), `punches` (enum_list: jab, cross, hook, uppercut), `badge` (file, an image up to 256 KiB, checked and recorded by SHA-256) |
| Commands | `sleep` and `exit` (required), `hello` and `ls` (picked by default), `download`, `shell` (the `dummy` and `linux-elf` targets only). Each declares the params a task of it would take; the dummy minion runs none of them, and its description lists the ones its build picked. |
| Channels | `http` connection schema version 2, with connection parameters `method` (GET, POST, PUT; default POST) and `path_prefix` (default `/`) |
| Crypto | `aes-256-cbc-hmac-sha256` (AES-256-CBC with HMAC-SHA256); the keys core generates go into the artifact |

## The linux-elf binary

For `linux-elf`, BOB compiles a real program: a static Linux binary (no cgo,
stripped) that prints the dummy minion's JSON description and exits. The
description is **baked in at compile time**: BOB writes it next to the
program's source (`cmd/bob-factory/stub`, embedded in BOB) and runs
`go build` for the build's platform, with `GOOS=linux` and `GOARCH=amd64` or
`386`. The plan gets a **Compile the binary** step, and the artifact is
`bob-<build id>`, `application/x-executable`, about 1 MiB.

```console
$ ./bob-5b0c4f6e
{
  "format": "logos.bob-dummy.v1",
  "build_id": "5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f",
  "target": "linux-elf",
  "platform": "linux/amd64",
  …
}
```

So BOB needs the **Go toolchain at run time**. Its image is built on
`golang:1.25-alpine`, runs as an unprivileged user, and warms its build cache
with the standard library for both platforms, so a compile takes about a
second. Run outside the image, BOB uses the `go` on its `PATH`; without one a
`linux-elf` build fails with `no_toolchain`, and one that does not compile
with `compile_failed` (the compiler's output is in the build log).

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
