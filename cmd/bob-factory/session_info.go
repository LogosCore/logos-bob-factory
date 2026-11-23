package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// session_info prints the session as core keeps it (factory_commands.go): the
// host and process its minion's hello said, how it checks in, where it is
// seen from, the connection it uses and its moves, the connections the minion
// holds, delegates, tasks at once, feedback and restarts. Its keys are never
// printed: their suite is.
var sessionInfoCommand = protocol.FactoryCommand{
	ID:    "session_info",
	Label: "Session info",
	Description: "Prints the session as core keeps it: the host and process its minion's hello said, its check-ins, " +
		"addresses, the connection it uses and its moves, delegates, restarts and feedback. BOB runs it, not the minion: " +
		"it answers while the minion is out of reach. Its keys are left out.",
}

// sessionInfo is session_info's code.
type sessionInfo struct{}

func (sessionInfo) Run(_ context.Context, t *factory.CommandTask) (factory.CommandResult, error) {
	writeSessionInfo(t.Out(), buildSchema, t.Session)
	return factory.CommandResult{OK: true}, nil
}

// channelParams are the connection parameters of the channels BOB's minions
// speak, by number as text, as the channels number them in the versions BOB
// reads (the HTTP channel's 3 and 4, whose mode is 14; the TCP channel's 1;
// minion/fields.go reads them): a session's own connections say their
// params by number alone. channels_test.go's schemas check them.
var channelParams = map[string]struct {
	versions []uint64
	keys     map[string]string
}{
	"http": {[]uint64{3, 4}, map[string]string{"1": "base_url", "2": "extra_base_urls", "3": "host_header", "4": "sni", "5": "proxy",
		"6": "tls_verify", "7": "tls_pin_sha256", "8": "user_agent", "9": "headers", "10": "request_timeout",
		"11": "checkin_interval", "12": "checkin_jitter", "13": "ws_reconnect_backoff", "14": "mode"}},
	"tcp": {[]uint64{1}, map[string]string{"1": "host", "2": "port", "3": "connect_timeout", "4": "keepalive", "5": "reconnect_backoff",
		"6": "request_timeout", "7": "checkin_interval", "8": "checkin_jitter", "9": "listen_port", "10": "listen_address",
		"11": "max_connections", "12": "proxy"}},
}

// writeSessionInfo writes s in sections of aligned rows: what it is and how it
// checks in, its host, where it is seen from and through, its connection,
// tasks at once, feedback and restarts.
func writeSessionInfo(w io.Writer, schema protocol.BuildSchema, s protocol.SessionRecord) {
	r := newReport(w)
	defer r.flush()
	row, title := r.row, r.title

	r.line(fmt.Sprintf("Session #%d (%s)", s.Number, s.SessionID))
	target := s.Target
	if t, ok := schema.Target(s.Target); ok && t.Label != "" {
		target = fmt.Sprintf("%s (%s)", t.Label, s.Target)
	}
	row("  ", "Build", fmt.Sprintf("%s: %s, %s", s.BuildID, target, s.Platform))
	if s.Description != "" {
		row("  ", "Description", s.Description)
	}
	if s.BuildDescription != "" {
		row("  ", "Its build's note", s.BuildDescription)
	}
	opened := when(s.CreatedAt) + ", under its build's id"
	if s.Staged {
		opened = when(s.CreatedAt) + ", after a stage"
	}
	row("  ", "Opened", opened)
	if s.LastSeenAt != nil {
		row("  ", "Last seen", when(*s.LastSeenAt))
	} else {
		row("  ", "Last seen", "never: no check-in yet")
	}
	if s.Sleep != nil {
		row("  ", "Check-ins", sleepText(*s.Sleep))
	}
	if s.LateAt != nil {
		row("  ", "Late at", when(*s.LateAt))
	}
	if s.LostAt != nil {
		row("  ", "Lost at", when(*s.LostAt))
	}
	if persistentKind(s.ConnKind) {
		if s.Connected {
			row("  ", "Connected", "yes: a persistent connection serves it")
		} else {
			row("  ", "Connected", "no: no persistent connection serves it now")
		}
	}
	if s.Persisted {
		row("  ", "Persisted", fmt.Sprintf("a build names it, and its minions claim it (%d %s)", s.RestartCount, plural(s.RestartCount, "restart")))
	}
	if s.Archived {
		row("  ", "Archived", "hidden from the operation's sessions")
	}
	if s.FolderID != "" {
		row("  ", "Folder", s.FolderID)
	}
	if s.Crypto != nil {
		row("  ", "Keys", s.Crypto.Suite+" (not shown)")
	}

	h := s.Hello
	title("Host, as its minion's hello said")
	if h.Host != "" {
		row("  ", "Host", h.Host)
	}
	if user := account(h.User, h.Domain); user != "" {
		if h.Integrity != "" {
			user += ", integrity " + h.Integrity
		}
		row("  ", "User", user)
	}
	row("  ", "OS", h.OS+"/"+h.Arch)
	if h.Proc != "" || h.PID != 0 {
		proc := cmp.Or(h.Proc, "a process")
		if h.PID != 0 {
			proc += fmt.Sprintf(", PID %d", h.PID)
		}
		row("  ", "Process", proc)
	}
	if h.Version != "" {
		row("  ", "Minion version", h.Version)
	}
	if h.MachineID != "" {
		row("  ", "Machine id", h.MachineID)
	}
	if len(h.Addrs) > 0 {
		row("  ", "Addresses", strings.Join(h.Addrs, ", "))
	}
	if s.Address != "" {
		row("  ", "Address shown", s.Address)
	}
	if h.RoundtripTime != nil {
		row("  ", "Round trip", fmt.Sprintf("%d ms", *h.RoundtripTime))
	}
	for _, key := range slices.Sorted(maps.Keys(h.X)) {
		row("  ", key, plain(h.X[key]))
	}
	if a := s.Acting; a != nil {
		acting := account(a.User, a.Domain)
		if a.Integrity != "" {
			acting += ", integrity " + a.Integrity
		}
		if a.How != "" {
			acting += ", by " + a.How
		}
		row("  ", "Acting as", acting+", since "+when(a.Since))
	}

	if s.PeerAddress != "" || len(s.Peers) > 0 {
		title("Seen from")
		if s.PeerAddress != "" {
			row("  ", "Now", s.PeerAddress)
		}
		for _, p := range s.Peers {
			row("  ", p.Address, fmt.Sprintf("%d %s, %s to %s", p.Count, plural(p.Count, "message"), when(p.FirstSeen), when(p.LastSeen)))
		}
	}

	if len(s.Via) > 0 {
		title("Through delegates (%d), nearest first", len(s.Via))
		for _, v := range s.Via {
			row("  ", fmt.Sprintf("Session #%d", v.Number), "its task "+v.TaskID)
		}
		if s.ViaSince != nil {
			row("  ", "Since", when(*s.ViaSince))
		}
	}

	title("Connection")
	if s.Conn > 0 {
		uses := strconv.Itoa(s.Conn)
		if s.ConnName != "" {
			uses += ". " + s.ConnName
		}
		if s.ConnKind != "" {
			uses += " (" + s.ConnKind + ")"
		}
		if s.ConnCount > 0 {
			uses += fmt.Sprintf(", of %d", s.ConnCount)
		}
		row("  ", "Uses", uses)
	} else {
		row("  ", "Uses", "not known yet")
	}
	for i, c := range s.ConnChanges {
		label := ""
		if i == 0 {
			label = "Moves"
		}
		move := fmt.Sprintf("%s, %s → %d", when(c.At), placeText(c.From), c.To)
		if c.Reason != "" {
			move += ": " + c.Reason
		}
		row("  ", label, move)
	}
	if s.Connections == nil {
		row("  ", "Its connections", "its build's (build_info lists them)")
	} else {
		as := "as its minion said"
		if s.ConnectionsAt != nil {
			as += ", " + when(*s.ConnectionsAt)
		}
		title("Its connections (%d), %s", len(s.Connections), as)
		for i, c := range s.Connections {
			head := fmt.Sprintf("  %d. %s: %s", i+1, c.Name, c.Channel)
			if c.Version > 0 {
				head += fmt.Sprintf(" v%d", c.Version)
			}
			head += ", " + c.Kind
			if i+1 == s.Conn {
				head += " (in use)"
			}
			r.line(head)
			const in = "     "
			names := paramNames(c.Channel, c.Version)
			for _, num := range byNumber(c.Params) {
				row(in, cmp.Or(names[num], "param "+num), plain(c.Params[num]))
			}
			labels := valueLabels(schema, c.Channel, c.Kind)
			for _, num := range byNumber(c.Values) {
				row(in, cmp.Or(labels[num], "value "+num), plain(c.Values[num]))
			}
			if c.FailoverAuto != nil && !*c.FailoverAuto {
				row(in, "Failover", "never moves the minion to it")
			}
			if c.FailAfter > 0 {
				row(in, "Failover", fmt.Sprintf("moves after %d failures in a row", c.FailAfter))
			}
			if c.MaxMessage > 0 {
				row(in, "Largest message", byteSize(int64(c.MaxMessage)))
			}
		}
	}

	if s.MaxTasks != nil || s.MaxTasksSet != nil {
		title("Tasks")
		if s.MaxTasks != nil {
			row("  ", "At once", fmt.Sprintf("%d, as its minion said", *s.MaxTasks))
		}
		if s.MaxTasksSet != nil {
			row("  ", "Operators' limit", strconv.Itoa(*s.MaxTasksSet))
		}
	}

	title("Feedback")
	row("  ", "Unread errors", strconv.Itoa(s.FeedbackErrors))
	if s.FeedbackAt != nil {
		row("  ", "Latest", when(*s.FeedbackAt))
	}
	if s.FeedbackReadAt != nil {
		row("  ", "Marked read", when(*s.FeedbackReadAt))
	}

	if len(s.Restarts) > 0 {
		title("Restarts (%d in all), the latest first", s.RestartCount)
		for _, rs := range s.Restarts {
			claim := fmt.Sprintf("PID %d", rs.PID)
			if rs.Host != "" {
				claim += " on " + rs.Host
			}
			row("  ", when(rs.At), claim+", build "+rs.BuildID)
		}
	}
}

// sleepText is a minion's check-in schedule in words.
func sleepText(s protocol.SessionSleep) string {
	if s.Interval == 0 {
		return "no wait between check-ins"
	}
	text := "every " + shortDuration(time.Duration(s.Interval)*time.Millisecond)
	if s.Jitter > 0 {
		text += fmt.Sprintf(", ±%d%%", s.Jitter)
	}
	return text
}

// shortDuration is d without the zero units Go prints: 1m, 1h30m, 45s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// account is a user with its domain, as Windows writes them (DOMAIN\user).
func account(user, domain string) string {
	if domain == "" || user == "" {
		return user
	}
	return domain + `\` + user
}

// persistentKind reports whether kind keeps a connection open, so a session of
// it is connected or not.
func persistentKind(kind string) bool {
	return kind == protocol.KindReversePersistent || kind == protocol.KindBindPersistent
}

// placeText is a connection's place, from 1; 0: unknown.
func placeText(place int) string {
	if place < 1 {
		return "an unknown one"
	}
	return strconv.Itoa(place)
}

// plural is word for n of it.
func plural[N ~int | ~int64](n N, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// paramNames are channel's connection parameters by number, when its
// connection schema version is one BOB knows (or unsaid).
func paramNames(channel string, version uint64) map[string]string {
	c, ok := channelParams[channel]
	if !ok || (version != 0 && !slices.Contains(c.versions, version)) {
		return nil
	}
	return c.keys
}

// valueLabels are BOB's connection parameters of a channel's kind, by number:
// their labels.
func valueLabels(schema protocol.BuildSchema, channel, kind string) map[string]string {
	out := map[string]string{}
	if ch, ok := schema.Channel(channel); ok {
		for _, f := range ch.KindFields(kind) {
			out[strconv.Itoa(f.Num)] = cmp.Or(f.Label, f.Key)
		}
	}
	return out
}

// byNumber are m's keys, numbers as text, in their order; any other after.
func byNumber(m map[string]any) []string {
	keys := slices.Collect(maps.Keys(m))
	slices.SortFunc(keys, func(a, b string) int {
		x, errA := strconv.Atoi(a)
		y, errB := strconv.Atoi(b)
		switch {
		case errA == nil && errB == nil:
			return cmp.Compare(x, y)
		case errA == nil:
			return -1
		case errB == nil:
			return 1
		}
		return strings.Compare(a, b)
	})
	return keys
}
