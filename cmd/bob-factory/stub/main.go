// Command stub is the program BOB compiles for its linux-elf target: a dummy
// minion that prints the description it was built with and exits. BOB writes
// that description to minion.json before compiling, so it is baked into the
// binary; the minion.json kept here is a placeholder for builds of this
// repository.
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
