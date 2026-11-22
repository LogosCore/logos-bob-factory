# logos-bob-factory

BOB (Body Opponent Bag, the human-shaped boxing dummy) is a bare Logos
factory, the module type that builds minions. It registers with core as
`factory` / `bob`, says hello, keeps heartbeating, and builds **dummy
minions**: Linux and Windows programs that print the config they were built
with (core's minion config: the build id, keys, connections, commands and
values, numbered CBOR) when asked, and otherwise run as a minion that talks
to core. It is the dummy the factory
contracts are worked out against. Its minion speaks the HTTP channel's
`reverse-call` and `reverse-persistent` and the TCP channel's three kinds:
those channels are run end to end with it.

Built on [`logos-golang-factory-core`](https://github.com/logoscore/logos-golang-factory-core).
The Logos docs say what any factory and minion do ([Writing a
Factory](https://logoscore.github.io/logos-docs/guides/writing-a-factory/),
[Writing a Minion](https://logoscore.github.io/logos-docs/guides/writing-a-minion/));
this README says what BOB and its minion do.

## Build schema

| | |
|---|---|
| Version | 6, of build schema version 3. Its minions read core's minion config (`minion_config`) by numbers alone, so its channels, fields and connection parameters are numbered (`minion/fields.go`); each build's minion exits with codes of its own (version 5); Linux on ARM (version 6) |
| Targets | `linux-elf`: format `elf`, platforms `linux/amd64`, `linux/386`, `linux/arm64` and `linux/arm`, with its own field `goarm` for `linux/arm` alone ([ARM](#arm)); the artifact is a [binary](#the-linux-elf-binary). `windows-exe`: format `exe`, platforms `windows/amd64`, `windows/386` and `windows/arm64`; the artifact is the same minion as a [Windows program](#the-windows-exe-program) |
| Fields | `greeting` (string, num 1: what the `hello` command answers), `kill_date` (datetime, 2), `punches` (enum_list: jab, cross, hook, uppercut; 3), `badge` (file, 4: an image up to 256 KiB, which BOB fetches and checks by its size and SHA-256 before it compiles; it never goes into the minion). `goarm` (5) is `linux/arm`'s alone ([ARM](#arm)) |
| Max tasks | Declared: a build says how many tasks its minion runs at once (default 5), and core changes it live from the session. |
| Compression | Declared: `deflate`. A build packs its messages unless the operator turns it off: the minion inflates what core sends packed, and packs each item it sends that is 256 bytes or more and shrinks an eighth (`minion/packing.go`; docs: wire format, Compression). A tunnel stream's data that did not shrink three items in a row (bytes encrypted already: TLS, SSH, RDP) goes unpacked for its next MiB, then packing is tried again, as core does. |
| Persisted sessions | Declared ([The linux-elf binary](#the-linux-elf-binary)). |
| Failover | Declared ([Failover](#failover)). |
| Feedback | The minion tells its operators what it would only log ([Feedback](https://logoscore.github.io/logos-docs/contracts/minion-messages/sessions/#feedback)): check-ins that failed two or more in a row (`checkin_failed`, said once one works again, with the first error), its WebSocket closing when it did not close it (`ws_closed`), a connection to its TCP listener past `max_connections` (`tcp_refused`), a persisted session core did not give back (`session_not_claimed`), and a command that crashed (`task_panic`, an error: the task fails, the minion goes on). It sends them first in its next message, at once, folds one like a queued one into its count, and keeps at most 100 unsent (past that the oldest go, and a `feedback_dropped` warning says how many). The `feedback` command (op 14) sends a warning or an error of your words. |
| Commands | `sleep` and `exit` (required); `hello`, `ls`, `switch_connection`, `edit_connection`, `add_connection`, `remove_connection`, `volumes` and `ps` (picked by default); `download`, `stream_download`, `upload`, `stream_upload`, `rm`, `walk`, `shell`, `screenshot`, `kill`, `feedback`, `tunnel`, `reverse_tunnel`, `delegate` and `reverse_delegate` ([Commands](#commands)). Each declares the params a task of it takes, by number (`minion/ops.go`); the minion's description lists the ones its build picked. |
| Factory commands | `build_info` and `session_info`, which BOB runs itself, never the minion, in every session of its builds ([Factory commands](#factory-commands)) |
| Channels | `http` (numbered 1), connection schema version 3: `reverse-call` with the parameters `method` (GET, POST, PUT; default POST) and `path_prefix` (default `/`), and `reverse-persistent` (a WebSocket) with `path_prefix`. `tcp` (2), version 1: `bind-persistent` (the minion listens and the channel dials it), `reverse-persistent` and `reverse-call`, with the channel's own fields |
| Crypto | `aes-256-cbc-hmac-sha256` (AES-256-CBC with HMAC-SHA256); the keys core generates go into the artifact |
| Failure codes | Why a build failed: `badge_unreadable` (the badge does not read, or not as it was uploaded), `no_toolchain` and `compile_failed` ([The linux-elf binary](#the-linux-elf-binary)), `knocked_out` (`fail_builds`: [Management](#management)) |
| Error codes | What the minion's exit codes mean (`error_codes`; docs: minion-build, Minion error codes): 1 could not start (for a reason without a code of its own), 2 crashed (Go's own: a panic no task recovered, or a fatal error of the runtime), 3 its config does not read, 4 built without core's keys, 5 no connection it can use. Core shows them on BOB's page and on each build's Overview. `minion/exits.go` holds them; the minion exits with one when it cannot start, and with 0 when an `exit` task ends it. Each build's minion exits with codes of its own (`error_codes_per_build`), drawn by core from 3 to 255, but for 1 and 2, which are fixed (Go exits with them itself): before it compiles, BOB writes the build's codes into its copy of `minion/exits.go`, and the build log names them ("exit codes of its own: 3→201, 4→87, 5→14"). The build's Overview lists them; Unique error codes off in the build form keeps the declared ones. |

## Commands

| Op | Command | Action, result | Params: num (default) |
|---|---|---|---|
| 1 | `sleep` | | `interval` 1 (required), `jitter` 2 (10 %) |
| 2 | `exit` | | |
| 3 | `hello` | | |
| 4 | `ls` | `list_dir`, `dir_listing` | `path` 1 (`.`) |
| 5 | `download` | `download_file`, `file_download` | `path` 1 (required) |
| 6 | `stream_download` | `stream_file`, `file_stream` | `path` 1 (required) |
| 7 | `upload` | `upload_file`, `file_upload` | `file` 1 (required), `path` 2 (required), `overwrite` 3 (false) |
| 8 | `stream_upload` | `upload_stream`, `file_upload` | `stream` 1, `path` 2 (required), `overwrite` 3 (false) |
| 9 | `rm` | `delete_path` | `path` 1 (required), `recursive` 2 (false) |
| 10 | `walk` | `walk_dir`, `dir_walk` | `path` 1 (required), `depth` 2 (5), `max_folders` 3 (10000), `hidden` 4 (true), `one_fs` 5 (true) |
| 11 | `shell` | | `command` 1 (required), `timeout` 2 (30s) |
| 12 | `switch` | legacy (`switch_connection`'s old id) | `conn` 1 (required), `for` 2 (0s) |
| 13 | `volumes` | `list_volumes`, `volumes` | |
| 14 | `feedback` | | `level` 1 (warning), `message` 2 (required) |
| 15 | `tunnel` | `tunnel` | `timeout` 2 (10s), `max_streams` 3 (256) |
| 17 | `reverse_tunnel` | `reverse_tunnel` | `bind` 1 (0.0.0.0), `listen` 2 (required), `udp` 5 (false), `timeout` 3 (10s), `max_streams` 4 (256) |
| 18 | `delegate` | `delegate` | |
| 19 | `reverse_delegate` | `reverse_delegate` | `bind` 1 (0.0.0.0), `listen` 2 (required), `kind` 3 (`reverse-persistent`), `max_connections` 4 (16) |
| 20 | `screenshot` | result `screenshot` | `screen` 1 (0), `format` 2 (png), `quality` 3 (85, JPEG's), `x_display` 4 |
| 21 | `edit_connection` | `edit_connection` | `conn` 1 (required), `connection` 2 (required, structured) |
| 22 | `switch_connection` | `switch_connection` | `conn` 1 (required), `for` 2 (0s) |
| 23 | `add_connection` | `add_connection` | `connection` 1 (required, structured), `switch` 2 (false) |
| 24 | `remove_connection` | `remove_connection` | `conn` 1 (required) |
| 25 | `ps` | `list_processes`, `process_list` | |
| 26 | `kill` | `kill_process` | `pid` 1 (required), `started` 2 (0), `force` 3 (false) |

Op 16 (a port forward's, before the fronts were core's) is never used
again. `switch` (op 12) is `switch_connection` as it was named before:
a legacy command, kept for the builds that have it, whose minions run op
12; new builds get `switch_connection` (op 22), and new minions have no op
12. The tunnels' `timeout` and `max_streams`, `reverse_delegate`'s
`max_connections` and `screenshot`'s `x_display` are under the build
form's More options (`advanced`). How core runs each action and shows each
result is the docs' [Task Results and
Actions](https://logoscore.github.io/logos-docs/contracts/task-results-and-actions/).

### Shell, files and listings

- **`shell`** runs its command line in the system shell: `/bin/sh -c` in a
  process group of its own on Unix (`cmd.exe` on Windows: [The windows-exe
  program](#the-windows-exe-program)). A cancel or the timeout kills all of
  it, what it started too.
- **`ls`** sends a big directory in pieces of 500 entries, several to a
  result part while the message has room; a piece is cut sooner when its
  entries would not fit half a message.
- **`rm`** removes a file, a link (never what it points to) or a folder,
  one that is not empty only with `recursive`; never a filesystem's root
  (`/`, `C:\`), whatever `recursive` says.
- **`walk`** lists a folder and those under it breadth first (the top
  levels first), each a listing into core's file browser as it is read. It
  follows no link, enters no folder on another file system (`one_fs`) and,
  with `hidden` off, no hidden one; `depth` and `max_folders` limit it (0:
  no limit). It says where it stands (a `dir_walk`) every 2 seconds and at
  its end, and reads on only while fewer than 16 of its pieces wait to be
  sent: it goes as fast as the connection takes it, and leaves room for the
  other tasks.
- **`volumes`** lists the host's file systems: on Linux those mounted where
  the minion sees them (`/proc/self/mountinfo`, less the kernel's own and
  the snaps), with their space (`statfs`), label (`/dev/disk/by-label`) and
  kind (removable by `/sys/class/block`); on Windows its drives ([The
  windows-exe program](#the-windows-exe-program)). It sends them in parts of
  half a message each; core's file browser runs it at a machine's roots.
- **`screenshot`** takes a picture of the host's screen: `screen` 0 every
  screen in one picture, 1 the main one, then the others in the host's
  order; `format` PNG or JPEG (`quality`). On Linux an X display's
  (`minion/x11.go`: the X protocol over the display's socket, the user's
  MIT-MAGIC-COOKIE-1, RandR 1.5 for the monitors; `x_display`, else
  `DISPLAY`, else `:0`; Wayland gives none); on Windows the desktop of its
  session ([The windows-exe program](#the-windows-exe-program)). It goes to
  core as a download's file does, with a name and no path.

### Processes

- **`ps`** lists the host's processes. On Linux from `/proc`, those of the
  minion's PID namespace (a container's own): each one's name (its `comm`,
  whole from its executable or first arg when the kernel cut it at 15
  bytes), parent, state, threads, start (the boot time, `btime`, and its
  start in clock ticks, `AT_CLKTCK`), CPU time, resident memory, who it
  runs as (its effective uid: `root` or `user` its integrity), its command
  line (its args joined by spaces) and its executable (another user's only
  for root). No session, and no architecture: an executable's would take
  opening it, which can hang on a network file system. On Windows every
  session's ([The windows-exe program](#the-windows-exe-program)). It sends
  them in parts of half a message each, a command line cut to half a part;
  core shows them as a tree, the minion's own marked.
- **`kill`** ends a process: given `started` (as a list said it), only the
  one with that pid that started then, to the second; 0: whatever has the
  pid. `force` sends SIGKILL, else SIGTERM (which a process may catch, or
  ignore); on Linux 5.3 and later through a pidfd opened before the check,
  so a pid reused between the check and the signal is never signalled. The
  output says what it did, and whether the process ended within 2 seconds.
  Its own process the minion ends as `exit` ends it: the tasks beside it
  stopped first, then the task's end, and it exits once core has it.

### Tunnels

`tunnel` and `reverse_tunnel` run until stopped and hold no place in the max
tasks ([Tunnels](https://logoscore.github.io/logos-docs/contracts/minion-messages/tunnels/)).
`tunnel` opens `tcp`, `udp`, `unix`, `pipe`, `source`, `tls`, `process`,
`accept` and `icmp` (a `linux-elf` build all but `pipe`, Windows' named
pipes); `reverse_tunnel` opens `tcp` and `udp`. Core adds its own params
(`front`, `port`, `host`, `to`, `stop_idle`, `cap` and the rest:
`protocol.TunnelCoreParams`) and puts the front in front: a SOCKS or HTTP
proxy, a port forward, a terminal, a desktop or the VPN, on one of its
operational ports. `tunnel`'s `timeout` applies with every front but the
terminal, which connects to nothing.

- **Streams.** Each connection a tool makes through the front is a stream
  the minion connects from its host (a name resolved there) within
  `timeout`, its bytes carried in the session's messages with offsets and a
  window each way (`minion/tunnel.go`). Bytes core sends with an open,
  before the minion's reply, are written once it is connected. An open it
  does not take as it is (a key this version does not know) it answers
  unsupported, never a plain connect (`protocol.ReadTunnel`).
- **Timing that follows the link** (`minion/rtt.go`). What goes unacked
  past the session's measured round trip and four times its spread (RFC
  6298, Karn's rule; 5 s at least, a minute at most) goes again from core's
  ack, a window shut that long is probed with one byte, and core's data
  that comes ahead of where the stream is waits for the gap to fill. The
  room the minion gives core on a stream doubles while core fills it and
  the host keeps up, to 4 MiB a stream (32 MiB of growth over all of a
  tunnel's streams), and it reads up to 4 MiB of a host ahead of core's
  ack. It checks in at once while it has tunnel items, and as core's `poll`
  says otherwise. Over a connection kept open a stream's items wait on no
  round trip: what core pushes for it reaches its host as it comes, and
  what the host answers goes in a side message while the session's own
  message waits for its reply ([Connections](#connections)).
- **What a stream did** (`minion/tunnel_stats.go`): its bytes each way,
  what it sent again and its probes, and the most it had in flight, of
  window, read ahead of core's ack and kept ahead of a gap. A stream that
  sent again or probed, ran a minute or more, or moved a MiB says it in the
  log when it goes, and a tunnel's last line ("the tunnel closes") adds up
  all of its streams. Nothing of it goes to core.
- **Processes** (the terminal front: `minion/proc.go`,
  `minion/tunnel_ctl.go`): its program in a PTY, a ConPTY on Windows, or on
  pipes where it has no terminal to give; its exit goes to core in a
  `tunnel_ctl` before the stream's end (128 and the signal's number on
  Unix). A signal is SIGINT, SIGTERM or SIGKILL to the terminal's foreground
  job (what its shell runs), or to the process's group on pipes. On Windows
  an interrupt is a Ctrl-C typed into the console, terminate and kill end
  the process's whole tree (`taskkill /T`), and a process on pipes has no
  console to interrupt.
- **UDP.** A UDP association (SOCKS5 UDP ASSOCIATE) is a UDP socket of its
  own (`minion/tunnel_udp.go`): core's datagrams go out of it to the
  destinations they name (a name resolved there, cached a minute), what
  comes back goes to core with its source; no acks, a queue of 256 KiB, each
  datagram's number taken once.
- **Pings** (the VPN front's `icmp`, `minion/tunnel_icmp.go`): echo requests
  to any address, from an unprivileged ICMP socket where the host allows one
  (Linux's ping sockets), else a raw one when the minion runs as root, else
  none (the open is denied); on Windows `IcmpSendEcho`, any user, IPv4
  alone. Errors come back where its socket delivers them: a raw socket's and
  Windows', never Linux's ping sockets'.

### Delegates

`delegate` and `reverse_delegate` run until stopped and hold no place in the
max tasks ([Delegation](https://logoscore.github.io/logos-docs/contracts/minion-messages/delegation/)).

- **`delegate`** serves the TCP channel's `bind-persistent` kind for its
  operation's minions while it runs; core starts one itself when a target
  that names the session needs it. Core sends it a target's opens, it dials
  the target's `host` and `port` (the `listen_port` when none) within
  `connect_timeout` and the open's deadline (no `proxy`), and carries the
  frames both ways in delegate items, cut in pieces where a message has no
  room, until either side closes (`minion/delegate.go`). Core stops it after
  `stop_idle` (1h) with nothing through it.
- **`reverse_delegate`** listens as the TCP channel's reverse kinds do, and
  carries the minions that dial it: each connection a reverse-persistent one
  (its first frame naming the kind served and its address), or each a
  reverse call (a sync, core's reply written back, no reply for a refusal)
  (`minion/delegate_reverse.go`). The minions it carries are built with a
  tcp connection to its host's address and that port.

## Factory commands

What BOB runs itself, never the minion ([Factory
Commands](https://logoscore.github.io/logos-docs/contracts/factory-commands/)):
every session of its builds has them, whatever the build picked, and they
answer while the minion is out of reach, since core sends their tasks to a
BOB instance, not to the minion. Adding one needs no new schema version.

| Command | Params | Does |
|---|---|---|
| `build_info` | | Prints the session's build as core keeps it (`cmd/bob-factory/build_info.go`) |
| `session_info` | | Prints the session as core keeps it (`cmd/bob-factory/session_info.go`) |

Each prints, in sections of aligned rows, what core gave BOB with the task
(`cmd/bob-factory/factory_commands.go` writes the rows). Times are UTC
(2026.11.22 10:00:15 UTC).

**`build_info`** prints the session's build:

- the build's id and the session's number; its target (by label),
  platform, description, schema version (and BOB's, once it moved on), who
  started it and when, when it started and ended, the instance and BOB
  version that built it, its artifact (name, size, type, SHA-256) and its
  warnings;
- its values by the schema's labels: a file by its name, size and type, a
  credential by its type and name;
- its commands, and each connection with its channel, kind, the operators'
  note, the endpoint's params and BOB's values;
- its failover policy, and the exit codes its minion exits with;
- what the minion was built with: the keys' suite, the minion config's
  size, compression, tasks at once, a persisted session;
- the credentials it carries, by id, type, name and revision.

It prints none of what the call carries for the task alone: not the keys,
not the minion config (it holds them), not a credential's values, nor any
link.

**`session_info`** prints the session:

- its number and id, its build (target by label, platform), its notes, when
  it opened (under its build's id, or after a stage) and was last seen, its
  check-in schedule and when it turns late and lost, whether a persistent
  connection serves it, whether it is persisted (and its restarts), archived
  or in a folder, and its keys' suite;
- its host as the minion's hello said: host name, user and domain with the
  integrity level, OS and architecture, process and PID, minion version,
  machine id, addresses (and the one shown), round trip, the hello's other
  values, and who its tasks run as when not the process's own;
- the external addresses it was seen from (now, then each with its count
  and times), and the delegates its messages come through;
- the connection it uses (its place, name and kind, of how many), the moves
  the minion reported, and the connections it holds when its hello said
  them: the channel's params named as the channel numbers them (the HTTP
  channel's version 3, the TCP channel's version 1; another version's by
  number) and BOB's values by their labels;
- tasks at once, as the minion said and the operators' limit; unread
  errors and when feedback last came; the latest restarts.

It never prints the session's keys.

## The linux-elf binary

For `linux-elf`, BOB compiles a real program: a static Linux binary (no cgo,
stripped). Run, it **stages** with core through its first HTTP connection, as
the [Sessions](https://logoscore.github.io/logos-docs/contracts/minion-messages/sessions/#staging)
contract says, says **hello** under the temporary id with the new keys (its
pid, OS and arch, hostname, user, executable, root or user, addresses, its
machine id, its check-in schedule, so core can tell when it is late, and
the stage's round trip, `roundtrip_time`),
which opens a session in core, then checks in under the session id with a
`get_tasks` for the room its max tasks leave, every check-in interval of the connection, spread by
its jitter (`checkin_interval`, `checkin_jitter`; the channel's 60s and 20%
when the endpoint has none). It logs each step to the console and runs until
stopped. A failed check-in (the channel out of reach, its 404, core
restarting) keeps the session; it starts over from the stage only when core
answers `session_closed`, after 24 hours of nothing but failures, or when the
stage or hello fails. Starting over, it waits a check-in interval (a second
at least) and stages anew with a new key pair: a stage or hello whose reply
never came is not sent again, so the session a lost hello opened is left to
close after a day without a check-in. Built with a persisted session ("Build with persisted
session" on a session in core's Sessions page), it claims that session in its
hello, so a restarted minion comes back to it; told `session_replaced` (another
process took it), it goes on in a session of its own and stops claiming. `./bob-<id>.bin describe`
prints the config it was built with instead, the bytes as they are. The
config is **baked in at compile time**: core makes it with the build request
(BOB declares `minion_config`), and the minion embeds it (`//go:embed
minion.cfg`) and reads it with `protocol.ReadMinionConfig`, by numbers alone
(`minion/fields.go`), so it carries no names of its settings. The minion's
sources are their own Go module, `minion/` in this repository. For each
build BOB copies that directory, writes the config into it as `minion.cfg`,
and runs
`go build` there for the build's platform (`GOOS=linux`, `GOARCH` `amd64`,
`386`, `arm64` or `arm`): `-trimpath`, `-mod=readonly`, stripped
(`-ldflags=-s -w -buildid=`), with `CGO_ENABLED=0`, `GOWORK=off` and
`GOTOOLCHAIN=local`. Its modules (the protocol package, the CBOR
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
A build that ends well carries one warning: "a dummy minion, made to try
the build flow end to end".

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

Its machine id, on Linux: a container's id when it runs in one (the image's
machine-id is every container's), else the install's (`/etc/machine-id`,
`/var/lib/dbus/machine-id`), else the host name, else a random id for the
process. Not the firmware's UUID, which only root reads: the same machine
gets the same id whoever runs the minion, and the minion writes nothing on
the host ([Machine
identity](https://logoscore.github.io/logos-docs/contracts/minion-messages/sessions/#machine-identity)).

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
- **Screenshots**: the desktop of the session the minion runs in, copied by
  GDI (`BitBlt` with `CAPTUREBLT`, `GetDIBits`), its own pixels (DPI aware,
  per monitor where Windows has it), the monitors from `EnumDisplayMonitors`
  (named `\\.\DISPLAY1`). A minion run as a service, in session 0, has no
  desktop to take, and says so.
- **Process lists**: every session's, from one `NtQuerySystemInformation`
  (`SystemProcessInformation`, as Task Manager reads them: each one's name,
  parent, session, threads, start, CPU time and working set, without
  opening any), then what opening each gives
  (`PROCESS_QUERY_LIMITED_INFORMATION`: an administrator's for nearly every
  process, a user's for their own): its executable
  (`QueryFullProcessImageName`), command line (`NtQueryInformationProcess`),
  user and integrity level (its token), and `386` (or `arm`) for one WOW64
  runs. No state: Windows' processes have none of their own. `kill` opens
  the process, checks when it started (`GetProcessTimes`) and ends it with
  `TerminateProcess` through the same handle, `force` or not.

The image's build cache holds the standard library for `windows/amd64`;
`windows/386` and `windows/arm64` compile it on their first build, kept for
the next.

## The minion at work

What it does once it runs, on either target.

### Tasks

Each task runs in a goroutine of its own, as many at once as its max tasks
say: its `get_tasks` asks for the room the limit leaves through a call (none
when full), and for the limit itself over a persistent connection (a
standing allowance). A limit core changes live (its `max_tasks` item) the
minion says back in its next message. Its tasks' parts share each message:
output, data, chunks and listing
pieces take two thirds of what the connection carries (`max_message`), 512
KiB at most, and the rest goes in the next check-ins, which come at once.

- **A task runs once.** A task id it saw in the last 24 hours is acked again
  and not run again, so a core message sent to it again runs nothing twice
  ([Freshness and
  replay](https://logoscore.github.io/logos-docs/contracts/minion-messages/wire-format/#freshness-and-replay)).
  Copies that came before its next message get one ack.
- **Acks.** A task is acked in the minion's next message. A tunnel's, a
  delegate's and a switch's ack goes at once: core opens their streams, and
  the move goes, once it has it. Any other's waits until the task ends, 50
  ms at most (`ackHold` in `minion/tasks.go`), so a quick command's ack,
  output and end go in one message; a cancel, or a file the task sends or
  receives, sends it sooner.
- **`exit`** cancels the tasks running beside it and waits up to 10 s for
  their ends, each sent cancelled before exit's own.
- **Core's questions.** A `reack` (which of the session's tasks it still
  has, asked after a claim and over each new listening connection) gets an
  `ack` naming those it runs, or ended with an end not yet sent, and
  `unknown_task` for each other. A `rehello` gets a `hello` in its next
  message, through the connection in use; it sends none on its own. Items it
  does not take get `unknown_item` (`resume` among them: it keeps its own
  state).

### Connections

The minion holds all of its build's connections (`minion/conns.go`) and uses
one at a time: at start the first it can use, in the build's order (an
`http` one of a reverse kind, or a `tcp` one: a listening one only when it
comes first), and its hello says which (`conn`, its place from 1). The
session, keys and schedule stay the same across moves.

**Editing** a connection (`edit_connection`, core's action of the name:
`minion/edit.go`) applies what core sends, what changed of the connection
at that place (a patch: its params and values that changed, `null` for one
gone, its name, failover or `max_message`; the connection whole when its
channel or kind changes), the others kept in their order. The minion says
its connections again, whole, with a `connection_update` in the message
that ends the task, whose output says what changed ("connection 2: kind,
params 1 changed"). A channel BOB does not speak, or a kind it does not
use (`bind-call`; `http`'s `bind-persistent`), fails the task, and nothing
changes. The connection in
use goes on with the new settings from the next exchange, which carries
the update: the WebSocket or TCP connection it keeps open closes and the
next exchange opens one by the new settings; a listener listens where the
new settings say and waits for core's dial there, and a switch waiting for
a dial on the edited connection gives up. The minion knows no connection
names but the ones an edit gives: the others go unnamed, and core keeps
the names it has. An edit lasts while the process runs: a session it
starts over in says the edited connections in its hello, and a restarted
minion has its build's again (it says them when it claims a persisted
session, which may hold an edited list).

**Adding** a connection (`add_connection`, `minion/edit.go`) puts the one
core sends, whole, after the minion's others, at most 32, and says them
again with a `connection_update`; the task's output says what it added
("connection 3: ws, http reverse-persistent"). It stays on the connection
it uses, unless the task's `switch` is set: it then moves there as
`switch_connection` does, and the task ends with how that went. Like an
edit, the connection lasts while the process runs.

**Removing** one (`remove_connection`, `minion/edit.go`) takes the
connection at `conn` out, never the one in use or the only one; the ones
after it move up a place, and with them what holds a place: the one in
use, a switch's listener or its wait for core's dial, a move back owed,
failover's last try. Its `connection_update` says the place it took out
(`removed`), and the task's output what it removed ("connection 1: front,
http reverse-call removed"). Like an edit, the removal lasts while the
process runs.

Each exchange that gets its answer, whichever connection carried it, notes
its round trip (`minion/roundtrip.go`): from sending the message to core's
reply to it, or the channel's empty answer. A hello says the latest
(`roundtrip_time`, in milliseconds): the stage's, or for a rehello the last
check-in's.

- **Requests** (`http`, `reverse-call`): a request for each exchange, by its
  `method` and `path_prefix`, every check-in interval.
- **A WebSocket** (`http`, `reverse-persistent`: `minion/ws.go`): every
  exchange goes over it, and core pushes tasks and cancels within the
  `get_tasks` allowance. The minion checks in at once after its hello and
  after each reconnect, which links the WebSocket to its session and sets
  the allowance, then not on a schedule while it stays open: a message goes
  only when there is something in it (output, a pushed task's ack:
  [Tasks](#tasks)), and it pings the channel every 30 s to find a link that
  died silently. One that closes
  opens again after 1 s, doubling up to the endpoint's
  `ws_reconnect_backoff` (60 s by default). The channel closes it when its
  own socket to core closes, which is what makes a schedule unnecessary.
- **Side messages** (`minion/side.go`), over a connection kept open (a
  WebSocket, the TCP channel's `reverse-persistent` and `bind-persistent`):
  the session's messages go one at a time, each waiting for its reply, but
  meanwhile the tunnels' and delegates' items go in side messages of their
  own, as many waiting as there is to send (the streams' windows bound the
  bytes on their way), each within a message's room, gathering what comes
  over 5 ms (a stream's ack and its host's answer in one). They go
  only over a socket whose first message core answered (it ties the socket
  to the session); one whose reply never comes (the socket closed) goes
  again over the next socket, as it was, under a new id; and a move to
  another connection waits for their replies first. What core pushes for a
  tunnel or a delegate is taken as it comes, in the connection's reader;
  the rest of a push is taken by the session, those that came before a
  reply before it (a task pushed, then its cancel in a reply). Through
  requests (`reverse-call`) one message goes at a time.
- **The TCP channel** (`minion/tcp.go`, `minion/tcp_reverse.go`): frames
  are `[length: 4 bytes, big-endian][id: 16 bytes][sealed]`.
  `bind-persistent` makes the minion listen (`listen_port`,
  `listen_address`, `max_connections`, `keepalive`): the channel dials it
  when core opens the target's connection, and the newest connection in is
  the one used (the one before is likely stale). `reverse-persistent` is a
  connection it dials and keeps open as it keeps a WebSocket
  (`reconnect_backoff`), `reverse-call` a connection for each exchange.
- **What core pushes** over a kept connection queues without a limit
  (`minion/pushqueue.go`; core bounds it: the task allowance, each stream's
  window), so the connection's reader never waits on the session.
- **A connection that fails a write**, or gives no reply within the request
  timeout (`request_timeout`, 40 s by default), is dropped: the next
  exchange dials, opens or waits for a dial again.

**Switching** (`switch_connection`): the minion checks in through the new connection
with a `conn_update` and moves only if that answers (over a WebSocket, that
check-in also sets the allowance, so pushes start at once); otherwise the
task fails and it stays where it was. With `for`, it moves back when the
time is up ("back after 10m"). A switch to a listening connection opens its
port and says so in the task (an early part: core's cue to dial the
session's target), then moves through core's dial; with no dial in 10
minutes it closes the port and fails. Moving off a listening connection
closes its port.

### Failover

The build's failover policy comes in its config
(`protocol.DefaultMinionFailover` when it has none), and
`minion/failover.go` acts on it
([Failover](https://logoscore.github.io/logos-docs/contracts/channel-core-connections/#failover)):
`fail_after` failed exchanges in a row on the connection in use (its own
`fail_after` when it has one), over at least `fail_for`, move the minion to
the next connection it may use (each connection's `auto`), in the build's
order, round the list with `wrap`; when a whole round failed it pauses
`cycle_pause` first. Away from its first connection by failover, it tries
that one again after `failback_after`. A move goes as a switch does, with a
`conn_update` saying why; an operator's switch resets it. Failover passes a
listening connection by, whatever its `auto` says: that one waits for core's
dial, which a switch asks for.

## The minion's sources

`minion/` is a Go module of its own, which needs the protocol package
(`logos-golang-protocol`), the CBOR library and coder/websocket, nothing
else. Its runner and workings are its own: minions share only the protocol.

| Files | What |
|---|---|
| `main.go` | The config read (`//go:embed minion.cfg`, `protocol.ReadMinionConfig`), staging, the `hello`, the session loop, switching, starting over, the backstop |
| `fields.go`, `ops.go`, `exits.go` | The numbers it reads its config, its tasks and its exit codes by, checked against the schema by the factory's tests |
| `conns.go`, `ws.go`, `tcp.go`, `tcp_reverse.go`, `failover.go`, `pushqueue.go`, `side.go`, `edit.go`, `roundtrip.go` | Its connections: HTTP requests and a WebSocket, the TCP channel's three kinds, failover, what core pushes, side messages, edits, the round trip its hello says |
| `tasks.go` | The runner: core's items dispatched, `get_tasks` within its limit, acks, result parts, cancels, `reack`, `max_tasks` |
| `packing.go`, `feedback.go` | Compression; feedback |
| `machine*.go`, `host_*.go`, `integrity.go`, `smbios.go` | The `hello`: the machine id, the host, the integrity level |
| `download.go`, `upload.go` | File transfers both ways |
| `walk.go`, `volumes*.go`, `rm.go`, `shell*.go`, `paths*.go`, `mode_*.go`, `owners_*.go` | Its commands' work on the host: paths as the host writes them, hidden files, owners |
| `screenshot*.go`, `x11.go` | Screenshots: GDI on Windows, the X protocol elsewhere |
| `ps*.go` | Processes and `kill`: `/proc` and pidfds on Linux, `NtQuerySystemInformation` and each process's handle on Windows |
| `tunnel*.go`, `proc*.go`, `pipe_*.go`, `icmp_*.go`, `rtt.go` | Tunnels, terminals, named pipes, pings, resend timing that follows the link, and what each stream did (`tunnel_stats.go`) |
| `delegate.go`, `delegate_reverse.go` | Delegation |

## Icon

BOB registers with its own icon, a boxing dummy kept as a constant in
`cmd/bob-factory/icon.go`; core's UI shows it instead of the default factory
icon. The icon is the lit skin of a shaded picture, which reads right in a
light colour on dark themes, so BOB also registers `iconLight`, the same
picture drawn for light themes, with the outline, lines and shading as the
shape.

## Management

BOB's module page in core has the Configuration, Actions, Activity and Logs
tabs (Logs is core's live tail of BOB's log: BOB declares the `logs`
capability). Next to the builds section every factory gets from factory-core
(`max_concurrent_builds`, which BOB starts at 4; `drain`, `resume`; build
counters, the last failure and `draining`), BOB declares:

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

`docker-compose.yml` joins logos-core's network (`logos-core_logos`) and
reaches core as `logos-core-dev:8002` and RabbitMQ as `logos-rabbitmq`. Once
BOB's log says `control plane: registered with core`, core's **Modules**
lists it with its targets, build parameters, commands and minion error
codes. To try a minion, run a channel too (logos-http-channel's dev stack
listens on the host's port 8090), then **Builds → New build**: a target and
platform, commands, values, and a connection through the channel (`base_url`
`http://localhost:8090` for a minion run on the host). Download the
artifact, run it where it reaches the channel, and its session appears under
**Sessions**.

## Build a saved request without core

A development build of BOB (`-tags dev`) has a `build` command that compiles
the minion of a saved `factory.build` request: the JSON core's UI downloads
for a build (`<id>.factory-build.json`), the exact bytes the factory
received. No core, RabbitMQ or running factory is needed, and the request's
deadline and upload link are not used, so an old file works. It checks the
request against the build schema, then runs the same compile as a real build
(config baked in as `minion.cfg`, the build's exit codes in `exits.go`):

```bash
go run -tags dev ./cmd/bob-factory build [-src ./minion] [-o outdir] x.factory-build.json
```

The artifact is `bob-<first 8 of the build id>.bin` or `.exe` in `-o`
(default `.`). The command is in `cmd/bob-factory/devbuild_dev.go` behind the
`dev` tag; the image builds without it, so its binary has no such command.
The file holds the minion's keys: keep it as private as the artifact.

## Developing against an unreleased protocol

While BOB or its minion needs a protocol change not yet tagged, they build
against a checkout of `logos-golang-protocol` beside this one ([Changing the
Protocol](https://logoscore.github.io/logos-docs/contributing/changing-the-protocol/)).
BOB compiles its minion for each build from a copy of `minion/` in a scratch
folder, where no workspace applies and no relative path resolves, so:

- `go.work` at the root uses `./minion` too: go commands run in `minion/`
  refuse a workspace that leaves its module out.

  ```
  go 1.25.7

  use (
  	.
  	./minion
  	../logos-golang-protocol
  )
  ```

- `minion/go.mod` replaces the protocol with the checkout, by its absolute
  path:

  ```
  replace github.com/logoscore/logos-golang-protocol => /path/to/logos-golang-protocol
  ```

- The image builds from tags alone (`.dockerignore` keeps a workspace out),
  so BOB's binary is built on the host, and a `docker-compose.override.yml`
  mounts it, the minion's sources, and the checkout at that same absolute
  path:

  ```bash
  CGO_ENABLED=0 GOOS=linux go build -o .dev-bin/bob-factory ./cmd/bob-factory
  ```

  ```yaml
  services:
    bob-factory:
      volumes:
        - ./.dev-bin/bob-factory:/app/bob-factory:ro
        - ./minion:/app/minion:ro
        - /path/to/logos-golang-protocol:/path/to/logos-golang-protocol:ro
  ```

  Then `docker compose up -d`, without `--build`; after each change, build
  the binary again and `docker compose restart`.

- An unreleased factory-core goes in the workspace too
  (`../logos-golang-factory-core`). A change to the factory alone (a
  factory command, say) needs the binary's mount alone: the image's minion
  sources build with the protocol they name.

None of it is committed: `.gitignore` lists `go.work`, `go.work.sum`,
`docker-compose.override.yml` and `.dev-bin/`, and the `replace` is dropped
by hand before a release. Tagged with it in place, the image cannot compile
a minion ("replacement directory … does not exist"), and every build fails.

## Test

```bash
go build ./... && go vet ./... && go test ./... && golangci-lint run ./...   # the factory (the compile tests need go on PATH)
cd minion   # the minion, a module of its own: go test ./... at the root does not reach it
go build ./... && go vet ./... && go test ./... && golangci-lint run ./...
GOOS=windows go vet ./... && GOOS=windows GOARCH=386 go vet ./... && GOARCH=386 go vet ./...
GOOS=windows golangci-lint run ./... && GOOS=windows GOARCH=386 golangci-lint run ./...
```

Nothing in the tests runs on Windows or on 32 bits, so the minion is vetted
and linted for them. A few tests are Windows' alone (`ps_windows_test.go`:
`SYSTEM_PROCESS_INFORMATION`'s layout on 64 and 32 bits, the system's
processes, WOW64): compiled there with `GOOS=windows go test -c`, for
`amd64` and `386`, they run on any Windows. The factory's tests, in `cmd/bob-factory`:

| Test | Checks |
|---|---|
| `build_test.go` | The schema valid as core checks it, the commands, the targets and `goarm`; a request as core sends it, with the minion config core would make of it, read back by number as the minion will (`TestMinionConfigOfABuild`) |
| `channels_test.go` | The HTTP channel's connection schema (version 3) and the TCP channel's (version 1), each field's key, `num`, type and default as the channels declare them, for `protocol.EncodeMinionConfig` |
| `fields_test.go`, `ops_test.go`, `exits_test.go` | The minion's numbers and exit codes against the schema; a minion whose config does not read exits with 3; a build's own codes baked in, and exited with |
| `compile_test.go` | Compiles for Linux, Windows and ARM; the build log and the compile step's progress |
| `bob_test.go` | The real builder in process with `factorytest`, through the same request checks, job and result as a build core sends: the artifact an ELF or a PE that prints the config it was given, the plan following the build, a wrong badge refused, `fail_builds` |
| `stage_test.go` | A compiled minion against a fake channel and core: staging, the hello, a check-in that gets a 404 and keeps the session, one that gets `session_closed` and stages again; tasks run; one built with a persisted session claiming it, then not after `session_replaced` |
| `management_test.go`, `broker_test.go` | The manifest beside factory-core's builds section, its properties, `say_hello`; the broker's URL |
| `build_info_test.go` | `build_info` through `factorytest.Command`, as core's call runs it: the build by BOB's labels, its file, connections and exit codes, and none of its keys, minion config, credentials' values or links; each factory command with its code |
| `session_info_test.go` | `session_info` through `factorytest.Command`: a session with every record core keeps (hello, acting, peers, a delegate, moves, the minion's own connections, restarts), none of its keys; a session just opened; the channels' params named as `channels_test.go`'s schemas number them |

BOB has no CI workflow: its checks run locally, and a release runs them
again in both modules, with the race detector (`go test -race -count=1
./...`) and lint at exactly 0 issues (`--max-issues-per-linter 0
--max-same-issues 0`).
