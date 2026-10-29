// Command minion is the program BOB compiles for its linux-elf target: a
// dummy minion that prints the description it was built with and exits. It is
// a module of its own: BOB copies this directory for each build, writes the
// build's description to minion.json, so it is baked into the binary, and
// runs go build there. The minion.json kept here is a placeholder.
package main

import (
	_ "embed"
	"os"
)

//go:embed minion.json
var minion []byte

func main() {
	_, _ = os.Stdout.Write(minion)
}
