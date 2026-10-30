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
stripped). Run, it **stages** with core through its first HTTP connection, as
the [Minion Messages](https://logoscore.github.io/logos-docs/contracts/minion-messages/#staging)
contract says, says **hello** under the temporary id with the new keys (its
pid, OS and arch, hostname, user, executable, root or user, and addresses),
which opens a session in core, then checks in under the session id with a
`get_tasks` for one task every check-in interval of the connection, spread by
its jitter (`checkin_interval`, `checkin_jitter`; the channel's 60s and 20%
when the endpoint has none). It logs each step to the console and runs until
stopped. A failed check-in (the channel out of reach, its 404, core
restarting) keeps the session; it starts over from the stage only when core
answers `session_closed`, after 24 hours of nothing but failures, or when the
stage or hello fails. `./bob-<id>.bin describe`
prints the JSON description it was built with instead. The
description is **baked in at compile time**. The minion's sources are their
own Go module, `minion/` in this repository. For each build BOB copies that
directory, writes the build's description into it as `minion.json`, and runs
`go build` there for the build's platform (`GOOS=linux`, `GOARCH=amd64` or
`386`), with no network: a `vendor/` directory, once the minion has
dependencies, is used as it is: the minion vendors the protocol package (its sealed frame, CBOR messages and stage and hello items) and the CBOR library. The plan is **Check the badge** (when the build has one), **Compile the
binary** and **Upload the artifact**. The compile step runs the command, and
everything the compiler prints to stdout and stderr goes to the build log as
it comes (`go build -v`, so it names what it compiles), on success too. The
artifact is
`bob-<first 8 of the build id>.bin`, `application/x-executable`, about 1 MiB.

```console
$ ./bob-5b0c4f6e.bin
minion 00:15:11.276315 build 160a41b2-e202-4841-9766-5dedfb861947, crypto aes-256-cbc-hmac-sha256
minion 00:15:11.276347 channel http "localhost-8090" (POST http://localhost:8090/), check-in every 1s ± 20%
minion 00:15:11.338861 generated an RSA-2048 key pair in 62ms; public key sha256 39c61b8d6944f84a
minion 00:15:11.338884 correlation id 8cd63702…
minion 00:15:11.338931 message 2910971f… under 160a41b2-e202-4841-9766-5dedfb861947: 363 bytes of CBOR, a 418-byte sealed frame, 435 bytes packed for the channel
minion 00:15:11.343012 channel answered 200 with 435 bytes in 4ms
minion 00:15:11.343047 opened core's reply: 356 bytes of CBOR, 1 item(s)
minion 00:15:11.343921 stage_response: correlation id matches
minion 00:15:11.343925 temporary id bb1779b3-9fcc-4639-8aa4-5d52c1f3b4e6
minion 00:15:11.343927 new keys decrypted with our private key: enc_key sha256 a621cf52…, mac_key sha256 d81c124c…
minion 00:15:11.343929 staged
minion 00:15:11.344102 hello: pid 2953236, linux/amd64, host "homez", user "user", integrity user, addrs [192.168.0.124 …]
minion 00:15:11.344150 message 368b46b2… under bb1779b3-9fcc-4639-8aa4-5d52c1f3b4e6: 620 bytes of CBOR, a 674-byte sealed frame, 691 bytes packed for the channel
minion 00:15:11.345990 channel answered 200 with 147 bytes in 2ms
minion 00:15:11.346034 opened core's reply: 70 bytes of CBOR, 1 item(s)
minion 00:15:11.346057 session id 8fba1ac4-b8e7-46ca-b96e-bdddc9e52906
minion 00:15:11.346060 next check-in in 851ms
minion 00:15:12.198214 message c4dcd5dd… under 8fba1ac4-b8e7-46ca-b96e-bdddc9e52906: 37 bytes of CBOR, a 98-byte sealed frame, 115 bytes packed for the channel
minion 00:15:12.199823 channel answered 200 with 17 bytes in 2ms
minion 00:15:12.199841 check-in 1: get_tasks max 1, 0 item(s) back (tasks are not built yet)
minion 00:15:12.199842 next check-in in 1.033s
…

$ ./bob-5b0c4f6e.bin describe
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
| Properties (live) | `fail_builds` (fail every build with code `knocked_out`), `log_level` |
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
