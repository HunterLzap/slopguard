package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProcessSpec configures a governed external command execution backed by
// process-group lifecycle management and bounded output readers.
type ProcessSpec struct {
	Command          string
	Args             []string
	Dir              string
	Env              []string // When non-nil, overrides process environment verbatim.
	Stdin            []byte
	Timeout          time.Duration
	MaxOutputBytes   int
	FailOnTruncation bool
}

// ProcessResult holds the outcome of RunProcess or RunStdioSession.
type ProcessResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
	Duration  time.Duration
	Err       error
}

func resolveCommandExecutable(command string) (string, error) {
	name := strings.TrimSpace(command)
	if name == "" {
		return "", ErrEmptyCommand
	}
	if strings.ContainsRune(name, filepath.Separator) || filepath.IsAbs(name) {
		info, err := os.Stat(name)
		if err != nil {
			return "", fmt.Errorf("engine: command %s: %w", name, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("engine: command %s is a directory", name)
		}
		return name, nil
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("engine: resolve %s: %w", name, err)
	}
	return path, nil
}

// RunProcess executes an external command with a dedicated process group,
// bounded streaming stdout/stderr buffers, and reliable grandchild cleanup.
func RunProcess(ctx context.Context, spec ProcessSpec) ProcessResult {
	started := time.Now()
	res := ProcessResult{ExitCode: -1}
	finish := func() ProcessResult {
		res.Duration = time.Since(started)
		return res
	}

	if ctx == nil {
		ctx = context.Background()
	}
	path, err := resolveCommandExecutable(spec.Command)
	if err != nil {
		res.Err = err
		return finish()
	}
	if err := ctx.Err(); err != nil {
		res.Err = fmt.Errorf("engine: %s canceled before start: %w", spec.Command, err)
		return finish()
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newProcessCommand(path, spec.Args)
	cmd.Dir = spec.Dir
	if spec.Env != nil {
		cmd.Env = spec.Env
	}
	if len(spec.Stdin) > 0 {
		cmd.Stdin = strings.NewReader(string(spec.Stdin))
	}
	configureProcessGroup(cmd)

	outR, outW, err := os.Pipe()
	if err != nil {
		res.Err = fmt.Errorf("engine: stdout pipe for %s: %w", spec.Command, err)
		return finish()
	}
	defer outR.Close()
	defer outW.Close()

	errR, errW, err := os.Pipe()
	if err != nil {
		res.Err = fmt.Errorf("engine: stderr pipe for %s: %w", spec.Command, err)
		return finish()
	}
	defer errR.Close()
	defer errW.Close()

	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		outW.Close()
		errW.Close()
		res.Err = fmt.Errorf("engine: start %s: %w", spec.Command, err)
		return finish()
	}
	outW.Close()
	errW.Close()

	proc := attachProcess(cmd)
	defer proc.release()

	maxOut := spec.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = MaxOutputBytes
	}
	outBuf := newCappedBuffer(maxOut)
	errBuf := newCappedBuffer(maxOut)

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		scanStream(outR, outBuf)
	}()
	go func() {
		defer readers.Done()
		scanStream(errR, errBuf)
	}()
	readDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(readDone)
	}()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var waitErr error
	canceled := false
	select {
	case waitErr = <-waitCh:
	case <-runCtx.Done():
		canceled = true
		proc.kill()
		waitErr = <-waitCh
	}

	select {
	case <-readDone:
	case <-time.After(readDrainGrace):
		proc.kill()
		_ = outR.Close()
		_ = errR.Close()
		<-readDone
	}

	res.Stdout = outBuf.String()
	res.Stderr = errBuf.String()
	res.Truncated = outBuf.truncated() || errBuf.truncated()

	if canceled {
		res.ExitCode = -1
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			res.Err = fmt.Errorf("engine: %s timed out after %s", spec.Command, timeout)
		} else {
			res.Err = fmt.Errorf("engine: %s canceled: %w", spec.Command, runCtx.Err())
		}
		return finish()
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			if res.ExitCode == 0 {
				res.ExitCode = 1
			}
			return finish()
		}
		res.Err = fmt.Errorf("engine: wait %s: %w", spec.Command, waitErr)
		return finish()
	}
	res.ExitCode = 0
	if spec.FailOnTruncation && res.Truncated {
		res.Err = fmt.Errorf("engine: %s output exceeded limit (%d bytes)", spec.Command, maxOut)
	}
	return finish()
}

// RunStdioSession runs an interactive stdin/stdout child process (such as MCP stdio)
// with dedicated process-group cleanup, bounded stderr draining, and strict timeout.
func RunStdioSession(
	ctx context.Context,
	spec ProcessSpec,
	talk func(stdin io.WriteCloser, stdout io.Reader) error,
) ProcessResult {
	started := time.Now()
	res := ProcessResult{ExitCode: -1}
	finish := func() ProcessResult {
		res.Duration = time.Since(started)
		return res
	}

	if talk == nil {
		res.Err = errors.New("engine: nil stdio talk callback")
		return finish()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	path, err := resolveCommandExecutable(spec.Command)
	if err != nil {
		res.Err = err
		return finish()
	}
	if err := ctx.Err(); err != nil {
		res.Err = fmt.Errorf("engine: %s canceled before start: %w", spec.Command, err)
		return finish()
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newProcessCommand(path, spec.Args)
	cmd.Dir = spec.Dir
	if spec.Env != nil {
		cmd.Env = spec.Env
	}
	configureProcessGroup(cmd)

	inR, inW, err := os.Pipe()
	if err != nil {
		res.Err = fmt.Errorf("engine: stdin pipe for %s: %w", spec.Command, err)
		return finish()
	}
	defer inR.Close()
	defer inW.Close()

	outR, outW, err := os.Pipe()
	if err != nil {
		res.Err = fmt.Errorf("engine: stdout pipe for %s: %w", spec.Command, err)
		return finish()
	}
	defer outR.Close()
	defer outW.Close()

	errR, errW, err := os.Pipe()
	if err != nil {
		res.Err = fmt.Errorf("engine: stderr pipe for %s: %w", spec.Command, err)
		return finish()
	}
	defer errR.Close()
	defer errW.Close()

	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		inR.Close()
		outW.Close()
		errW.Close()
		res.Err = fmt.Errorf("engine: start %s: %w", spec.Command, err)
		return finish()
	}
	inR.Close()
	outW.Close()
	errW.Close()

	proc := attachProcess(cmd)
	defer proc.release()

	maxOut := spec.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = 64 * 1024
	}
	errBuf := newCappedBuffer(maxOut)
	errDone := make(chan struct{})
	go func() {
		scanStream(errR, errBuf)
		close(errDone)
	}()

	// Ensure timeout/cancellation aborts blocked talk() reads immediately by
	// closing pipes and terminating the process group.
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-runCtx.Done():
			proc.kill()
			_ = inW.Close()
			_ = outR.Close()
			_ = errR.Close()
		case <-stopWatch:
		}
	}()

	talkErr := talk(inW, outR)
	_ = inW.Close()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-time.After(readDrainGrace):
		proc.kill()
		_ = outR.Close()
		waitErr = <-waitCh
	case <-runCtx.Done():
		proc.kill()
		_ = outR.Close()
		waitErr = <-waitCh
	}

	close(stopWatch)
	<-watchDone

	select {
	case <-errDone:
	case <-time.After(readDrainGrace):
		proc.kill()
		_ = errR.Close()
		<-errDone
	}

	res.Stderr = errBuf.String()
	res.Truncated = errBuf.truncated()

	if talkErr != nil {
		res.Err = talkErr
		return finish()
	}
	if runCtx.Err() != nil {
		res.Err = runCtx.Err()
		return finish()
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return finish()
		}
		res.Err = waitErr
		return finish()
	}
	res.ExitCode = 0
	return finish()
}

func (b *cappedBuffer) truncated() bool {
	return b != nil && b.total > int64(len(b.head)+len(b.tail))
}
