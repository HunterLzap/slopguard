package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/HunterLzap/slopguard/internal/mcp"
)

func runMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootFlag := fs.String("root", ".", "project root directory")
	help, err := parseFlags(fs, args)
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	if help {
		return ExitOK
	}
	explicitRoot := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "root" {
			explicitRoot = true
		}
	})

	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		return commandError(stderr, "resolve project root: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return commandError(stderr, "project root %s: %v", root, err)
	}
	// Global MCP hosts (e.g. Antigravity / Claude Code) typically spawn `slopguard mcp`
	// with cwd set to `/` or `$HOME` and no `--root`. Leave workspace root unbound so
	// tools resolve from MCP `initialize` roots or explicit `project_path` instead of `/`.
	if !explicitRoot && isDisallowedGlobalMCPDir(root) {
		root = ""
	}

	server := mcp.NewServer(buildMCPTools(root), stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := server.Serve(ctx, stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "slopguard mcp: %v\n", err)
		return ExitError
	}
	return ExitOK
}
