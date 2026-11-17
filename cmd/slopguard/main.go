// SlopGuard CLI entrypoint. Phase 1: wires subcommands; logic lives in internal/cli.
package main

import (
	"os"

	"github.com/HunterLzap/slopguard/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
