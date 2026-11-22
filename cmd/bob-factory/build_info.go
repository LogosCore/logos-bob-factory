package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// build_info is BOB's factory command (docs: contracts/factory-commands):
// BOB runs it, never the minion, in every session of its builds, so it
// answers while the minion is out of reach. It prints the session's build as
// core keeps it, the record and the factory.build request BOB got, by the
// schema's labels. What the call carries for the task alone is never
// printed: the minion's keys (their suite is), the minion config (it holds
// them: its size is), credentials' values (their ids, types and names are)
// and links.
var buildInfoCommand = protocol.FactoryCommand{
	ID:    "build_info",
	Label: "Build info",
	Description: "Prints the session's build as core keeps it: its target, values, commands, connections, exit codes and " +
		"artifact. BOB runs it, not the minion: it answers while the minion is out of reach. Keys and secrets are left out.",
}

// factoryCommands are the code of the schema's factory commands, by id.
var factoryCommands = map[string]factory.FactoryCommand{buildInfoCommand.ID: buildInfo{}}

// buildInfo is build_info's code.
type buildInfo struct{}

func (buildInfo) Run(_ context.Context, t *factory.CommandTask) (factory.CommandResult, error) {
	req, err := t.BuildRequest()
	if err != nil {
		return factory.CommandResult{}, fmt.Errorf("the build's request does not read: %w", err)
	}
	writeBuildInfo(t.Out(), buildSchema, t.Session, t.Build, req)
	return factory.CommandResult{OK: true}, nil
}

// writeBuildInfo writes b, session's build, made with req, in sections of
// aligned rows: what it is, its values, commands, connections, failover, exit
// codes, what the minion was built with, and the credentials it carries.
func writeBuildInfo(w io.Writer, schema protocol.BuildSchema, s protocol.SessionRecord, b protocol.BuildRecord, req protocol.BuildRequest) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	// A row's label and value, indented; a line without a tab (a title)
	// ends the rows aligned together.
	row := func(indent, label, value string) {
		_, _ = fmt.Fprintf(tw, "%s%s\t%s\n", indent, oneLine(label), oneLine(value))
	}
	title := func(format string, args ...any) { _, _ = fmt.Fprintf(tw, "\n"+format+"\n", args...) }

	_, _ = fmt.Fprintf(tw, "Build %s (session #%d)\n", b.BuildID, s.Number)
	target, known := schema.Target(b.Target)
	if known && target.Label != "" {
		row("  ", "Target", fmt.Sprintf("%s (%s)", target.Label, b.Target))
	} else {
		row("  ", "Target", b.Target)
	}
	row("  ", "Platform", b.Platform)
	if b.Description != "" {
		row("  ", "Description", b.Description)
	}
	schemaVersion := strconv.Itoa(b.SchemaVersion)
	if b.SchemaVersion != schema.Version {
		schemaVersion += fmt.Sprintf(" (BOB's is %d now)", schema.Version)
	}
	row("  ", "Schema version", schemaVersion)
	created := when(b.CreatedAt)
	if b.CreatedBy != nil {
		created += " by " + actor(*b.CreatedBy)
	}
	row("  ", "Created", created)
	if b.StartedAt != nil {
		row("  ", "Started", when(*b.StartedAt))
	}
	if b.FinishedAt != nil {
		finished := when(*b.FinishedAt)
		if b.StartedAt != nil {
			finished += ", took " + took(b.FinishedAt.Sub(*b.StartedAt))
		}
		row("  ", "Finished", finished)
	}
	if b.Instance != "" {
		built := b.Instance
		if b.FactoryVersion != "" {
			built += ", BOB " + b.FactoryVersion
		}
		row("  ", "Built by", built)
	}
	if a := b.Artifact; a != nil {
		art := fmt.Sprintf("%s, %s, %s", a.Filename, byteSize(a.Size), cmp.Or(a.MediaType, "no media type"))
		if a.Deleted {
			art += " (its bytes were deleted)"
		}
		row("  ", "Artifact", art)
		row("  ", "Artifact SHA-256", a.SHA256)
	}
	for _, warning := range b.Warnings {
		row("  ", "Warning", warning)
	}

	files := map[string]protocol.BuildFile{}
	maps.Copy(files, req.Files)
	maps.Copy(files, b.Files) // the record's: the same files, with new links
	fields := schema.PlatformFields(target, b.Platform)
	title("Values (%d)", len(req.Values))
	shown := map[string]bool{}
	for _, f := range fields {
		if v, ok := req.Values[f.Key]; ok {
			shown[f.Key] = true
			row("  ", cmp.Or(f.Label, f.Key), fieldValue(f, v, files, req.Credentials))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(req.Values)) {
		if !shown[key] {
			row("  ", key, plain(req.Values[key]))
		}
	}

	title("Commands (%d)", len(req.Commands))
	for _, line := range wrap(req.Commands, 76) {
		_, _ = fmt.Fprintln(tw, "  "+line)
	}

	title("Connections (%d)", len(req.Connections))
	for i, c := range req.Connections {
		e := c.Endpoint
		_, _ = fmt.Fprintf(tw, "  %d. %s: %s v%d, %s\n", i+1, oneLine(e.Name), e.Channel.ModuleName, e.Channel.SchemaVersion, e.Kind)
		const in = "     "
		if i < len(b.ConnectionDescriptions) && b.ConnectionDescriptions[i] != "" {
			row(in, "Note", b.ConnectionDescriptions[i])
		}
		for _, key := range slices.Sorted(maps.Keys(e.Params)) {
			row(in, key, plain(e.Params[key]))
		}
		var kindFields []protocol.FieldSpec
		if ch, ok := schema.Channel(e.Channel.ModuleName); ok {
			kindFields = ch.KindFields(e.Kind)
		}
		shown := map[string]bool{}
		for _, f := range kindFields {
			if v, ok := c.Values[f.Key]; ok {
				shown[f.Key] = true
				row(in, cmp.Or(f.Label, f.Key), fieldValue(f, v, files, req.Credentials))
			}
		}
		for _, key := range slices.Sorted(maps.Keys(c.Values)) {
			if !shown[key] {
				row(in, key, plain(c.Values[key]))
			}
		}
		if f := c.Failover; f != nil {
			var own []string
			if f.Auto != nil && !*f.Auto {
				own = append(own, "failover never moves the minion to it")
			}
			if f.FailAfter > 0 {
				own = append(own, fmt.Sprintf("moves after %d failures in a row", f.FailAfter))
			}
			if len(own) > 0 {
				row(in, "Failover", strings.Join(own, "; "))
			}
		}
		if c.MaxMessage > 0 {
			row(in, "Largest message", byteSize(int64(c.MaxMessage)))
		}
	}

	if f := req.Failover; f != nil {
		title("Failover")
		_, _ = fmt.Fprintln(tw, "  "+failoverText(*f))
	}

	if len(b.ErrorCodes) > 0 {
		title("Exit codes (%d)", len(b.ErrorCodes))
		for _, e := range b.ErrorCodes {
			label := e.Label
			switch {
			case e.Fixed:
				label += " (in every build)"
			case e.ExitCode != e.Code:
				label += fmt.Sprintf(" (declared %d)", e.Code)
			}
			row("  ", strconv.Itoa(e.ExitCode), label)
		}
	}

	title("Built with")
	if req.Crypto != nil {
		row("  ", "Keys", req.Crypto.Suite+" (not shown)")
	}
	if req.Envelope != "" {
		row("  ", "Envelope", req.Envelope)
	}
	if len(req.MinionConfig) > 0 {
		row("  ", "Minion config", byteSize(int64(len(req.MinionConfig)))+", baked in (not shown: it holds the keys)")
	}
	row("  ", "Compression", cmp.Or(req.Compression, "off"))
	if req.MaxTasks != nil {
		row("  ", "Tasks at once", strconv.FormatUint(*req.MaxTasks, 10))
	}
	if req.SessionID != "" {
		row("  ", "Persisted session", req.SessionID)
	}

	if len(req.Credentials) > 0 {
		title("Credentials (%d): their values are not shown", len(req.Credentials))
		for _, id := range slices.Sorted(maps.Keys(req.Credentials)) {
			c := req.Credentials[id]
			about := c.Type
			if c.Name != "" {
				about += ", " + c.Name
			}
			if rev, ok := b.CredentialRevs[id]; ok {
				about += fmt.Sprintf(", revision %d", rev)
			}
			row("  ", id, about)
		}
	}
}

// fieldValue is a value of field f in words: a file by its name, size and
// type, a credential by its type and name (never its values), a list joined.
func fieldValue(f protocol.FieldSpec, v any, files map[string]protocol.BuildFile, creds map[string]protocol.Credential) string {
	switch f.Type {
	case protocol.FieldFile:
		id, _ := v.(string)
		if file, ok := files[id]; ok {
			return fmt.Sprintf("%s, %s, %s", file.Name, byteSize(file.Size), cmp.Or(file.MediaType, "no media type"))
		}
	case protocol.FieldCredential:
		id, _ := v.(string)
		return credentialText(id, creds)
	case protocol.FieldCredentialList:
		list, _ := v.([]any)
		out := make([]string, 0, len(list))
		for _, x := range list {
			id, _ := x.(string)
			out = append(out, credentialText(id, creds))
		}
		return strings.Join(out, "; ")
	}
	return plain(v)
}

// credentialText is a credential by its type and name, and its id.
func credentialText(id string, creds map[string]protocol.Credential) string {
	c, ok := creds[id]
	if !ok {
		return id
	}
	return strings.TrimSuffix(c.Type+" "+c.Name, " ") + " (" + id + ")"
}

// plain is a JSON value in words: a number as written, a list joined, an
// object as JSON; "(empty)" for an empty string.
func plain(v any) string {
	switch x := v.(type) {
	case nil:
		return "(none)"
	case string:
		if x == "" {
			return "(empty)"
		}
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		out := make([]string, len(x))
		for i, item := range x {
			out[i] = plain(item)
		}
		return strings.Join(out, ", ")
	case map[string]any:
		b, _ := json.Marshal(x)
		return string(b)
	}
	return fmt.Sprint(v)
}

// failoverText is a build's failover policy in a line.
func failoverText(f protocol.BuildFailover) string {
	if !f.Enabled {
		return "off: only an operator's switch moves the minion"
	}
	parts := []string{fmt.Sprintf("on: the minion moves after %d failures in a row", f.FailAfter)}
	if f.FailFor != "" && f.FailFor != "0s" {
		parts[0] += " over " + f.FailFor
	}
	if f.Wrap {
		parts = append(parts, "after the last connection, the first again")
	}
	if f.CyclePause != "" && f.CyclePause != "0s" {
		parts = append(parts, f.CyclePause+" pause after a round that all failed")
	}
	if f.FailbackAfter != "" && f.FailbackAfter != "0s" {
		parts = append(parts, "back to the first after "+f.FailbackAfter)
	}
	return strings.Join(parts, "; ")
}

// wrap joins words with ", " into lines of at most width characters (a
// longer word on a line of its own).
func wrap(words []string, width int) []string {
	var lines []string
	line := ""
	for i, word := range words {
		if i < len(words)-1 {
			word += ","
		}
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) > width:
			lines = append(lines, line)
			line = word
		default:
			line += " " + word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// oneLine is s with its control characters (a tab, a line break) as spaces:
// each value stays on its row.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// when is a time as the console shows times: 2026.11.22 10:00:00 UTC.
func when(t time.Time) string { return t.UTC().Format("2006.01.02 15:04:05") + " UTC" }

// took is how long something took: to the millisecond below a second, else
// to the second.
func took(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// actor is who did something: a user by name, else by id, and core or an
// agent so.
func actor(a protocol.Actor) string {
	who := cmp.Or(a.Name, a.ID)
	if a.Type != "" && a.Type != "user" {
		who += " (" + a.Type + ")"
	}
	return who
}

// byteSize is a size in bytes as people read it: 3 B, 12.5 KiB, 2.1 MiB.
func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
