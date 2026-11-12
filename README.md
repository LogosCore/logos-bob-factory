# logos-bob-factory

BOB (Body Opponent Bag, the human-shaped boxing dummy) is a bare Logos
factory, the module type that builds minions. It registers with core as
`factory` / `bob`, says hello, keeps heartbeating, and builds **dummy
minions**: Linux and Windows programs that print the config they were built
with (core's minion config: the build id, keys, connections, commands and
values, numbered CBOR) when asked, and otherwise run as a minion that talks
to core. It is the dummy the factory
contracts are worked out against.

Built on [`logos-golang-factory-core`](https://github.com/logoscore/logos-golang-factory-core).

## Build schema

| | |
|---|---|
| Version | 6, of build schema version 3. Its minions read core's minion config (`minion_config`) by numbers alone, so its channels, fields and connection parameters are numbered (`minion/fields.go`); each build's minion exits with codes of its own (version 5); Linux on ARM (version 6) |
| Targets | `linux-elf`: format `elf`, platforms `linux/amd64`, `linux/386`, `linux/arm64` and `linux/arm`, with its own field `goarm` for `linux/arm` alone ([ARM](#arm)); the artifact is a [binary](#the-linux-elf-binary). `windows-exe`: format `exe`, platforms `windows/amd64`, `windows/386` and `windows/arm64`; the artifact is the same minion as a [Windows program](#the-windows-exe-program) |
| Fields | `greeting` (string), `kill_date` (datetime), `punches` (enum_list: jab, cross, hook, uppercut), `badge` (file, an image up to 256 KiB, checked and recorded by SHA-256) |
| Max tasks | Declared: a build says how many tasks its minion runs at once (default 5), and core changes it live from the session. |
| Compression | Declared: `deflate`. A build packs its messages unless the operator turns it off: the minion inflates what core sends packed, and packs each item it sends that is 256 bytes or more and shrinks an eighth (`minion/packing.go`; docs: wire format, Compression). |
| Feedback | The minion tells its operators what it would only log: check-ins that failed in a row, its WebSocket closing, a connection past its listener's limit, a persisted session not given back, a command that crashed (the task fails, the minion goes on). The `feedback` command (op 14) sends a warning or an error of your words. |
| Commands | `sleep` and `exit` (required); `hello`, `ls`, `switch` and `volumes` (picked by default); `download`, `stream_download`, `upload`, `stream_upload`, `rm`, `walk`, `shell`, `feedback`, and the tunnels and delegates below. Each declares the params a task of it takes, by number (`minion/ops.go`); the minion's description lists the ones its build picked. `volumes` (action `list_volumes`, result `volumes`) lists the host's file systems: on Linux those mounted where the minion sees them (`/proc/self/mountinfo`, less the kernel's own and the snaps), with their space (`statfs`), label (`/dev/disk/by-label`) and kind (removable by `/sys/class/block`); core's file browser runs it at a machine's roots. |
| Reverse delegate | `reverse_delegate` (op 19, action `reverse_delegate`, operator-picked: `bind` (0.0.0.0), `listen` (required), `kind` (`reverse-persistent` or `reverse-call`), `max_connections` (16)): while it runs, the minion listens as the TCP channel's reverse kinds do, and carries the minions that dial it: each connection a reverse-persistent one (its first frame naming the kind served and its address), or each a reverse call (a sync, core's reply written back, no reply for a refusal) (`minion/delegate_reverse.go`). The minions it carries are built with a tcp connection to its host's address and that port. |
| Delegate | `delegate` (op 18, action `delegate`, operator-picked; core starts one itself when a target that names the session needs it): while it runs, the minion serves the TCP channel's `bind-persistent` kind for its operation's minions. Core sends it a target's opens, it dials the target's `host` and `port` (the `listen_port` when none) within `connect_timeout` and the open's deadline (no `proxy`), and carries the frames both ways in delegate items, cut in pieces where a message has no room, until either side closes (`minion/delegate.go`). It holds no place in the max tasks; core stops it after `stop_idle` (1h) with nothing through it. |
| Tunnels | `tunnel` (op 15, action `tunnel`: `timeout`, 10s, `max_streams`, 256; opens `tcp`, `udp`, `unix`, `pipe` (Windows), `source`, `tls`, `process`, `accept` and `icmp`) and `reverse_tunnel` (op 17, action `reverse_tunnel`: `bind`, `listen`, `udp`, `timeout`, `max_streams`; opens `tcp` and `udp`) run until stopped and hold no place in its max tasks. Core adds its own params (`front`, `port`, `host`, `to`, `stop_idle`, `cap` and the rest: `protocol.TunnelCoreParams`) and puts the front in front: a SOCKS or HTTP proxy, a port forward, a terminal or the VPN, on one of its operational ports, each connection a tool makes through it a stream the minion connects from its host (a name resolved there), its bytes carried in the session's messages with offsets and a window each way (`minion/tunnel.go`). A process (the terminal's) runs in a PTY, a ConPTY on Windows, or on pipes, and its exit goes to core in a `tunnel_ctl` (`minion/proc.go`, `minion/tunnel_ctl.go`). An open it does not take as it is (a key this version does not know) it answers unsupported, never a plain connect (`protocol.ReadTunnel`). The minion checks in at once while it has tunnel items, and as core's `poll` says otherwise; it sends again from core's ack what goes unacked past the link's measured round trip (5s at least, a minute at most: `minion/rtt.go`), probes a window shut that long with one byte, and keeps core's data that comes ahead of where it is. A UDP association (SOCKS5 UDP ASSOCIATE) is a UDP socket of its own (`minion/tunnel_udp.go`): core's datagrams go out of it to the destinations they name (a name resolved there, cached a minute), what comes back goes to core with its source; no acks, a queue of 256 KiB, each datagram's number taken once. |
| Channels | `http` (numbered 1), connection schema version 3: `reverse-call` with the parameters `method` (GET, POST, PUT; default POST) and `path_prefix` (default `/`), and `reverse-persistent` (a WebSocket) with `path_prefix`. `tcp` (2), version 1: `bind-persistent` (the minion listens and the channel dials it), `reverse-persistent` and `reverse-call`, with the channel's own fields |
| Crypto | `aes-256-cbc-hmac-sha256` (AES-256-CBC with HMAC-SHA256); the keys core generates go into the artifact |
| Error codes | What the minion's exit codes mean (`error_codes`; docs: minion-build, Minion error codes): 1 could not start (for a reason without a code of its own), 2 crashed (Go's own: a panic no task recovered, or a fatal error of the runtime), 3 its config does not read, 4 built without core's keys, 5 no connection it can use. Core shows them on BOB's page and on each build's Overview. `minion/exits.go` holds them; the minion exits with one when it cannot start, and with 0 when an `exit` task ends it. Each build's minion exits with codes of its own (`error_codes_per_build`), drawn by core from 3 to 255, but for 1 and 2, which are fixed (Go exits with them itself): before it compiles, BOB writes the build's codes into its copy of `minion/exits.go`, and the build log names them ("exit codes of its own: 3→201, 4→87, 5→14"). The build's Overview lists them; Unique error codes off in the build form keeps the declared ones. |

## The linux-elf binary

For `linux-elf`, BOB compiles a real program: a static Linux binary (no cgo,
stripped). Run, it **stages** with core through its first HTTP connection, as
the [Minion Messages](https://logoscore.github.io/logos-docs/contracts/minion-messages/#staging)
contract says, says **hello** under the temporary id with the new keys (its
pid, OS and arch, hostname, user, executable, root or user, addresses, and
its check-in schedule, so core can tell when it is late),
which opens a session in core, then checks in under the session id with a
`get_tasks` for the room its max tasks leave, every check-in interval of the connection, spread by
its jitter (`checkin_interval`, `checkin_jitter`; the channel's 60s and 20%
when the endpoint has none). It logs each step to the console and runs until
stopped. A failed check-in (the channel out of reach, its 404, core
restarting) keeps the session; it starts over from the stage only when core
answers `session_closed`, after 24 hours of nothing but failures, or when the
stage or hello fails. Built with a persisted session ("Build with persisted
session" on a session in core's Sessions page), it claims that session in its
hello, so a restarted minion comes back to it; told `session_replaced` (another
process took it), it goes on in a session of its own and stops claiming. `./bob-<id>.bin describe`
prints the config it was built with instead, the bytes as they are. The
config is **baked in at compile time**: core makes it with the build request
(BOB declares `minion_config`), and the minion reads it by numbers alone
(`minion/fields.go`), so it carries no names of its settings. The minion's
sources are their own Go module, `minion/` in this repository. For each
build BOB copies that directory, writes the config into it as `minion.cfg`,
and runs
`go build` there for the build's platform (`GOOS=linux`, `GOARCH=amd64` or
`386`) with `-mod=readonly`. Its modules (the protocol package, the CBOR
library, coder/websocket) come from the module cache, which the image warms
when it is built, so a build needs no network; a module the cache lacks
comes through `GOPROXY`, checked against the minion's `go.sum` either way:
what compiles is what `go.sum` names. The plan is **Check the badge** (when the build has one), **Compile the
binary** and **Upload the artifact**. Each step writes the build log, which
core shows live, by step (factory-core's live build log): the badge's name,
size and check; the build, its exit codes, the command and everything the
compiler prints to stdout and stderr, line by line as it comes (`go build
-v`, so it names what it compiles, on success too), and the binary's size;
the artifact's name, size, media type and SHA-256 as it uploads. While it
compiles, the step's message counts the packages compiled so far ("go build
for linux/amd64: 12 packages compiled", those the build cache lacked). The
artifact is
`bob-<first 8 of the build id>.bin`, `application/x-executable`, about 1 MiB.

```console
$ ./bob-5b0c4f6e.bin
minion 00:15:11.276315 build 5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f
minion 00:15:11.276347 connection 1, http (POST http://localhost:8090/) of 1, check-in every 1s ± 20%
minion 00:15:11.338861 generated an RSA-2048 key pair in 62ms; public key sha256 39c61b8d6944f84a
minion 00:15:11.338884 correlation id 8cd63702…
minion 00:15:11.338931 message 2910971f… under 5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f: 342 bytes of CBOR, a 402-byte sealed frame, 419 bytes packed for the channel
minion 00:15:11.343012 channel answered 200 with 403 bytes in 4ms
minion 00:15:11.343047 opened core's reply: 322 bytes of CBOR, 1 item(s)
minion 00:15:11.343921 stage_response: correlation id matches
minion 00:15:11.343925 temporary id bb1779b3-9fcc-4639-8aa4-5d52c1f3b4e6
minion 00:15:11.343927 new keys decrypted with our private key: enc_key sha256 a621cf52…, mac_key sha256 d81c124c…
minion 00:15:11.343929 staged
minion 00:15:11.344102 hello: pid 2953236, linux/amd64, host "homez", user "user", integrity 6, addrs [192.168.0.124 …]
minion 00:15:11.344150 message 368b46b2… under bb1779b3-9fcc-4639-8aa4-5d52c1f3b4e6: 269 bytes of CBOR, a 322-byte sealed frame, 339 bytes packed for the channel
minion 00:15:11.345990 channel answered 200 with 115 bytes in 2ms
minion 00:15:11.346034 opened core's reply: 44 bytes of CBOR, 1 item(s)
minion 00:15:11.346057 session id 8fba1ac4-b8e7-46ca-b96e-bdddc9e52906
minion 00:15:11.346060 next check-in in 851ms
minion 00:15:12.198214 message c4dcd5dd… under 8fba1ac4-b8e7-46ca-b96e-bdddc9e52906: 28 bytes of CBOR, a 82-byte sealed frame, 99 bytes packed for the channel
minion 00:15:12.199823 channel answered 200 with 17 bytes in 2ms
minion 00:15:12.199841 check-in 1: get_tasks max 5, 1 item(s) sent, 0 back
minion 00:15:12.199842 next check-in in 1.033s
…

$ ./bob-5b0c4f6e.bin describe > minion.cfg   # core's minion config, numbered CBOR, as built in
```

So BOB needs the **Go toolchain and the minion's sources at run time**. Its
image is built on `golang:1.25-alpine`, keeps the sources in `/app/minion`,
runs as an unprivileged user, and warms its build cache with the standard
library for `linux/amd64`, `linux/386` and `windows/amd64`, so a compile takes
about a second. To work on the
minion, mount `./minion` over `/app/minion` (commented out in
`docker-compose.yml`): the next build compiles the edits without rebuilding
BOB. Run outside the image, BOB uses the `go` on its `PATH` and the sources in
`MINION_SRC`. Without a toolchain a `linux-elf` build fails with
`no_toolchain`, and one that does not compile with `compile_failed` (the
compiler's output is in the build log).

### ARM

`linux-elf` builds for `linux/arm64` and for 32-bit ARM, `linux/arm`. A
`linux/arm` build says which ARM it runs on with `goarm` (num 5), a field of
that platform alone ([Fields for some platforms](https://logoscore.github.io/logos-docs/contracts/minion-build/#fields-for-some-platforms)):
the build form shows it once `linux/arm` is picked, and core takes it for no
other platform. BOB compiles with it as `GOARM`:

| `goarm` | For |
|---|---|
| `7` (default) | ARMv7 and newer in 32-bit mode: most boards and phones since 2011 |
| `6` | ARMv6: the first Raspberry Pis and the Pi Zero |
| `5` | ARMv5, with no floating-point unit: floating point in software, slower |

The build log's command line names it (`GOOS=linux GOARCH=arm GOARM=6
CGO_ENABLED=0 go build ...`), and so does the binary's build info
(`go version -m`). The image's build cache holds no ARM standard library: a
first build for each compiles it, kept for the next.

## The windows-exe program

For `windows-exe`, BOB compiles the same minion for Windows (`GOOS=windows`,
`GOARCH=amd64`, `386` or `arm64`): a console program, its artifact
`bob-<first 8 of the build id>.exe`,
`application/vnd.microsoft.portable-executable`. It does everything the Linux
one does, with what Windows has:

- **Shell**: `cmd.exe` (`%ComSpec%`), its command line passed as typed
  (`/d /s /c "<command>"`: no AutoRun commands), no window of its own; a
  cancel or the timeout ends the whole tree it started (`taskkill /T /F`).
- **Machine id**: the firmware's SMBIOS system UUID (`GetSystemFirmwareTable`,
  which any user may call: `smbios-uuid`), else the install's `MachineGuid`
  from the registry's 64-bit view (`win-machine-guid`), else the host name;
  nothing run, nothing written.
- **Hello**: its integrity level from its token (low, medium, high, system)
  and the host's domain or workgroup (`NetGetJoinInformation`).
- **Listings**: files with the hidden attribute are hidden (a walk told to
  leave hidden folders out leaves them out), and an entry's mode is its file
  attributes, which core shows as PowerShell does.
- **Volumes**: its drives, `A:` to `Z:` (`GetLogicalDrives`), its mapped
  network drives with them (their share, `WNetGetConnection`): each one's
  kind (`GetDriveType`), label, file system and read-only flag
  (`GetVolumeInformation`), and size and the space free to its user
  (`GetDiskFreeSpaceEx`). An empty DVD drive or card reader says "no media",
  without Windows asking anyone to insert a disk (`SetThreadErrorMode`).

The image's build cache holds the standard library for `windows/amd64`;
`windows/386` and `windows/arm64` compile it on their first build, kept for
the next.

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
| `LOGOS_CORE_BASE_URL` | `http://localhost:8002` | Core's address (its front, wherever BOB runs): builds fetch their files from it and upload the binary to it. Over HTTPS to a self-signed core, `LOGOS_CORE_CA_FILE`, `LOGOS_CORE_CERT_SHA256` or `LOGOS_CORE_TLS_INSECURE` ([How modules reach core](https://logoscore.github.io/logos-docs/core-infrastructure/#how-modules-reach-core)) |
| `RABBITMQ_HOST` / `_PORT` / `_USER` / `_VHOST` | `localhost` / `5672` / `logos` / `/` | Core's broker (logos-core's dev stack, from the host) |
| `RABBITMQ_PASSWORD` | none: required | Its password, logos-core's `RABBITMQ_DEFAULT_PASS` |
| `LOG_FORMAT` | `json` | `json` or `text`, on stderr |
| `MINION_SRC` | `/app/minion` | The minion module compiled for `linux-elf` builds; `./minion` when running from the repository |
| `GOPROXY` | `https://proxy.golang.org,direct` | Where a module the image's cache lacks comes from: a mirror (Nexus, Artifactory, Athens) on a network without the internet |
| `GOSUMDB` | `sum.golang.org` | The checksum database; `off` behind a mirror that does not proxy it (`go.sum` still checks every module) |

## Run

```bash
RABBITMQ_PASSWORD=… MINION_SRC=./minion go run ./cmd/bob-factory   # on the host, against logos-core's dev stack
docker compose up -d --build             # next to logos-core (set RABBITMQ_PASSWORD in .env)
```

## Test

```bash
go test ./...                                   # the factory (the compile tests need go on PATH)
(cd minion && go vet ./... && go test ./...)    # the minion, a module of its own
(cd minion && GOOS=windows go vet ./... && GOOS=windows GOARCH=386 go vet ./...)
```

`bob_test.go` runs the real builder in process with `factorytest`, through the
same request checks, job and result as a build core sends; `stage_test.go`
runs a compiled minion against a fake channel and core. `go test ./...` at the
root does not reach `minion/`: test it in its own directory.
