package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/HunterLzap/slopguard/internal/bridge"
	"github.com/HunterLzap/slopguard/internal/core"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := bridge.RunCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, core.ExecRunner{}, nil)
	stop()
	os.Exit(code)
}
