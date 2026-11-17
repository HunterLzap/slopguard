package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

const (
	helperEnv      = "SLOPGUARD_TEST_HELPER"
	helperModeEnv  = "SLOPGUARD_HELPER_MODE"
	helperCodeEnv  = "SLOPGUARD_HELPER_CODE"
	helperTextEnv  = "SLOPGUARD_HELPER_TEXT"
	helperBytesEnv = "SLOPGUARD_HELPER_BYTES"
	helperPIDSEnv  = "SLOPGUARD_HELPER_PIDS"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		helperMain()
		return
	}
	os.Exit(m.Run())
}

// helperMain implements fake child commands for ExecRunner tests.
func helperMain() {
	switch os.Getenv(helperModeEnv) {
	case "echo":
		fmt.Fprint(os.Stdout, os.Getenv(helperTextEnv))
		os.Exit(0)
	case "exit":
		fmt.Fprint(os.Stdout, "stdout-line\n")
		fmt.Fprint(os.Stderr, "stderr-line\n")
		code, _ := strconv.Atoi(os.Getenv(helperCodeEnv))
		os.Exit(code)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "large":
		n, _ := strconv.Atoi(os.Getenv(helperBytesEnv))
		chunk := strings.Repeat("A", 4096)
		written := 0
		for written < n {
			take := len(chunk)
			if n-written < take {
				take = n - written
			}
			os.Stdout.WriteString(chunk[:take])
			written += take
		}
		os.Stdout.WriteString("TAILMARK")
		os.Exit(0)
	case "tree":
		self, err := os.Executable()
		if err != nil {
			os.Exit(9)
		}
		child := exec.Command(self)
		child.Env = append(os.Environ(), helperEnv+"=1", helperModeEnv+"=sleep")
		if err := child.Start(); err != nil {
			os.Exit(8)
		}
		pidFile := os.Getenv(helperPIDSEnv)
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", os.Getenv(helperModeEnv))
		os.Exit(97)
	}
}

func helperSpec(t *testing.T, mode string) contract.RunSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return contract.RunSpec{
		Name: exe,
		Env: map[string]string{
			helperEnv:     "1",
			helperModeEnv: mode,
		},
	}
}

func TestExecRunnerBasic(t *testing.T) {
	runner := NewExecRunner()
	spec := helperSpec(t, "echo")
	spec.Env[helperTextEnv] = "hello world"
	result := runner.Run(context.Background(), spec)
	if result.Err != nil {
		t.Fatalf("Run error: %v", result.Err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "hello world" {
		t.Fatalf("Stdout = %q, want %q", result.Stdout, "hello world")
	}
	if result.Skipped {
		t.Fatal("Skipped = true, want false")
	}
	if result.Duration <= 0 {
		t.Fatal("Duration should be positive")
	}
}

func TestExecRunnerNonZeroExit(t *testing.T) {
	runner := NewExecRunner()
	spec := helperSpec(t, "exit")
	spec.Env[helperCodeEnv] = "7"
	result := runner.Run(context.Background(), spec)
	if result.Err != nil {
		t.Fatalf("Run returned system error: %v", result.Err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "stdout-line") {
		t.Fatalf("Stdout = %q", result.Stdout)
	}
	if !strings.Contains(result.Stderr, "stderr-line") {
		t.Fatalf("Stderr = %q", result.Stderr)
	}
}

func TestExecRunnerEmptyCommand(t *testing.T) {
	result := NewExecRunner().Run(context.Background(), contract.RunSpec{Name: "  "})
	if result.Err == nil || !strings.Contains(result.Err.Error(), "empty command") {
		t.Fatalf("Err = %v, want empty command error", result.Err)
	}
	if result.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", result.ExitCode)
	}
}

func TestExecRunnerMissingTool(t *testing.T) {
	runner := NewExecRunner()
	runner.LookPathFunc = func(string) (string, error) { return "", exec.ErrNotFound }
	result := runner.Run(context.Background(), contract.RunSpec{Name: "definitely-not-installed-tool"})
	if result.Err != nil {
		t.Fatalf("Err = %v, want nil for missing tool", result.Err)
	}
	if !result.Skipped {
		t.Fatal("Skipped = false, want true")
	}
	if result.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", result.ExitCode)
	}
}

func TestExecRunnerEnvMerge(t *testing.T) {
	runner := NewExecRunner()
	spec := helperSpec(t, "echo")
	spec.Env[helperTextEnv] = "env-ok"
	result := runner.Run(context.Background(), spec)
	if result.Stdout != "env-ok" {
		t.Fatalf("Stdout = %q, want env-ok", result.Stdout)
	}
}

func TestExecRunnerOutputTruncation(t *testing.T) {
	runner := NewExecRunner()
	runner.MaxOutputBytes = 4096
	spec := helperSpec(t, "large")
	spec.Env[helperBytesEnv] = "20000"
	result := runner.Run(context.Background(), spec)
	if result.Err != nil {
		t.Fatalf("Run error: %v", result.Err)
	}
	if len(result.Stdout) > 4096 {
		t.Fatalf("Stdout length = %d, want <= 4096", len(result.Stdout))
	}
	if !strings.Contains(result.Stdout, "[slopguard:") || !strings.Contains(result.Stdout, "truncated") {
		t.Fatalf("Stdout missing truncation marker: %q", result.Stdout[:minInt(200, len(result.Stdout))])
	}
	if !strings.HasPrefix(result.Stdout, "AAAA") {
		t.Fatal("Stdout head not preserved")
	}
	if !strings.HasSuffix(result.Stdout, "TAILMARK") {
		t.Fatal("Stdout tail not preserved")
	}
}

func TestExecRunnerTimeout(t *testing.T) {
	runner := NewExecRunner()
	spec := helperSpec(t, "sleep")
	spec.Timeout = 400 * time.Millisecond
	started := time.Now()
	result := runner.Run(context.Background(), spec)
	elapsed := time.Since(started)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "timed out") {
		t.Fatalf("Err = %v, want timeout error", result.Err)
	}
	if result.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", result.ExitCode)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("timeout took %s, want well under 8s", elapsed)
	}
}

func TestExecRunnerTimeoutKillsProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	runner := NewExecRunner()
	spec := helperSpec(t, "tree")
	spec.Env[helperPIDSEnv] = pidFile
	spec.Timeout = 700 * time.Millisecond

	result := runner.Run(context.Background(), spec)
	if result.Err == nil {
		t.Fatalf("Err = nil, want timeout error (exit=%d stdout=%q stderr=%q)", result.ExitCode, result.Stdout, result.Stderr)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", string(raw), err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("grandchild process %d still alive after runner timeout", pid)
}

func TestExecRunnerCancelBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := NewExecRunner().Run(ctx, helperSpec(t, "echo"))
	if result.Err == nil || !strings.Contains(result.Err.Error(), "canceled") {
		t.Fatalf("Err = %v, want canceled error", result.Err)
	}
}

// TestExecRunnerCmdPathAndArgWithSpaces covers the Windows cmd.exe quoting
// trap: a .cmd wrapper whose absolute path contains spaces AND whose argument
// also contains spaces used to be split at the first space.
func TestExecRunnerCmdPathAndArgWithSpaces(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows cmd.exe quoting regression test")
	}
	dir := filepath.Join(t.TempDir(), "dir with space")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "echo args.cmd")
	content := "@echo off\r\necho ARG=%1\r\nexit /b 0\r\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	result := NewExecRunner().Run(context.Background(), contract.RunSpec{
		Name: script,
		Args: []string{"hello world"},
		Dir:  dir,
	})
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("Run = %+v, want success", result)
	}
	if !strings.Contains(result.Stdout, "hello world") {
		t.Fatalf("Stdout = %q, want spaced argument preserved", result.Stdout)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
