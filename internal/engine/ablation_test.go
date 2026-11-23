package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitExec(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeRepoFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hashTreeFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	hashes := make(map[string]string)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		hashes[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hashes
}

func setupBuggyGoRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary required")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary required")
	}

	repo := t.TempDir()
	writeRepoFile(t, filepath.Join(repo, "go.mod"), "module example.com/ablationdemo\n\ngo 1.23\n")
	// Buggy HEAD: Add returns a - b instead of a + b.
	writeRepoFile(t, filepath.Join(repo, "calc.go"), `package demo

func Add(a, b int) int {
	return a - b
}
`)
	writeRepoFile(t, filepath.Join(repo, "calc_test.go"), `package demo

import "testing"

func TestSanity(t *testing.T) {
	if Add(5, 0) != 5 {
		t.Fatalf("expected 5")
	}
}
`)
	gitExec(t, repo, "init", "-q")
	gitExec(t, repo, "config", "user.email", "ablation@example.com")
	gitExec(t, repo, "config", "user.name", "Ablation")
	gitExec(t, repo, "add", "-A")
	gitExec(t, repo, "commit", "-qm", "initial buggy version")
	return repo
}

func TestAblationRealBugFixRedThenGreenAndCleanWorktree(t *testing.T) {
	repo := setupBuggyGoRepo(t)

	// Fix production bug and add reproducing regression test in working tree.
	writeRepoFile(t, filepath.Join(repo, "calc.go"), `package demo

func Add(a, b int) int {
	return a + b
}
`)
	writeRepoFile(t, filepath.Join(repo, "calc_test.go"), `package demo

import "testing"

func TestAddRegression(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2,3) = %d, want 5", got)
	}
}
`)
	// Also create node_modules & .venv in main repo to verify symlink handling never deletes originals.
	writeRepoFile(t, filepath.Join(repo, "node_modules", "pkg", "marker.txt"), "keep-me")
	writeRepoFile(t, filepath.Join(repo, ".venv", "pyvenv.cfg"), "keep-venv")

	beforeStatus := gitExec(t, repo, "status", "--porcelain")
	beforeHashes := hashTreeFiles(t, repo)

	out, err := RunAblation(context.Background(), NewExecRunner(), AblationOptions{
		ProjectRoot: repo,
		TestCmd:     "go test .",
	})
	if err != nil {
		t.Fatalf("RunAblation error: %v", err)
	}
	if out.Result.Status != "passed" || len(out.Findings) != 0 {
		t.Fatalf("expected passed ablation, got result=%+v findings=%+v", out.Result, out.Findings)
	}

	// Case C: main repo working tree, file hashes, dependencies, and worktree list are completely unpolluted.
	afterStatus := gitExec(t, repo, "status", "--porcelain")
	if afterStatus != beforeStatus {
		t.Fatalf("git status changed after ablation:\nbefore:\n%s\nafter:\n%s", beforeStatus, afterStatus)
	}
	afterHashes := hashTreeFiles(t, repo)
	if len(afterHashes) != len(beforeHashes) {
		t.Fatalf("file count changed: before=%d after=%d", len(beforeHashes), len(afterHashes))
	}
	for k, v := range beforeHashes {
		if afterHashes[k] != v {
			t.Fatalf("file %s modified during ablation", k)
		}
	}
	wtList := strings.TrimSpace(gitExec(t, repo, "worktree", "list"))
	if strings.Count(wtList, "\n") != 0 {
		t.Fatalf("dangling git worktree found:\n%s", wtList)
	}
}

func TestAblationFakeAlwaysPassTestBlockedInRedPhase(t *testing.T) {
	repo := setupBuggyGoRepo(t)

	// Fix production code, but write a fake test that always passes even on buggy HEAD.
	writeRepoFile(t, filepath.Join(repo, "calc.go"), `package demo

func Add(a, b int) int {
	return a + b
}
`)
	writeRepoFile(t, filepath.Join(repo, "calc_test.go"), `package demo

import "testing"

func TestFakeAlwaysPass(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("impossible")
	}
}
`)

	beforeStatus := gitExec(t, repo, "status", "--porcelain")
	beforeHashes := hashTreeFiles(t, repo)

	out, err := RunAblation(context.Background(), NewExecRunner(), AblationOptions{
		ProjectRoot: repo,
		TestCmd:     "go test .",
	})
	if err != nil {
		t.Fatalf("RunAblation error: %v", err)
	}
	if out.Result.Status != "failed" {
		t.Fatalf("status = %q, want failed", out.Result.Status)
	}
	if len(out.Findings) != 1 || out.Findings[0].Rule != AblationNotReproducingRuleID {
		t.Fatalf("findings = %+v, want %s", out.Findings, AblationNotReproducingRuleID)
	}

	if afterStatus := gitExec(t, repo, "status", "--porcelain"); afterStatus != beforeStatus {
		t.Fatalf("git status mutated on fake test block:\nbefore:\n%s\nafter:\n%s", beforeStatus, afterStatus)
	}
	afterHashes := hashTreeFiles(t, repo)
	for k, v := range beforeHashes {
		if afterHashes[k] != v {
			t.Fatalf("file %s modified during failed ablation", k)
		}
	}
	wtList := strings.TrimSpace(gitExec(t, repo, "worktree", "list"))
	if strings.Count(wtList, "\n") != 0 {
		t.Fatalf("dangling git worktree after failure:\n%s", wtList)
	}
}

func TestAblationPreconditionsAndStagedMode(t *testing.T) {
	repo := setupBuggyGoRepo(t)

	// Missing test_cmd returns error.
	if _, err := RunAblation(context.Background(), NewExecRunner(), AblationOptions{ProjectRoot: repo}); err == nil {
		t.Fatal("expected error when test_cmd is empty")
	}

	// Only prod change (no test change) -> ablation:missing_test.
	writeRepoFile(t, filepath.Join(repo, "calc.go"), "package demo\nfunc Add(a, b int) int { return a + b }\n")
	missingOut, err := RunAblation(context.Background(), NewExecRunner(), AblationOptions{
		ProjectRoot: repo,
		TestCmd:     "go test .",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missingOut.Findings) != 1 || missingOut.Findings[0].Rule != AblationMissingTestRuleID {
		t.Fatalf("findings = %+v, want %s", missingOut.Findings, AblationMissingTestRuleID)
	}

	// Test-only change (reset calc.go, modify calc_test.go) -> skipped cleanly.
	gitExec(t, repo, "checkout", "--", "calc.go")
	writeRepoFile(t, filepath.Join(repo, "calc_test.go"), "package demo\nimport \"testing\"\nfunc TestSanity(t *testing.T) {}\n")
	skipOut, err := RunAblation(context.Background(), NewExecRunner(), AblationOptions{
		ProjectRoot: repo,
		TestCmd:     "go test .",
	})
	if err != nil {
		t.Fatal(err)
	}
	if skipOut.Result.Status != "skipped" || len(skipOut.Findings) != 0 {
		t.Fatalf("expected skipped ablation for test-only change, got %+v", skipOut)
	}

	// Staged mode ignores unstaged broken code in working tree.
	writeRepoFile(t, filepath.Join(repo, "calc.go"), "package demo\nfunc Add(a, b int) int { return a + b }\n")
	writeRepoFile(t, filepath.Join(repo, "calc_test.go"), "package demo\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2,3) != 5 { t.Fatal(\"fail\") } }\n")
	gitExec(t, repo, "add", "calc.go", "calc_test.go")
	// Break working tree unstaged file; --staged ablation should still pass using index blobs!
	writeRepoFile(t, filepath.Join(repo, "calc.go"), "BROKEN SYNTAX\n")
	stagedOut, err := RunAblation(context.Background(), NewExecRunner(), AblationOptions{
		ProjectRoot: repo,
		TestCmd:     "go test .",
		Staged:      true,
	})
	if err != nil {
		t.Fatalf("staged ablation error: %v", err)
	}
	if stagedOut.Result.Status != "passed" || len(stagedOut.Findings) != 0 {
		t.Fatalf("expected staged ablation to pass, got %+v", stagedOut)
	}
}
