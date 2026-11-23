package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary required")
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "hook@example.com"},
		{"config", "user.name", "HookTester"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestHookInstallPreservesCustomHookIsIdempotentAndUninstallsCleanly(t *testing.T) {
	repo := t.TempDir()
	initTestGitRepo(t, repo)

	oldEnv := currentHookEnv
	currentHookEnv = hookEnv{
		executable: func() (string, error) { return "/usr/local/bin/slopguard", nil },
		lookPath:   func(string) (string, error) { return "/usr/local/bin/slopguard", nil },
	}
	t.Cleanup(func() { currentHookEnv = oldEnv })

	hookPath := filepath.Join(repo, ".git", "hooks", "pre-commit")
	custom := "#!/bin/sh\necho custom-pre-commit\n"
	writeFile(t, hookPath, custom)

	// Install idempotently twice.
	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		if code := Execute([]string{"hook", "install", "--pre-commit", "--root", repo}, nil, &stdout, &stderr); code != ExitOK {
			t.Fatalf("install #%d exit = %d, stderr = %s", i+1, code, stderr.String())
		}
	}

	raw, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "echo custom-pre-commit") {
		t.Fatalf("custom pre-commit content lost:\n%s", text)
	}
	if strings.Count(text, preCommitHookStartMarker) != 1 || strings.Count(text, preCommitHookEndMarker) != 1 {
		t.Fatalf("expected exactly one managed marker block, got:\n%s", text)
	}
	if !strings.Contains(text, `export PATH="$PATH:/opt/homebrew/bin:/usr/local/bin:$HOME/.local/bin:$HOME/go/bin"`) {
		t.Fatalf("hook missing non-interactive PATH fallback:\n%s", text)
	}
	if !strings.Contains(text, "verify --staged --root") {
		t.Fatalf("hook missing verify --staged:\n%s", text)
	}
	if strings.Index(text, "export PATH=") > strings.Index(text, "verify --staged --root") {
		t.Fatalf("PATH fallback must appear before invoking slopguard:\n%s", text)
	}
	if strings.Contains(text, "nohup") || strings.Contains(text, " &") {
		t.Fatalf("pre-commit hook must be synchronous and blocking, found background syntax:\n%s", text)
	}

	// Uninstall preserves custom script and removes only managed block.
	var stdout, stderr bytes.Buffer
	if code := Execute([]string{"hook", "uninstall", "--root", repo}, nil, &stdout, &stderr); code != ExitOK {
		t.Fatalf("uninstall exit = %d, stderr = %s", code, stderr.String())
	}
	after, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(after)) != strings.TrimSpace(custom) {
		t.Fatalf("after uninstall = %q, want %q", string(after), custom)
	}
}

func TestHookInstallAndUninstallFreshRepoAndBlockingCommit(t *testing.T) {
	repo := t.TempDir()
	initTestGitRepo(t, repo)

	// Create a tiny fake slopguard executable script that delegates `verify --staged` exit code via sentinel file.
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "slopguard")
	sentinel := filepath.Join(repo, ".block-commit")
	script := "#!/bin/sh\nif [ -f \"" + sentinel + "\" ]; then echo 'blocked by slopguard' >&2; exit 1; fi\nexit 0\n"
	writeFile(t, fakeBin, script)
	if err := os.Chmod(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}

	oldEnv := currentHookEnv
	currentHookEnv = hookEnv{
		executable: func() (string, error) { return fakeBin, nil },
		lookPath:   func(string) (string, error) { return fakeBin, nil },
	}
	t.Cleanup(func() { currentHookEnv = oldEnv })

	var stdout, stderr bytes.Buffer
	if code := Execute([]string{"hook", "install", "--root", repo}, nil, &stdout, &stderr); code != ExitOK {
		t.Fatalf("install exit = %d, stderr = %s", code, stderr.String())
	}

	// Stage a file and turn on block sentinel -> git commit MUST fail!
	writeFile(t, filepath.Join(repo, "hello.txt"), "hi\n")
	writeFile(t, sentinel, "1")
	addCmd := exec.Command("git", "add", "hello.txt")
	addCmd.Dir = repo
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	failCommit := exec.Command("git", "commit", "-m", "should block")
	failCommit.Dir = repo
	if out, err := failCommit.CombinedOutput(); err == nil {
		t.Fatalf("expected git commit to be blocked by pre-commit hook, succeeded:\n%s", out)
	}

	// Remove block sentinel -> git commit succeeds!
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	okCommit := exec.Command("git", "commit", "-m", "should pass")
	okCommit.Dir = repo
	if out, err := okCommit.CombinedOutput(); err != nil {
		t.Fatalf("expected git commit to pass, got %v\n%s", err, out)
	}

	// Uninstall on fresh hook deletes the empty hook file.
	stdout.Reset()
	stderr.Reset()
	if code := Execute([]string{"hook", "uninstall", "--root", repo}, nil, &stdout, &stderr); code != ExitOK {
		t.Fatalf("uninstall exit = %d, stderr = %s", code, stderr.String())
	}
	hookPath := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
		t.Fatalf("expected empty pre-commit hook file to be removed, stat err = %v", err)
	}
}

func TestHookInstallNotGitRepoFailsExit2(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Execute([]string{"hook", "install", "--root", dir}, nil, &stdout, &stderr); code != ExitError {
		t.Fatalf("code = %d, want 2; stderr = %s", code, stderr.String())
	}
}
