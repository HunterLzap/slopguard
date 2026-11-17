package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func TestParsePorcelainStatus(t *testing.T) {
	data := " M a.txt\x00?? b.txt\x00R  new.txt\x00old.txt\x00A  c.txt\x00"
	changes := ParsePorcelainStatus(data)
	if len(changes) != 4 {
		t.Fatalf("len(changes) = %d, want 4: %+v", len(changes), changes)
	}
	if changes[0].Path != filepath.FromSlash("a.txt") || changes[0].Status != " M" {
		t.Fatalf("changes[0] = %+v", changes[0])
	}
	if changes[1].Path != filepath.FromSlash("b.txt") || changes[1].Status != "??" {
		t.Fatalf("changes[1] = %+v", changes[1])
	}
	if changes[2].Path != filepath.FromSlash("new.txt") || changes[2].OldPath != filepath.FromSlash("old.txt") {
		t.Fatalf("rename parsed wrong: %+v", changes[2])
	}
	if changes[3].Path != filepath.FromSlash("c.txt") || changes[3].Status != "A " {
		t.Fatalf("changes[3] = %+v", changes[3])
	}
}

func TestParsePorcelainStatusSkipsShortTokens(t *testing.T) {
	if got := ParsePorcelainStatus("\x00 M \x00"); len(got) != 0 {
		t.Fatalf("changes = %+v, want none", got)
	}
}

func TestParseNameOnly(t *testing.T) {
	got := ParseNameOnly("a.txt\x00dir/b.txt\x00")
	want := []string{filepath.FromSlash("a.txt"), filepath.FromSlash("dir/b.txt")}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGitStatusSpec(t *testing.T) {
	runner := &stubRunner{runFn: func(contract.RunSpec) contract.RunResult {
		return contract.RunResult{ExitCode: 0, Stdout: " M a.txt\x00"}
	}}
	git := NewGit(runner, `C:\repo`)
	changes, err := git.Status(context.Background())
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "a.txt" {
		t.Fatalf("changes = %+v", changes)
	}
	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].Name != "git" || calls[0].Dir != `C:\repo` {
		t.Fatalf("call = %+v", calls[0])
	}
	wantArgs := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}
	if len(calls[0].Args) != len(wantArgs) {
		t.Fatalf("args = %v", calls[0].Args)
	}
	for i := range wantArgs {
		if calls[0].Args[i] != wantArgs[i] {
			t.Fatalf("args = %v, want %v", calls[0].Args, wantArgs)
		}
	}
}

func TestGitIsRepository(t *testing.T) {
	runner := &stubRunner{runFn: func(contract.RunSpec) contract.RunResult {
		return contract.RunResult{ExitCode: 0, Stdout: "true\n"}
	}}
	if !NewGit(runner, ".").IsRepository(context.Background()) {
		t.Fatal("IsRepository = false, want true")
	}

	runner.runFn = func(contract.RunSpec) contract.RunResult {
		return contract.RunResult{ExitCode: 128, Stderr: "fatal"}
	}
	if NewGit(runner, ".").IsRepository(context.Background()) {
		t.Fatal("IsRepository = true, want false")
	}
}

func TestGitChangedFilesMergesAndFilters(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runner := &stubRunner{runFn: func(spec contract.RunSpec) contract.RunResult {
		joined := strings.Join(spec.Args, " ")
		switch {
		case strings.Contains(joined, "status"):
			return contract.RunResult{ExitCode: 0, Stdout: " M a.txt\x00 M deleted.txt\x00?? b.txt\x00"}
		case strings.Contains(joined, "rev-parse"):
			return contract.RunResult{ExitCode: 0, Stdout: "true\n"}
		case strings.Contains(joined, "diff"):
			return contract.RunResult{ExitCode: 0, Stdout: "b.txt\x00missing.txt\x00"}
		default:
			t.Fatalf("unexpected git call: %v", spec.Args)
			return contract.RunResult{}
		}
	}}
	files, err := NewGit(runner, dir).ChangedFiles(context.Background())
	if err != nil {
		t.Fatalf("ChangedFiles error: %v", err)
	}
	want := []string{"a.txt", "b.txt"}
	if len(files) != len(want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("files = %v, want %v", files, want)
		}
	}
}

func TestGitChangedFilesWithoutHEAD(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	diffCalled := false
	runner := &stubRunner{runFn: func(spec contract.RunSpec) contract.RunResult {
		joined := strings.Join(spec.Args, " ")
		switch {
		case strings.Contains(joined, "status"):
			return contract.RunResult{ExitCode: 0, Stdout: "?? untracked.txt\x00"}
		case strings.Contains(joined, "rev-parse"):
			return contract.RunResult{ExitCode: 128, Stderr: "no HEAD"}
		case strings.Contains(joined, "diff"):
			diffCalled = true
			return contract.RunResult{ExitCode: 0}
		default:
			return contract.RunResult{ExitCode: 1}
		}
	}}
	files, err := NewGit(runner, dir).ChangedFiles(context.Background())
	if err != nil {
		t.Fatalf("ChangedFiles error: %v", err)
	}
	if len(files) != 1 || files[0] != "untracked.txt" {
		t.Fatalf("files = %v", files)
	}
	if diffCalled {
		t.Fatal("git diff should not be called when HEAD is absent")
	}
}

func TestGitOutputError(t *testing.T) {
	runner := &stubRunner{runFn: func(contract.RunSpec) contract.RunResult {
		return contract.RunResult{ExitCode: 128, Stderr: "fatal: not a repo"}
	}}
	_, err := NewGit(runner, ".").Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "fatal: not a repo") {
		t.Fatalf("err = %v, want git stderr in error", err)
	}
}

func TestGitMissingExecutable(t *testing.T) {
	runner := &stubRunner{runFn: func(contract.RunSpec) contract.RunResult {
		return contract.RunResult{Skipped: true, ExitCode: -1}
	}}
	_, err := NewGit(runner, ".").Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "git executable not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestGitNilRunner(t *testing.T) {
	_, err := NewGit(nil, ".").Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "runner is nil") {
		t.Fatalf("err = %v", err)
	}
}

func TestListFilesFiltersAndSkips(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"src/a.TS":              "x",
		"src/b.py":              "x",
		"node_modules/pkg/x.ts": "x",
		"dist/out.ts":           "x",
		".git/config":           "x",
		"README.md":             "x",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListFiles(dir, []string{".ts"})
	if err != nil {
		t.Fatalf("ListFiles error: %v", err)
	}
	if len(got) != 1 || got[0] != filepath.Join("src", "a.TS") {
		t.Fatalf("files = %v", got)
	}

	all, err := ListFiles(dir, nil)
	if err != nil {
		t.Fatalf("ListFiles(nil) error: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all files = %v, want 3", all)
	}
}

func TestListFilesErrors(t *testing.T) {
	if _, err := ListFiles(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Fatal("expected error for missing directory")
	}
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ListFiles(file, nil); err == nil {
		t.Fatal("expected error for non-directory root")
	}
}
