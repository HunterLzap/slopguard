//go:build !windows

package core

import (
	"context"
	"os/exec"
)

func processCommand(ctx context.Context, bin string, args []string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, bin, args...), nil
}
