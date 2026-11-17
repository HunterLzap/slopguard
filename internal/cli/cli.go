// Package cli implements the SlopGuard command-line interface.
//
// The CLI layer only parses arguments, orchestrates calls into the core
// engine/adapter packages, and formats user-facing output. All detection and
// scanning business logic lives in the engine and adapter layers.
package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// Exit codes are part of the SlopGuard CLI contract (see docs/architecture.md).
const (
	// ExitOK means the command succeeded and no findings were reported.
	ExitOK = 0
	// ExitFindings means verification ran and reported at least one finding.
	ExitFindings = 1
	// ExitError means a toolchain or configuration error prevented the check.
	ExitError = 2
)

// Execute runs the CLI with args (excluding the program name) and returns the
// process exit code. Input and output are injected so the whole command surface
// stays testable.
func Execute(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return ExitError
	}

	switch args[0] {
	case "init":
		return runInit(args[1:], stdin, stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "baseline":
		return runBaseline(args[1:], stdin, stdout, stderr)
	case "radar":
		return runRadar(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "slopguard: unknown command %q\n", args[0])
		printUsage(stderr)
		return ExitError
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `SlopGuard - deterministic AI code-quality guardrails

Usage:
  slopguard init     [--yes] [--root DIR]
  slopguard verify   [--all] [--json] [--jobs N] [--record] [--root DIR]
  slopguard baseline [--tighten | --reset] [--yes] [--root DIR]
  slopguard radar    [--top N] [--json] [--root DIR]

Exit codes:
  0  checks passed
  1  checks reported findings
  2  toolchain or configuration error
`)
}

// parseFlags is a small helper that normalizes flag.ErrHelp handling.
func parseFlags(fs *flag.FlagSet, args []string) (help bool, err error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return false, err
	}
	if fs.NArg() != 0 {
		return false, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return false, nil
}

func commandError(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "slopguard: "+format+"\n", args...)
	return ExitError
}

// confirm reads one line from stdin and reports whether the user answered yes.
// EOF, empty input and any other answer mean no; automation stays non-destructive.
func confirm(stdin io.Reader) bool {
	if stdin == nil {
		return false
	}
	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes"
}
