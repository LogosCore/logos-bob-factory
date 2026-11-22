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

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// build_info prints the session's build as core keeps it, the record and the
// factory.build request BOB got, by the schema's labels (factory_commands.go).
// What the call carries for the task alone is never printed: the minion's
// keys (their suite is), the minion config (it holds them: its size is),
// credentials' values (their ids, types and names are) and links.
var buildInfoCommand = protocol.FactoryCommand{
	ID:    "build_info",
	Label: "Build info",
	Description: "Prints the session's build as core keeps it: its target, values, commands, connections, exit codes and " +
		"artifact. BOB runs it, not the minion: it answers while the minion is out of reach. Keys and secrets are left out.",
}

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
	r := newReport(w)
	defer r.flush()
	row, title := r.row, r.title

	r.line(fmt.Sprintf("Build %s (session #%d)", b.BuildID, s.Number))
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
		r.line("  " + line)
	}

	title("Connections (%d)", len(req.Connections))
	for i, c := range req.Connections {
		e := c.Endpoint
		r.line(fmt.Sprintf("  %d. %s: %s v%d, %s", i+1, e.Name, e.Channel.ModuleName, e.Channel.SchemaVersion, e.Kind))
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
		r.line("  " + failoverText(*f))
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
