package core

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
)

type ProcessResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type ProcessRunner interface {
	LookPath(bin string) (string, error)
	Run(ctx context.Context, dir, bin string, args ...string) (ProcessResult, error)
}

type ExecRunner struct{}

func (ExecRunner) LookPath(bin string) (string, error) { return exec.LookPath(bin) }

func (ExecRunner) Run(ctx context.Context, dir, bin string, args ...string) (ProcessResult, error) {
	cmd, err := processCommand(ctx, bin, args)
	if err != nil {
		return ProcessResult{ExitCode: -1}, err
	}
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	result := ProcessResult{
		Stdout:   normalizeOutput(stdout.String()),
		Stderr:   normalizeOutput(stderr.String()),
		ExitCode: -1,
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return result, nil
	}
	return result, err
}

func normalizeOutput(output string) string {
	return strings.ReplaceAll(output, "\r\n", "\n")
}
