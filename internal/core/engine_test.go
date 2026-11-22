package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type fakeRunner struct {
	bins []string
	run  func(dir, bin string, args []string) (ProcessResult, error)
}

func (f fakeRunner) LookPath(bin string) (string, error) {
	if slices.Contains(f.bins, bin) {
		return bin, nil
	}
	return "", exec.ErrNotFound
}

func (f fakeRunner) Run(_ context.Context, dir, bin string, args ...string) (ProcessResult, error) {
	return f.run(dir, bin, args)
}

func fixture(t *testing.T, dir, name, data string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyStatuses(t *testing.T) {
	for _, test := range []struct {
		name, file string
		scope      contract.VerifyScope
		bins       []string
		rules      bool
		goError    bool
		status     contract.VerifyStatus
		code       int
	}{
		{name: "unsupported", file: "README.txt", scope: contract.ScopeAll, status: contract.Unsupported, code: 2},
		{name: "no changes", scope: contract.ScopeChanged, bins: []string{"git"}, status: contract.NoChanges},
		{name: "missing", file: "main.go", scope: contract.ScopeAll, status: contract.MissingTool, code: 2},
		{name: "go passes but Semgrep is missing", file: "main.go", scope: contract.ScopeAll, bins: []string{"go"}, rules: true, status: contract.MissingTool, code: 2},
		{name: "go fails and Semgrep is missing", file: "main.go", scope: contract.ScopeAll, bins: []string{"go"}, rules: true, goError: true, status: contract.Fail, code: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, err := ProjectPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if test.file != "" {
				fixture(t, dir, test.file, "package demo\n")
			}
			if test.rules {
				fixture(t, RulesDir(dir), "local.yaml", "rules: []")
			}
			runner := fakeRunner{bins: test.bins, run: func(dir, bin string, args []string) (ProcessResult, error) {
				switch {
				case bin == "git" && args[0] == "rev-parse":
					return ProcessResult{Stdout: dir + "\n"}, nil
				case bin == "git" && args[0] == "status":
					return ProcessResult{}, nil
				case bin == "go" && test.goError:
					return ProcessResult{ExitCode: 1, Stderr: "main.go:1:2: broken code"}, nil
				case bin == "go":
					return ProcessResult{}, nil
				default:
					return ProcessResult{}, fmt.Errorf("unexpected tool %s %v", bin, args)
				}
			}}
			r, err := (Engine{Runner: runner}).Verify(context.Background(), dir, test.scope)
			if err != nil || r.Status != test.status || r.ExitCode != test.code {
				t.Fatalf("verify = %+v, %v", r, err)
			}
			if test.rules && !slices.Contains(r.MissingTools, SemgrepTool) {
				t.Fatalf("missing tools = %v", r.MissingTools)
			}
		})
	}
}

func TestVerifyMixedProject(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, filepath.Join("services", "api", "go.mod"), "module example.test/api\n")
	fixture(t, dir, filepath.Join("services", "api", "main.go"), "package main\n")
	fixture(t, dir, filepath.Join("clients", "web", "tsconfig.json"), "{}")
	fixture(t, dir, filepath.Join("clients", "web", "hello.ts"), "export const x: number = 1;\n")
	runner := fakeRunner{bins: []string{"go", "tsc", "eslint"}, run: func(dir, bin string, args []string) (ProcessResult, error) {
		if bin == "eslint" {
			return ProcessResult{Stdout: "[]"}, nil
		}
		want := map[string]string{"go": "api", "tsc": "web"}[bin]
		if filepath.Base(dir) != want {
			return ProcessResult{}, fmt.Errorf("%s ran in %s, want %s", bin, dir, want)
		}
		return ProcessResult{}, nil
	}}
	r, err := (Engine{Runner: runner}).Verify(context.Background(), dir, contract.ScopeAll)
	if err != nil || r.Status != contract.Pass || r.ScannedFilesCount != 2 ||
		!reflect.DeepEqual(r.DetectedLanguages, []string{"go", "typescript"}) || len(r.ExecutedCheckers) != 3 ||
		len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "golangci-lint") {
		t.Fatalf("verify = %+v, %v", r, err)
	}
}

func TestVerifyNativeGoVet(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "go.mod", "module example.test/fixture\n\ngo 1.23\n")
	fixture(t, dir, "broken.go", "package fixture\nimport \"fmt\"\nfunc broken(){fmt.Printf(\"%d\", \"wrong\")}\n")
	r, err := (Engine{Runner: ExecRunner{}}).Verify(context.Background(), dir, contract.ScopeAll)
	if err != nil || r.Status != contract.Fail || r.ExitCode != 1 || len(r.Diagnostics) == 0 {
		t.Fatalf("verify = %+v, %v", r, err)
	}
	d := r.Diagnostics[0]
	if d.File != "broken.go" || d.Line != 3 || !strings.Contains(d.CodeSnippet, "fmt.Printf") {
		t.Fatalf("missing Go diagnostic context: %+v", d)
	}
}

func TestChangedFiles(t *testing.T) {
	repo, err := ProjectPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, "src")
	runner := fakeRunner{bins: []string{"git"}, run: func(_, _ string, args []string) (ProcessResult, error) {
		if args[0] == "rev-parse" {
			return ProcessResult{Stdout: filepath.ToSlash(repo) + "\n"}, nil
		}
		return ProcessResult{Stdout: "M  src/staged.go\x00 M src/unstaged.go\x00?? src/new space.ts\x00R  src/new.go\x00src/old.go\x00 M outside.go\x00"}, nil
	}}
	got, err := ChangedFiles(context.Background(), runner, dir)
	want := []string{"new space.ts", "new.go", "staged.go", "unstaged.go"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v, %v", got, err)
	}
	bad := fakeRunner{bins: []string{"git"}, run: func(_, _ string, _ []string) (ProcessResult, error) {
		return ProcessResult{ExitCode: 128, Stderr: "not a git repository"}, nil
	}}
	if _, err := ChangedFiles(context.Background(), bad, dir); err == nil || !strings.Contains(err.Error(), "--scope all") {
		t.Fatalf("non-Git directory error = %v", err)
	}
}
