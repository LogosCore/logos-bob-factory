package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-factory-core/pkg/factorytest"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// squash is a report with each run of spaces (a row's indent, its
// alignment) as one: what a row says, whatever its neighbours' widths.
func squash(s string) string { return regexp.MustCompile(` {2,}`).ReplaceAllString(s, " ") }

// fullSession is a session with every record core keeps of one: a Windows
// minion that staged, acts as another user, moved to its second connection,
// is seen from two addresses and through a delegate, restarted and has
// unread errors.
func fullSession() protocol.SessionRecord {
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	after := func(d time.Duration) *time.Time { v := at.Add(d); return &v }
	n := func(v int) *int { return &v }
	s := factorytest.Session(buildSchema)
	s.Number, s.Target, s.Platform = 12, "windows-exe", "windows/amd64"
	s.Description, s.Staged, s.CreatedAt, s.LastSeenAt = "the front desk's PC", true, at, after(5*time.Minute)
	s.Hello = protocol.SessionHello{PID: 4242, OS: "windows", Arch: "amd64", Host: "FRONTDESK", User: "alice", Domain: "CORP",
		Proc: "bob.exe", Integrity: "high", Addrs: []string{"10.0.0.5", "fe80::1"}, Version: "v0.47.0",
		MachineID: "firmware:4c4c4544-0042", RoundtripTime: n(42), X: map[string]any{"locale": "en-GB"}}
	s.Acting = &protocol.SessionIdentity{User: "admin", Domain: "CORP", How: "token", TaskID: protocol.NewID(), Since: at.Add(2 * time.Minute)}
	s.Address, s.PeerAddress = "10.0.0.5", "203.0.113.7"
	s.Peers = []protocol.SessionPeer{
		{Address: "203.0.113.7", FirstSeen: at, LastSeen: at.Add(5 * time.Minute), Count: 120},
		{Address: "198.51.100.9", FirstSeen: at, LastSeen: at, Count: 1},
	}
	s.Via, s.ViaSince = []protocol.SessionVia{{SessionID: protocol.NewID(), Number: 3, TaskID: protocol.NewID()}}, after(time.Minute)
	s.Sleep = &protocol.SessionSleep{Interval: 30_000, Jitter: 10}
	s.LateAt, s.LostAt = after(6*time.Minute), after(35*time.Minute)
	s.Conn, s.ConnKind, s.ConnName, s.ConnCount = 2, protocol.KindReversePersistent, "edge-ws", 2
	s.ConnChanges = []protocol.SessionConnChange{{At: at.Add(3 * time.Minute), From: 1, To: 2, Reason: "failover"}}
	s.Connections = []protocol.SessionConnection{
		{Channel: "http", Kind: protocol.KindReverseCall, Name: "edge-eu", Version: 3,
			Params: map[string]any{"1": "https://edge-eu.example.com", "11": "60s"}, Values: map[string]any{"1": "POST", "2": "/cdn/assets/"}},
		{Channel: "http", Kind: protocol.KindReversePersistent, Name: "edge-ws", Version: 3,
			Params: map[string]any{"1": "https://edge-ws.example.com"}, Values: map[string]any{"2": "/ws/"}, MaxMessage: 1 << 20},
	}
	s.ConnectionsAt = after(3 * time.Minute)
	s.MaxTasks, s.MaxTasksSet = n(5), n(4)
	s.FeedbackErrors, s.FeedbackAt = 2, after(4*time.Minute)
	s.Persisted, s.RestartCount = true, 3
	s.Restarts = []protocol.SessionRestart{{At: at, PID: 4242, Host: "FRONTDESK", BuildID: s.BuildID}}
	s.Crypto = &protocol.BuildCrypto{Suite: protocol.CryptoAES256CBCHMACSHA256,
		Material: json.RawMessage(`{"enc_key":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=","mac_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="}`)}
	return s
}

// session_info prints the session as core keeps it: what it is and how it
// checks in, its host by its hello, where it is seen from and through, the
// connection it uses, its moves and the connections the minion holds (the
// channels' params and BOB's values by name), tasks, feedback and restarts;
// never its keys.
func TestSessionInfo(t *testing.T) {
	s := fullSession()
	out := factorytest.Command(t, buildSchema, sessionInfo{}, factorytest.NewStore(t), protocol.FactoryCommandRequest{
		Command: sessionInfoCommand.ID, Session: s,
	})
	if out.Result.State != protocol.EndDone || !out.Result.OK {
		t.Fatalf("result %+v, output:\n%s", out.Result, out.Output)
	}
	text := squash(string(out.Output))
	for _, want := range []string{
		"Session #12 (" + s.SessionID + ")",
		"Build " + s.BuildID + ": Windows executable (windows-exe), windows/amd64",
		"Description the front desk's PC",
		"Opened 2026.11.22 10:00:00 UTC, after a stage",
		"Last seen 2026.11.22 10:05:00 UTC",
		"Check-ins every 30s, ±10%",
		"Late at 2026.11.22 10:06:00 UTC",
		"Lost at 2026.11.22 10:35:00 UTC",
		"Connected no: no persistent connection serves it now",
		"Persisted a build names it, and its minions claim it (3 restarts)",
		"Keys aes-256-cbc-hmac-sha256 (not shown)",
		"Host FRONTDESK",
		`User CORP\alice, integrity high`,
		"OS windows/amd64",
		"Process bob.exe, PID 4242",
		"Minion version v0.47.0",
		"Machine id firmware:4c4c4544-0042",
		"Addresses 10.0.0.5, fe80::1",
		"Address shown 10.0.0.5",
		"Round trip 42 ms",
		"locale en-GB",
		`Acting as CORP\admin, by token, since 2026.11.22 10:02:00 UTC`,
		"Now 203.0.113.7",
		"203.0.113.7 120 messages, 2026.11.22 10:00:00 UTC to 2026.11.22 10:05:00 UTC",
		"198.51.100.9 1 message,",
		"Through delegates (1), nearest first",
		"Session #3 its task " + s.Via[0].TaskID,
		"Uses 2. edge-ws (reverse-persistent), of 2",
		"Moves 2026.11.22 10:03:00 UTC, 1 → 2: failover",
		"Its connections (2), as its minion said, 2026.11.22 10:03:00 UTC",
		"1. edge-eu: http v3, reverse-call\n",
		"base_url https://edge-eu.example.com",
		"checkin_interval 60s",
		"Method POST",
		"Path prefix /cdn/assets/",
		"2. edge-ws: http v3, reverse-persistent (in use)",
		"Largest message 1.0 MiB",
		"At once 5, as its minion said",
		"Operators' limit 4",
		"Unread errors 2",
		"Latest 2026.11.22 10:04:00 UTC",
		"Restarts (3 in all), the latest first",
		"2026.11.22 10:00:00 UTC PID 4242 on FRONTDESK, build " + s.BuildID,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, out.Output)
		}
	}
	for _, secret := range []string{"AQEBAQEB", "AgICAgIC"} {
		if strings.Contains(text, secret) {
			t.Errorf("the session's keys (%q) printed:\n%s", secret, out.Output)
		}
	}
}

// A session just opened says what it lacks; one whose connection has a
// version BOB does not know says its params by number.
func TestSessionInfoOfANewSession(t *testing.T) {
	s := factorytest.Session(buildSchema)
	var b bytes.Buffer
	writeSessionInfo(&b, buildSchema, s)
	text := squash(b.String())
	for _, want := range []string{
		"Build " + s.BuildID + ": Linux binary (linux-elf), linux/amd64", ", under its build's id",
		"Last seen never: no check-in yet", "OS linux/amd64", "Uses not known yet",
		"Its connections its build's (build_info lists them)", "Unread errors 0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, b.String())
		}
	}
	for _, none := range []string{"Connected", "Persisted", "Seen from", "Through delegates", "Restarts", "Tasks", "Keys"} {
		if strings.Contains(text, none) {
			t.Errorf("%q, of a session without it:\n%s", none, b.String())
		}
	}

	s.Connections = []protocol.SessionConnection{{Channel: "http", Kind: protocol.KindReverseCall, Name: "later", Version: 9,
		Params: map[string]any{"1": "https://later.example.com"}, Values: map[string]any{"7": "x"}}}
	b.Reset()
	writeSessionInfo(&b, buildSchema, s)
	if text := squash(b.String()); !strings.Contains(text, "param 1 https://later.example.com") || !strings.Contains(text, "value 7 x") {
		t.Errorf("a version BOB does not know:\n%s", b.String())
	}
}

// channelParams names the channels' params as the channels number them, in
// every kind and in a target.
func TestChannelParamsAsTheChannelsNumberThem(t *testing.T) {
	for module, c := range channelParams {
		schema, ok := testChannels(module, int(c.version))
		if !ok {
			t.Fatalf("%s v%d: no such channel schema here", module, c.version)
		}
		numbered := map[string]string{}
		for _, k := range schema.Kinds {
			for _, f := range append(append([]protocol.FieldSpec{}, k.Fields...), k.Target...) {
				numbered[strconv.Itoa(f.Num)] = f.Key
			}
		}
		if !maps.Equal(numbered, c.keys) {
			t.Errorf("%s: channelParams %v, the channel's %v", module, c.keys, numbered)
		}
	}
}
