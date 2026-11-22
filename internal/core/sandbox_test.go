package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxGuards(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "main.go", "package example\n")
	rule := "rules: fixture"
	for _, test := range []struct {
		name, message                   string
		validateCode, positive, project int
	}{
		{name: "syntax", validateCode: 2, message: "validation"},
		{name: "zero recall", message: "no positive"},
		{name: "broad", positive: 1, project: 21, message: "too broad"},
		{name: "valid", positive: 2, project: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := fakeRunner{bins: []string{"semgrep"}, run: func(cwd, _ string, args []string) (ProcessResult, error) {
				if args[0] == "--validate" {
					return ProcessResult{ExitCode: test.validateCode}, nil
				}
				count := test.project
				if filepath.Base(cwd) == "samples" {
					count = test.positive
				}
				path := filepath.Base(args[len(args)-1])
				return ProcessResult{Stdout: semgrepOutput(t, path, count)}, nil
			}}
			got, err := (Semgrep{Runner: runner}).VerifyRuleInSandbox(context.Background(), dir, rule, []Snippet{{Language: "go", Code: "package example"}})
			if test.message != "" {
				if err == nil || !strings.Contains(err.Error(), test.message) || got.ProofHash != "" {
					t.Fatalf("sandbox = %+v, %v", got, err)
				}
				return
			}
			want := fmt.Sprintf("%x", sha256.Sum256([]byte(rule+"|2|3")))
			if err != nil || got.ProofHash != want || got.PositiveHits != 2 || got.ProjectHits != 3 || got.ProjectHitFiles != 1 || got.ProjectScannedFiles != 1 {
				t.Fatalf("sandbox = %+v, %v", got, err)
			}
		})
	}
}

func semgrepOutput(t *testing.T, path string, count int) string {
	t.Helper()
	hits := make([]map[string]any, count)
	for i := range hits {
		hits[i] = map[string]any{"path": path, "check_id": "example", "start": map[string]int{"line": 1, "col": 1}, "extra": map[string]string{"message": "example"}}
	}
	data, err := json.Marshal(map[string]any{"paths": map[string][]string{"scanned": {path}}, "results": hits})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
