package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// BOB's factory commands (docs: contracts/factory-commands): what BOB runs
// itself, never the minion, in every session of its builds, so they answer
// while the minion is out of reach. Each prints what core gave BOB of the
// session (build_info.go, session_info.go) as a report: sections of aligned
// rows.

// factoryCommands are the code of the schema's factory commands, by id.
var factoryCommands = map[string]factory.FactoryCommand{
	buildInfoCommand.ID:   buildInfo{},
	sessionInfoCommand.ID: sessionInfo{},
}

// report writes a factory command's output: titled sections of rows, a label
// and its value each, indented. Rows that follow each other are aligned
// (text/tabwriter); a line without a tab, a title's, ends them.
type report struct{ tw *tabwriter.Writer }

func newReport(w io.Writer) *report { return &report{tw: tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)} }

// line writes s on a line of its own: a heading, or an item rows follow.
func (r *report) line(s string) { _, _ = fmt.Fprintln(r.tw, oneLine(s)) }

// title starts a section, after a blank line.
func (r *report) title(format string, args ...any) {
	r.line("")
	r.line(fmt.Sprintf(format, args...))
}

// row writes a label and its value, indented.
func (r *report) row(indent, label, value string) {
	_, _ = fmt.Fprintf(r.tw, "%s%s\t%s\n", indent, oneLine(label), oneLine(value))
}

// flush writes out the rows held for their alignment.
func (r *report) flush() { _ = r.tw.Flush() }

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
