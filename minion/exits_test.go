package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// run returns, before it connects anywhere, with the exit code that says why
// it cannot start; an error without one exits with exitFailed.
func TestRunExitCodes(t *testing.T) {
	keys := &protocol.MinionCrypto{Suite: protocol.CryptoAES256CBCHMACSHA256, EncKey: make([]byte, 32), MACKey: make([]byte, 32)}
	// A channel the minion does not speak: valid in a config, of no use to it.
	elsewhere := protocol.MinionConnection{Channel: 99, Version: 1, Kind: protocol.KindNumReverseCall}
	config := func(c protocol.MinionConfig) []byte {
		t.Helper()
		c.Build = make([]byte, 16)
		raw, err := c.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	cases := []struct {
		name   string
		config []byte
		want   int
	}{
		{"not a config", []byte("not a minion config"), exitConfig},
		{"no config at all", nil, exitConfig},
		{"no keys", config(protocol.MinionConfig{Connections: []protocol.MinionConnection{elsewhere}}), exitNoKeys},
		{"no connection it can use", config(protocol.MinionConfig{Crypto: keys, Connections: []protocol.MinionConnection{elsewhere}}),
			exitNoConnection},
	}
	saved := minion
	t.Cleanup(func() { minion = saved })
	for _, c := range cases {
		minion = c.config
		err := run()
		if got := exitCode(err); got != c.want {
			t.Errorf("%s: exit code %d (%v), want %d", c.name, got, err, c.want)
		}
	}
}

func TestExitCode(t *testing.T) {
	if got := exitCode(errors.New("x")); got != exitFailed {
		t.Errorf("an error without a code: %d, want %d", got, exitFailed)
	}
	wrapped := fmt.Errorf("read the config: %w", withCode(exitConfig, errors.New("x")))
	if got := exitCode(wrapped); got != exitConfig {
		t.Errorf("a wrapped one: %d, want %d", got, exitConfig)
	}
	if wrapped.Error() != "read the config: x" {
		t.Errorf("message %q", wrapped)
	}
}

// Go exits with 2 on a panic nothing recovered: the code BOB declares as
// exitCrashed. The test runs itself again, to panic.
func TestCrashExitCode(t *testing.T) {
	if os.Getenv("BOB_MINION_PANIC") == "1" {
		panic("a crash, on purpose")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashExitCode$")
	cmd.Env = append(os.Environ(), "BOB_MINION_PANIC=1")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != exitCrashed {
		t.Fatalf("a panic: %v, want exit code %d", err, exitCrashed)
	}
}
