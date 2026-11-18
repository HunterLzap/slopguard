package cli

import (
	"context"
	"flag"
	"io"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type dashboardOptions struct {
	port   int
	noOpen bool
}

// runDashboard is assigned to the production implementation in wire.go.
var runDashboard func(ctx context.Context, runner contract.Runner, opts dashboardOptions, stdout io.Writer) error

func runUI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 7788, "local dashboard port (127.0.0.1 only)")
	noOpen := fs.Bool("no-open", false, "do not open the system browser")
	help, err := parseFlags(fs, args)
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	if help {
		return ExitOK
	}
	if *port < 1 || *port > 65535 {
		return commandError(stderr, "--port must be between 1 and 65535")
	}
	deps, err := newDependencies()
	if err != nil {
		return commandError(stderr, "prepare dashboard: %v", err)
	}
	if runDashboard == nil {
		return commandError(stderr, "dashboard is not wired")
	}
	ctx, stop := newCommandContext(deps.engine)
	defer stop()
	if err := runDashboard(ctx, deps.runner, dashboardOptions{port: *port, noOpen: *noOpen}, stdout); err != nil {
		return commandError(stderr, "dashboard: %v", err)
	}
	return ExitOK
}
