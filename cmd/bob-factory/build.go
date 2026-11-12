package main

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// buildSchema is what BOB builds: dummy minions, which print their config
// when run with describe. It declares one field of each kind a build schema
// adds (datetime, enum_list, file), HTTP connection parameters, and commands
// of each kind (required, picked by default, picked by the operator), so the
// build flow can be worked out end to end before a real factory exists.
// Version 2: the JSON-only targets dummy and dummy-lite are gone. Version 3:
// connection parameters by kind. The TCP channel (bind-persistent) came
// later: a channel added needs no new version. Version 4: the minions read a
// minion config (MinionConfig), its channels, fields and connection
// parameters numbered (minion/fields.go), so no instance of version 3 is
// offered a build that carries one. Version 5: each build's own exit codes
// (ErrorCodesPerBuild), which an instance of version 4 would not build in.
var buildSchema = protocol.BuildSchema{
	SchemaVersion: protocol.BuildSchemaV3,
	Version:       5,
	// Core's minion config, the build's settings numbered: BOB's choice is
	// to bake it in whole (minion.cfg), and its minions read it by number.
	MinionConfig: true,
	Channels: []protocol.BuildChannel{{
		ModuleName: "http", Num: 1, Versions: []int{3},
		// Requests first (a build that names no kind polls), and a WebSocket
		// core pushes tasks over, whose handshake is always a GET.
		Kinds: []protocol.BuildKind{
			{Kind: protocol.KindReverseCall, Fields: []protocol.FieldSpec{
				{
					Key: "method", Num: 1, Type: protocol.FieldEnum, Label: "Method", Default: "POST", Group: "HTTP", Order: 1,
					Description: "The HTTP method the minion sends its messages with.",
					Options:     []protocol.EnumOption{{Value: "GET"}, {Value: "POST"}, {Value: "PUT"}},
				},
				pathPrefix("Every request path starts with it, e.g. /cdn/assets/ when the front forwards only that path."),
			}},
			{Kind: protocol.KindReversePersistent, Fields: []protocol.FieldSpec{
				pathPrefix("The WebSocket's path starts with it, e.g. /cdn/assets/ when the front forwards only that path."),
			}},
		},
	}, {
		// The TCP channel: a minion that listens, which core reaches by
		// asking the channel to dial it (bind-persistent), or one that
		// dials the channel and stays (reverse-persistent) or dials it for
		// each exchange (reverse-call). The ports and the rest are the
		// channel's own fields: BOB adds none.
		ModuleName: "tcp", Num: 2, Versions: []int{1},
		Kinds: []protocol.BuildKind{{Kind: protocol.KindBindPersistent}, {Kind: protocol.KindReversePersistent}, {Kind: protocol.KindReverseCall}},
	}},
	// Its minions encrypt with AES-256-CBC and HMAC-SHA256; core generates
	// each one's keys and BOB writes them into the artifact.
	Crypto: protocol.CryptoAES256CBCHMACSHA256,
	// Its minions claim a persisted session when built with one.
	PersistedSessions: true,
	// The minions move between their connections as the build's failover
	// settings say (minion/failover.go).
	Failover: true,
	// The minions run up to the build's number of tasks at once, which core
	// changes live (minion/tasks.go, max_tasks).
	MaxTasks: true,
	// The minions inflate packed items, and pack their own where it pays
	// when the build has compression on (minion/packing.go).
	Compression: []string{protocol.CompressionDeflate},
	Fields: []protocol.FieldSpec{
		{
			Key: "greeting", Num: 1, Type: protocol.FieldString, Label: "Greeting", Default: "hello from BOB", MaxLength: 200,
			Order: 1, Description: "What the dummy minion says.",
		},
		{
			Key: "kill_date", Num: 2, Type: protocol.FieldDatetime, Label: "Kill date", Group: "Limits", Order: 2,
			Description: "When the minion stops for good.",
		},
		{
			Key: "punches", Num: 3, Type: protocol.FieldEnumList, Label: "Punches it takes", Order: 3,
			Options: []protocol.EnumOption{{Value: "jab"}, {Value: "cross"}, {Value: "hook"}, {Value: "uppercut"}},
		},
		{
			Key: "badge", Num: 4, Type: protocol.FieldFile, Label: "Badge", Order: 4, Accept: []string{"image/*"}, MaxSize: 256 << 10,
			Description: "An image the minion would wear. BOB checks it as it builds; a file's bytes never go in a minion config.",
		},
	},
	Targets: []protocol.BuildTarget{
		{
			ID: "linux-elf", Label: "Linux binary", Format: "elf", Platforms: []string{"linux/amd64", "linux/386"},
			Description: "A static Linux binary, the dummy minion: run with describe, it prints the config baked in when it was compiled.",
			// Linux has no named pipes: its tunnels open the rest.
			Opens: []string{protocol.OpenTCP, protocol.OpenUDP, protocol.OpenUnix, protocol.OpenSource, protocol.OpenTLS, protocol.OpenProcess,
				protocol.OpenAccept, protocol.OpenICMP},
		},
		{
			ID: "windows-exe", Label: "Windows executable", Format: "exe",
			Platforms: []string{"windows/amd64", "windows/386", "windows/arm64"},
			Description: "A Windows console program (.exe), the same dummy minion: its shell is cmd.exe, its machine id " +
				"the firmware's UUID or the install's MachineGuid, its hello says its integrity level and domain.",
		},
	},
	Commands: []protocol.BuildCommand{
		{
			ID: "sleep", Op: 1, Label: "Sleep", Required: true,
			Description: "Changes how long the minion waits between check-ins.",
			Params: []protocol.FieldSpec{
				{Key: "interval", Num: 1, Type: protocol.FieldDuration, Label: "Interval", Required: true},
				{
					Key: "jitter", Num: 2, Type: protocol.FieldInt, Label: "Jitter", Unit: "%", Min: 0, Max: 100, Default: 10,
					Description: "How far each wait may stray from the interval.",
				},
			},
		},
		{ID: "exit", Op: 2, Label: "Exit", Required: true, Description: "Stops the minion for good."},
		{ID: "hello", Op: 3, Label: "Say hello", Default: true, Description: "Answers with the build's greeting."},
		{
			ID: "ls", Op: 4, Label: "List a directory", Default: true,
			Result: protocol.ResultDirListing, Action: protocol.ActionListDir,
			Params: []protocol.FieldSpec{{Key: "path", Num: 1, Type: protocol.FieldString, Label: "Path", Default: "."}},
		},
		{
			ID: "download", Op: 5, Label: "Download a file",
			Description: "Sends a file from the minion's host to core.",
			Result:      protocol.ResultFileDownload, Action: protocol.ActionDownloadFile,
			Params: []protocol.FieldSpec{{Key: "path", Num: 1, Type: protocol.FieldString, Label: "Path", Required: true}},
		},
		{
			ID: "stream_download", Op: 6, Label: "Stream a file",
			Description: "Streams a file of any size from the minion's host to whoever downloads it; core keeps none of it.",
			Result:      protocol.ResultFileStream, Action: protocol.ActionStreamFile,
			Params: []protocol.FieldSpec{{Key: "path", Num: 1, Type: protocol.FieldString, Label: "Path", Required: true}},
		},
		{
			ID: "upload", Op: 7, Label: "Upload a file",
			Description: "Puts a file from core on the minion's host.",
			Result:      protocol.ResultFileUpload, Action: protocol.ActionUploadFile,
			Params: []protocol.FieldSpec{
				{Key: "file", Num: 1, Type: protocol.FieldFile, Label: "File", Required: true, Order: 1},
				{Key: "path", Num: 2, Type: protocol.FieldString, Label: "Path", Required: true, Order: 2,
					Description: "Where it goes on the host; a folder takes it under its own name."},
				{Key: "overwrite", Num: 3, Type: protocol.FieldBool, Label: "Overwrite", Default: false, Order: 3,
					Description: "Replace a file already there."},
			},
		},
		{
			ID: "stream_upload", Op: 8, Label: "Stream a file up",
			Description: "Puts a file of any size on the minion's host, streamed as it is uploaded; core keeps none of it.",
			Result:      protocol.ResultFileUpload, Action: protocol.ActionUploadStream,
			Params: []protocol.FieldSpec{
				{Key: "stream", Num: 1, Type: protocol.FieldStream, Label: "File", Order: 1,
					Description: "Picked in the browser, it goes once the task is queued. Left out, upload it with curl from the task's view."},
				{Key: "path", Num: 2, Type: protocol.FieldString, Label: "Path", Required: true, Order: 2,
					Description: "Where it goes on the host. A folder takes a picked file under its own name; with curl, name the file."},
				{Key: "overwrite", Num: 3, Type: protocol.FieldBool, Label: "Overwrite", Default: false, Order: 3,
					Description: "Replace a file already there."},
			},
		},
		{
			ID: "rm", Op: 9, Label: "Delete a file or folder",
			Description: "Removes a file, a link (not what it points to) or a folder on the minion's host; never a filesystem's root.",
			Action:      protocol.ActionDeletePath,
			Params: []protocol.FieldSpec{
				{Key: "path", Num: 1, Type: protocol.FieldString, Label: "Path", Required: true, Order: 1},
				{Key: "recursive", Num: 2, Type: protocol.FieldBool, Label: "With what is in it", Default: false, Order: 2,
					Description: "A folder that is not empty goes with everything in it."},
			},
		},
		{
			ID: "walk", Op: 10, Label: "Walk a directory",
			Description: "Lists a directory and the folders under it, breadth first, into core's file browser as it goes; links are not followed.",
			Action:      protocol.ActionWalkDir,
			Result:      protocol.ResultDirWalk,
			Params: []protocol.FieldSpec{
				{Key: "path", Num: 1, Type: protocol.FieldString, Label: "Path", Required: true, Order: 1},
				{Key: "depth", Num: 2, Type: protocol.FieldInt, Label: "Depth", Default: 5, Order: 2,
					Description: "How many levels of folders under it to go down; 0 for all."},
				{Key: "max_folders", Num: 3, Type: protocol.FieldInt, Label: "At most", Default: 10000, Order: 3,
					Description: "Folders to list before it stops; 0 for no limit (stop it from its task)."},
				{Key: "hidden", Num: 4, Type: protocol.FieldBool, Label: "Into hidden folders", Default: true, Order: 4},
				{Key: "one_fs", Num: 5, Type: protocol.FieldBool, Label: "Stay on its file system", Default: true, Order: 5,
					Description: "Folders on another file system (a mount, /proc) are not entered."},
			},
		},
		{
			ID: "shell", Op: 11, Label: "Run a shell command",
			Description: "Runs a command in the system shell (sh on Linux, cmd.exe on Windows) and answers with its output.",
			Params: []protocol.FieldSpec{
				{Key: "command", Num: 1, Type: protocol.FieldString, Label: "Command", Required: true},
				{Key: "timeout", Num: 2, Type: protocol.FieldDuration, Label: "Timeout", Default: "30s"},
			},
		},
		{
			ID: "switch", Op: 12, Label: "Switch connection", Default: true,
			Description: "Moves the minion to another of its connections, once that one answers; it stays on the one it has if not.",
			Action:      protocol.ActionSwitchConnection,
			Params: []protocol.FieldSpec{
				{Key: "conn", Num: 1, Type: protocol.FieldInt, Format: protocol.FormatConnection, Label: "Connection",
					Required: true, Min: 1, Order: 1, Description: "The connection to move to."},
				{Key: "for", Num: 2, Type: protocol.FieldDuration, Label: "For", Default: "0s", Order: 2,
					Description: "How long before it moves back to the connection it has now. Empty or 0s: it stays."},
			},
		},
		{
			ID: "volumes", Op: 13, Label: "List volumes", Default: true,
			Description: "Lists the host's drives (Windows) or mounted file systems (Linux), with their kind, label, file system and space; the file browser shows them as its roots.",
			Action:      protocol.ActionListVolumes,
			Result:      protocol.ResultVolumes,
		},
		{
			ID: "feedback", Op: 14, Label: "Send feedback",
			Description: "Sends a warning or an error of your words as the minion's own feedback, to try the mechanic: it shows in the session's Feedback.",
			Params: []protocol.FieldSpec{
				{Key: "level", Num: 1, Type: protocol.FieldEnum, Label: "Level", Default: "warning", Order: 1,
					Options: []protocol.EnumOption{{Value: "warning", Label: "Warning"}, {Value: "error", Label: "Error"}}},
				{Key: "message", Num: 2, Type: protocol.FieldString, Label: "Message", Required: true, Order: 2},
			},
		},
		{
			ID: "tunnel", Op: 15, Label: "Tunnel",
			Description: "Opens a tunnel through the minion's host on one of core's operational ports: a SOCKS proxy, an HTTP proxy, a port forward or a terminal, as its front says. Each connection a tool makes through it, the minion makes from its host; a terminal's, a shell (or the program it says) on the host. It runs until stopped.",
			Action:      protocol.ActionTunnel,
			// TCP connections (from a source address, TLS on top), UDP
			// associations (SOCKS UDP ASSOCIATE), unix sockets, named pipes
			// (Windows' alone: its target says), processes (the terminal
			// front: a PTY, a ConPTY, else pipes), accepts (SOCKS BIND) and
			// pings (the VPN front's: an unprivileged ICMP socket, a raw one,
			// Windows' IcmpSendEcho).
			Opens: []string{protocol.OpenTCP, protocol.OpenUDP, protocol.OpenUnix, protocol.OpenPipe, protocol.OpenSource, protocol.OpenTLS,
				protocol.OpenProcess, protocol.OpenAccept, protocol.OpenICMP},
			// Core adds the front, the port, a forward's destination, when it
			// stops the tunnel idle (stop_idle, 1h) and the bandwidth cap
			// (protocol.TunnelCoreParams): no num, never sent. Its own are
			// seldom changed (Advanced); a terminal connects to nothing, so
			// the timeout is the other fronts' alone.
			Params: []protocol.FieldSpec{
				{Key: "timeout", Num: 2, Type: protocol.FieldDuration, Label: "Connect timeout", Default: "10s", Order: 10, Advanced: true,
					Description: "How long the minion tries to connect to a destination.",
					When: &protocol.FieldCondition{Key: protocol.ParamFront,
						Values: []string{protocol.FrontSOCKS, protocol.FrontHTTP, protocol.FrontForward, protocol.FrontVPN}}},
				{Key: "max_streams", Num: 3, Type: protocol.FieldInt, Label: "Connections at once", Default: 256, Min: 1, Max: 4096, Order: 11,
					Advanced: true, Description: "How many of the tools' connections the tunnel carries at once."},
			},
		},
		{
			ID: "reverse_tunnel", Op: 17, Label: "Reverse tunnel",
			Description: "Listens on a port of the minion's host, TCP or UDP: each connection to it (each address datagrams come from), core connects to a destination from its own network (an operator's handler, a server core reaches): a reverse port forward. The minion never learns where. It runs until stopped.",
			Action:      protocol.ActionReverseTunnel,
			Opens:       []string{protocol.OpenTCP, protocol.OpenUDP},
			// Core adds the destination, when it stops the tunnel idle
			// (stop_idle, never by default) and the bandwidth cap
			// (protocol.TunnelCoreParams): no num, never sent. Its one
			// front, a forward, no param names.
			Params: []protocol.FieldSpec{
				{Key: "bind", Num: 1, Type: protocol.FieldString, Label: "Listen address", Default: "0.0.0.0", MaxLength: 255, Order: 1,
					Description: "The address the minion listens on: 0.0.0.0 for all of its host's, 127.0.0.1 for its host alone."},
				{Key: "listen", Num: 2, Type: protocol.FieldInt, Label: "Listen port", Required: true, Min: 0, Max: 65535, Order: 2,
					Description: "The port on the minion's host: 0 for any free one."},
				{Key: "udp", Num: 5, Type: protocol.FieldBool, Label: "UDP", Default: false, Order: 3,
					Description: "Listen for UDP datagrams instead: a stream for each address they come from, ending after two minutes without a datagram either way."},
				{Key: "timeout", Num: 3, Type: protocol.FieldDuration, Label: "Connect timeout", Default: "10s", Order: 10, Advanced: true,
					Description: "How long core tries to connect to a destination; a connection to the minion waits that long, and 30 s more, for it."},
				{Key: "max_streams", Num: 4, Type: protocol.FieldInt, Label: "Connections at once", Default: 256, Min: 1, Max: 4096, Order: 11,
					Advanced: true, Description: "How many connections (UDP: addresses) the tunnel carries at once: one past it is reset (a new address's datagrams, dropped)."},
			},
		},
		{
			ID: "delegate", Op: 18, Label: "Delegate",
			Description: "Carries other minions' messages: while it runs, core opens the targets that name this session as their delegate through it, and the minion dials each from its host as the TCP channel would (a minion listening for the TCP channel, in a network only this host reaches). Core starts it itself when such a target needs it; it runs until stopped.",
			Action:      protocol.ActionDelegate,
			// It serves what the minion speaks the other end of: the TCP
			// channel's framing. Core adds stop_idle (1h): no num, never
			// sent.
			Serves: []protocol.ChannelVersions{{ModuleName: "tcp", Versions: []int{1}, Kinds: []string{protocol.KindBindPersistent}}},
			Kinds:  []string{protocol.KindReverseCall, protocol.KindReversePersistent, protocol.KindBindPersistent},
		},
		{
			ID: "reverse_delegate", Op: 19, Label: "Reverse delegate",
			Description: "Carries other minions' messages the other way: while it runs, the minion listens on a port of its host as the TCP channel does, and minions built with this host's address for their tcp connection dial it (reverse-persistent: each stays connected; reverse-call: a connection for each exchange). It runs until stopped.",
			Action:      protocol.ActionReverseDelegate,
			// Core adds stop_idle (never by default): no num, never sent.
			Serves: []protocol.ChannelVersions{{ModuleName: "tcp", Versions: []int{1},
				Kinds: []string{protocol.KindReverseCall, protocol.KindReversePersistent}}},
			Kinds: []string{protocol.KindReverseCall, protocol.KindReversePersistent, protocol.KindBindPersistent},
			Params: []protocol.FieldSpec{
				{Key: "bind", Num: 1, Type: protocol.FieldString, Label: "Listen address", Default: "0.0.0.0", MaxLength: 255, Order: 1,
					Description: "The address the minion listens on: 0.0.0.0 for all of its host's."},
				{Key: "listen", Num: 2, Type: protocol.FieldInt, Label: "Listen port", Required: true, Min: 1, Max: 65535, Order: 2,
					Description: "The port the minions it carries dial: their tcp connection's port."},
				{Key: "kind", Num: 3, Type: protocol.FieldEnum, Label: "Kind", Default: protocol.KindReversePersistent, Order: 3,
					Description: "How the minions it carries are built: connected (reverse-persistent), or a connection for each exchange (reverse-call).",
					Options: []protocol.EnumOption{{Value: protocol.KindReversePersistent, Label: "Reverse-persistent"},
						{Value: protocol.KindReverseCall, Label: "Reverse-call"}}},
				{Key: "max_connections", Num: 4, Type: protocol.FieldInt, Label: "Connections at once", Default: 16, Min: 1, Max: 256, Order: 10,
					Advanced: true, Description: "Minions it carries at once (calls under way, for reverse-call): one past it is closed."},
			},
		},
	},
	// What the minion's exit codes mean (minion/exits.go, which
	// exits_test.go checks against these): it exits with one when it cannot
	// start. Once running it goes on until stopped, and exit ends it with 0.
	// Each build's minion exits with codes of its own, drawn by core, but
	// for 1 and 2: Go's, which the runtime and libraries exit with too.
	ErrorCodesPerBuild: true,
	ErrorCodes: []protocol.ErrorCode{
		{Code: 1, Label: "Could not start", Description: "For a reason without a code of its own: the minion's log says which.", Fixed: true},
		{Code: 2, Label: "Crashed", Description: "Go's own exit code: a panic no task recovered, or a fatal error of the Go runtime " +
			"(out of memory, say). A bug in the minion: its output says where.", Fixed: true},
		{Code: 3, Label: "Its config does not read", Description: "The settings built into it do not read as a minion config: " +
			"the binary is damaged. Build it again."},
		{Code: 4, Label: "Built without core's keys", Description: "Its config has no payload keys, and BOB's minions speak core's crypto. Build it again."},
		{Code: 5, Label: "No connection it can use", Description: "None of its connections is one it speaks: an http connection of a " +
			"reverse kind, or a tcp one. Build it with one."},
	},
}

// pathPrefix is BOB's path prefix parameter, described for its kind.
func pathPrefix(help string) protocol.FieldSpec {
	return protocol.FieldSpec{
		Key: "path_prefix", Num: 2, Type: protocol.FieldString, Format: protocol.FormatPath, Label: "Path prefix", Default: "/",
		Group: "HTTP", Order: 2, Description: help,
		Pattern: `^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`, MaxLength: 256,
	}
}

// bob builds dummy minions, the way its management says (management.go).
type bob struct {
	// minionSrc is the minion module's directory, compiled for each target
	// (compiledTargets).
	minionSrc string
	settings  atomic.Pointer[settings]
}

// settings are BOB's own properties that shape its builds.
type settings struct {
	fail bool // fail_builds
}

// current is the settings a build starts with: the defaults until management
// applies any.
func (b *bob) current() settings {
	if s := b.settings.Load(); s != nil {
		return *s
	}
	return settings{}
}

// The steps of BOB's builds.
var (
	stepBadge   = protocol.BuildStep{ID: "badge", Label: "Check the badge"}
	stepCompile = protocol.BuildStep{ID: "compile", Label: "Compile the binary"}
	stepUpload  = protocol.BuildStep{ID: "upload", Label: "Upload the artifact"}
)

// Steps is the plan for a build of req: the badge is checked only when there
// is one.
func (b *bob) Steps(req protocol.BuildRequest) []protocol.BuildStep {
	var steps []protocol.BuildStep
	if id, _ := req.Values["badge"].(string); id != "" {
		steps = append(steps, stepBadge)
	}
	return append(steps, stepCompile, stepUpload)
}

func (b *bob) Build(ctx context.Context, job *factory.Job) (factory.Result, error) {
	s := b.current()
	if f, ok := job.File("badge"); ok {
		job.Step(stepBadge.ID, "checking the badge")
		if _, err := job.ReadFile(ctx, f); err != nil {
			return factory.Result{}, factory.Fail("badge_unreadable", err.Error())
		}
	}
	req := job.Request
	job.Step(stepCompile.ID, "go build for "+req.Platform)
	if s.fail {
		return factory.Result{}, factory.Fail("knocked_out", "BOB is set to fail every build (fail_builds)")
	}
	// Core's minion config, checked with the request: baked in as it came,
	// the minion reads it by numbers alone.
	job.Logf("dummy minion of build %s for %s with %d connection(s), a %d-byte config", req.BuildID, req.Platform,
		len(req.Connections), len(req.MinionConfig))

	exits := exitCodes(req)
	if len(exits) > 0 {
		job.Logf("exit codes of its own: %s", formatExitCodes(exits))
	}
	out := &lineLog{logf: job.Logf}
	bin, err := compile(ctx, b.minionSrc, req.Target, req.Platform, req.MinionConfig, exits, out)
	out.Flush()
	switch {
	case errors.Is(err, errNoToolchain):
		return factory.Result{}, factory.Fail("no_toolchain", err.Error())
	case err != nil && ctx.Err() == nil:
		return factory.Result{}, factory.Fail("compile_failed", "the dummy minion did not compile; see the build log")
	case err != nil:
		return factory.Result{}, err
	}
	job.Logf("compiled a %d-byte binary for %s", len(bin), req.Platform)

	job.Step(stepUpload.ID, "")
	a := artifactKinds[req.Target]
	name := "bob-" + req.BuildID[:8] + a.ext
	if err := job.UploadArtifact(ctx, name, a.mediaType, bytes.NewReader(bin)); err != nil {
		return factory.Result{}, err
	}
	return factory.Result{
		Warnings: []string{"a dummy minion, made to try the build flow end to end"},
	}, nil
}

// artifactKinds are what each target's artifact is named and typed as.
var artifactKinds = map[string]struct{ ext, mediaType string }{
	"linux-elf":   {".bin", "application/x-executable"},
	"windows-exe": {".exe", "application/vnd.microsoft.portable-executable"},
}
