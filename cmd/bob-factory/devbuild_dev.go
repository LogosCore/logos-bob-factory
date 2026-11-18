//go:build dev

// The development-only build helper: `bob-factory build <factory-build.json>`
// compiles the minion of a saved factory.build request (the JSON core's UI
// downloads for a build) without core, RabbitMQ or a running factory. The
// image never has it: it builds without the dev tag.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

func init() { devCommands["build"] = devBuild }

func devBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	src := fs.String("src", env("MINION_SRC", "./minion"), "the minion's sources")
	outDir := fs.String("o", ".", "directory for the artifact")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: bob-factory build [-src DIR] [-o DIR] <factory-build.json>")
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var req protocol.BuildRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("not a factory.build request: %w", err)
	}
	if p := protocol.ValidateBuildRequest(buildSchema, req); p != nil {
		return fmt.Errorf("the request does not fit BOB's build schema: %s", describeProblems(p))
	}
	a, ok := artifactKinds[req.Target]
	if !ok {
		return fmt.Errorf("unknown target %q", req.Target)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	exits := exitCodes(req)
	if len(exits) > 0 {
		fmt.Fprintf(os.Stderr, "exit codes of its own: %s\n", formatExitCodes(exits))
	}
	bin, err := compile(ctx, *src, req.Target, req.Platform, req.MinionConfig, exits, goarmOf(req), os.Stderr)
	if err != nil {
		return err
	}
	out := filepath.Join(*outDir, "bob-"+req.BuildID[:8]+a.ext)
	if err := os.WriteFile(out, bin, 0o755); err != nil {
		return err
	}
	fmt.Printf("%s (%d bytes)\n", out, len(bin))
	return nil
}

func describeProblems(p map[string]string) string {
	s := ""
	for _, k := range protocol.SortedProblemKeys(p) {
		s += k + ": " + p[k] + "; "
	}
	return s
}
