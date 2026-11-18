package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func dryRunArg(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func dryRunSampleTarget(args []string) string {
	for _, arg := range args {
		if strings.Contains(filepath.Base(arg), "slopguard-dryrun-") {
			return arg
		}
	}
	return ""
}

func eslintDryRunOutput(t *testing.T, files []string, rule string) string {
	t.Helper()
	type message struct {
		RuleID   string `json:"ruleId"`
		Severity int    `json:"severity"`
		Message  string `json:"message"`
		Line     int    `json:"line"`
	}
	type fileResult struct {
		FilePath string    `json:"filePath"`
		Messages []message `json:"messages"`
	}
	results := make([]fileResult, 0, len(files))
	for i, file := range files {
		results = append(results, fileResult{
			FilePath: file,
			Messages: []message{{
				RuleID:   rule,
				Severity: 2,
				Message:  "dry-run hit",
				Line:     i + 1,
			}},
		})
	}
	data, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("marshal eslint output: %v", err)
	}
	return string(data)
}

func TestDryRunRuleZeroHitsWarning(t *testing.T) {
	root := t.TempDir()
	var configPath string
	runner := &stubRunner{runFn: func(spec contract.RunSpec) contract.RunResult {
		configPath = dryRunArg(spec.Args, "--config")
		return contract.RunResult{Stdout: "[]", ExitCode: 0, Duration: 120 * time.Millisecond}
	}}

	result, err := DryRunRule(context.Background(), runner, DryRunOptions{
		Engine:      "eslint",
		RuleID:      "no-debugger",
		RulePayload: `"error"`,
		ProjectRoot: root,
		Files:       []string{"src/app.ts"},
	})
	if err != nil {
		t.Fatalf("DryRunRule error: %v", err)
	}
	if result.Hits != 0 || result.SampleMatched {
		t.Fatalf("result = %+v, want 0 hits and no sample match", result)
	}
	if !strings.Contains(result.Warning, "matched 0 occurrences") {
		t.Fatalf("warning = %q, want 0-hit warning", result.Warning)
	}
	if result.DurationMs != 120 {
		t.Fatalf("duration_ms = %d, want 120", result.DurationMs)
	}
	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(calls))
	}
	joined := strings.Join(calls[0].Args, " ")
	if !strings.Contains(joined, "--no-config-lookup") {
		t.Fatalf("eslint args = %q, want --no-config-lookup for flat fallback", joined)
	}
	if configPath == "" {
		t.Fatal("runner did not receive --config")
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("temp config %s was not cleaned up: %v", configPath, err)
	}
}

func TestDryRunRuleThresholdWarnings(t *testing.T) {
	t.Run("explicit threshold", func(t *testing.T) {
		root := t.TempDir()
		files := []string{"src/a.ts", "src/b.ts", "src/c.ts", "src/d.ts", "src/e.ts", "src/f.ts"}
		runner := &stubRunner{runFn: func(contract.RunSpec) contract.RunResult {
			return contract.RunResult{Stdout: eslintDryRunOutput(t, files, "no-console"), ExitCode: 1}
		}}
		result, err := DryRunRule(context.Background(), runner, DryRunOptions{
			Engine:      "eslint",
			RuleID:      "no-console",
			RulePayload: `"error"`,
			ProjectRoot: root,
			Threshold:   3,
		})
		if err != nil {
			t.Fatalf("DryRunRule error: %v", err)
		}
		if result.Hits != 6 {
			t.Fatalf("hits = %d, want 6", result.Hits)
		}
		if len(result.SampleFiles) != 5 {
			t.Fatalf("sample files = %v, want at most 5", result.SampleFiles)
		}
		if !strings.Contains(result.Warning, "exceeds threshold 3") {
			t.Fatalf("warning = %q, want threshold warning", result.Warning)
		}
	})

	t.Run("default threshold 20", func(t *testing.T) {
		root := t.TempDir()
		files := make([]string, 21)
		for i := range files {
			files[i] = fmt.Sprintf("src/f%02d.ts", i)
		}
		runner := &stubRunner{runFn: func(contract.RunSpec) contract.RunResult {
			return contract.RunResult{Stdout: eslintDryRunOutput(t, files, "no-console"), ExitCode: 1}
		}}
		result, err := DryRunRule(context.Background(), runner, DryRunOptions{
			Engine:      "eslint",
			RuleID:      "no-console",
			RulePayload: `"error"`,
			ProjectRoot: root,
		})
		if err != nil {
			t.Fatalf("DryRunRule error: %v", err)
		}
		if result.Hits != 21 {
			t.Fatalf("hits = %d, want 21", result.Hits)
		}
		if !strings.Contains(result.Warning, "exceeds threshold 20") {
			t.Fatalf("warning = %q, want default threshold warning", result.Warning)
		}
	})
}

func TestDryRunRuleSampleBadDetection(t *testing.T) {
	t.Run("sample matched", func(t *testing.T) {
		root := t.TempDir()
		var samplePath string
		runner := &stubRunner{runFn: func(spec contract.RunSpec) contract.RunResult {
			samplePath = dryRunSampleTarget(spec.Args)
			if samplePath == "" {
				t.Fatal("runner did not receive a sample_bad target")
			}
			files := []string{samplePath, "src/app.ts"}
			return contract.RunResult{Stdout: eslintDryRunOutput(t, files, "no-console"), ExitCode: 1}
		}}
		result, err := DryRunRule(context.Background(), runner, DryRunOptions{
			Engine:      "eslint",
			RuleID:      "no-console",
			RulePayload: `"error"`,
			ProjectRoot: root,
			SampleBad:   "console.log('bad')",
			Files:       []string{"src/app.ts"},
		})
		if err != nil {
			t.Fatalf("DryRunRule error: %v", err)
		}
		if !result.SampleMatched {
			t.Fatalf("sample_matched = false, want true (result=%+v)", result)
		}
		if result.Hits != 1 {
			t.Fatalf("hits = %d, want 1 (sample hits must not count)", result.Hits)
		}
		if len(result.SampleFiles) != 1 || result.SampleFiles[0] != "src/app.ts" {
			t.Fatalf("sample_files = %v, want [src/app.ts]", result.SampleFiles)
		}
		if result.Warning != "" {
			t.Fatalf("warning = %q, want empty", result.Warning)
		}
		if _, err := os.Stat(samplePath); !os.IsNotExist(err) {
			t.Fatalf("sample file %s was not cleaned up: %v", samplePath, err)
		}
	})

	t.Run("sample not matched", func(t *testing.T) {
		root := t.TempDir()
		runner := &stubRunner{runFn: func(contract.RunSpec) contract.RunResult {
			return contract.RunResult{Stdout: eslintDryRunOutput(t, []string{"src/app.ts"}, "no-console"), ExitCode: 1}
		}}
		result, err := DryRunRule(context.Background(), runner, DryRunOptions{
			Engine:      "eslint",
			RuleID:      "no-console",
			RulePayload: `"error"`,
			ProjectRoot: root,
			SampleBad:   "console.log('bad')",
			Files:       []string{"src/app.ts"},
		})
		if err != nil {
			t.Fatalf("DryRunRule error: %v", err)
		}
		if result.SampleMatched {
			t.Fatalf("sample_matched = true, want false")
		}
		if !strings.Contains(result.Warning, "sample_bad was not matched") {
			t.Fatalf("warning = %q, want sample_bad warning", result.Warning)
		}
	})
}

// TestDryRunRuleIgnoresESLintIgnoredFileWarnings reproduces real ESLint 9
// output: files that are ignored (outside base path / no matching config) come
// back as severity-1 messages with ruleId null. They must not count as hits.
func TestDryRunRuleIgnoresESLintIgnoredFileWarnings(t *testing.T) {
	root := t.TempDir()
	runner := &stubRunner{runFn: func(spec contract.RunSpec) contract.RunResult {
		samplePath := dryRunSampleTarget(spec.Args)
		if samplePath == "" {
			t.Fatal("runner did not receive a sample_bad target")
		}
		payload := []map[string]any{
			{
				"filePath": "src/app.ts",
				"messages": []map[string]any{{
					"ruleId":   nil,
					"severity": 1,
					"message":  "File ignored because no matching configuration was supplied.",
					"fatal":    false,
				}},
			},
			{
				"filePath": samplePath,
				"messages": []map[string]any{{
					"ruleId":   nil,
					"severity": 1,
					"message":  "File ignored because outside of base path.",
					"fatal":    false,
				}},
			},
		}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal eslint ignored output: %v", err)
		}
		return contract.RunResult{Stdout: string(data), ExitCode: 0}
	}}

	result, err := DryRunRule(context.Background(), runner, DryRunOptions{
		Engine:      "eslint",
		RuleID:      "no-console",
		RulePayload: `"error"`,
		ProjectRoot: root,
		SampleBad:   "console.log('bad');",
		Files:       []string{"src/app.ts"},
	})
	if err != nil {
		t.Fatalf("DryRunRule error: %v", err)
	}
	if result.Hits != 0 {
		t.Fatalf("hits = %d, want 0: ignored-file warnings must not count", result.Hits)
	}
	if result.SampleMatched {
		t.Fatal("sample_matched = true, want false: ignored-file warning must not count")
	}
	if !strings.Contains(result.Warning, "matched 0 occurrences") {
		t.Fatalf("warning = %q, want 0-hit warning", result.Warning)
	}
	if !strings.Contains(result.Warning, "sample_bad was not matched") {
		t.Fatalf("warning = %q, want sample_bad warning", result.Warning)
	}
}

func TestDryRunRuleDoesNotPolluteProjectConfig(t *testing.T) {
	t.Run("legacy eslint config", func(t *testing.T) {
		root := t.TempDir()
		configPath := filepath.Join(root, ".eslintrc.json")
		original := []byte("{\n  \"rules\": {\n    \"semi\": [\"error\", \"always\"]\n  }\n}\n")
		if err := os.WriteFile(configPath, original, 0o644); err != nil {
			t.Fatal(err)
		}

		var generatedPath, generatedContent string
		runner := &stubRunner{runFn: func(spec contract.RunSpec) contract.RunResult {
			generatedPath = dryRunArg(spec.Args, "--config")
			data, err := os.ReadFile(generatedPath)
			if err == nil {
				generatedContent = string(data)
			}
			return contract.RunResult{Stdout: "[]", Duration: 5 * time.Millisecond}
		}}
		if _, err := DryRunRule(context.Background(), runner, DryRunOptions{
			Engine:      "eslint",
			RuleID:      "no-console",
			RulePayload: `"error"`,
			ProjectRoot: root,
		}); err != nil {
			t.Fatalf("DryRunRule error: %v", err)
		}

		if filepath.Base(generatedPath) != ".eslintrc.json" {
			t.Fatalf("generated config = %s, want legacy .eslintrc.json", generatedPath)
		}
		if filepath.Dir(generatedPath) == root {
			t.Fatalf("generated config must live outside the project, got %s", generatedPath)
		}
		if !strings.Contains(generatedContent, `"no-console"`) {
			t.Fatalf("generated config missing injected rule:\n%s", generatedContent)
		}
		if !strings.Contains(strings.Join(runner.Calls()[0].Args, " "), "--no-eslintrc") {
			t.Fatalf("eslint args = %v, want --no-eslintrc", runner.Calls()[0].Args)
		}
		if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("project config changed: err=%v\n%s", err, got)
		}
		if _, err := os.Stat(generatedPath); !os.IsNotExist(err) {
			t.Fatalf("generated config not removed after dry-run: %v", err)
		}
	})

	t.Run("flat eslint config", func(t *testing.T) {
		root := t.TempDir()
		configPath := filepath.Join(root, "eslint.config.mjs")
		original := []byte("export default [];\n")
		if err := os.WriteFile(configPath, original, 0o644); err != nil {
			t.Fatal(err)
		}
		var generatedPath, generatedContent string
		runner := &stubRunner{runFn: func(spec contract.RunSpec) contract.RunResult {
			generatedPath = dryRunArg(spec.Args, "--config")
			data, err := os.ReadFile(generatedPath)
			if err == nil {
				generatedContent = string(data)
			}
			return contract.RunResult{Stdout: "[]"}
		}}
		if _, err := DryRunRule(context.Background(), runner, DryRunOptions{
			Engine:      "eslint",
			RuleID:      "no-console",
			RulePayload: `"error"`,
			ProjectRoot: root,
		}); err != nil {
			t.Fatalf("DryRunRule error: %v", err)
		}
		if filepath.Base(generatedPath) != "eslint.config.mjs" {
			t.Fatalf("generated config = %s, want eslint.config.mjs", generatedPath)
		}
		if !strings.Contains(generatedContent, "file://") || !strings.Contains(generatedContent, "no-console") {
			t.Fatalf("flat temp config missing base import or rule:\n%s", generatedContent)
		}
		if !strings.Contains(strings.Join(runner.Calls()[0].Args, " "), "--no-config-lookup") {
			t.Fatalf("eslint args = %v, want --no-config-lookup", runner.Calls()[0].Args)
		}
		if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("project flat config changed: err=%v\n%s", err, got)
		}
	})
}

func TestDryRunRuleParsesRuffAndSemgrep(t *testing.T) {
	t.Run("ruff", func(t *testing.T) {
		root := t.TempDir()
		runner := &stubRunner{runFn: func(spec contract.RunSpec) contract.RunResult {
			if spec.Name != "ruff" {
				t.Fatalf("runner tool = %q, want ruff", spec.Name)
			}
			if !strings.Contains(strings.Join(spec.Args, " "), "--output-format json") {
				t.Fatalf("ruff args = %v, want JSON output", spec.Args)
			}
			return contract.RunResult{
				Stdout:   `[{"code":"E501","message":"line too long","filename":"app/main.py","location":{"row":3}}]`,
				ExitCode: 1,
			}
		}}
		result, err := DryRunRule(context.Background(), runner, DryRunOptions{
			Engine:      "ruff",
			RuleID:      "E501",
			RulePayload: "# extra ruff config\n",
			ProjectRoot: root,
			Files:       []string{"app/main.py"},
		})
		if err != nil {
			t.Fatalf("DryRunRule error: %v", err)
		}
		if result.Hits != 1 || len(result.SampleFiles) != 1 || result.SampleFiles[0] != "app/main.py" {
			t.Fatalf("result = %+v, want one app/main.py hit", result)
		}
	})

	t.Run("semgrep", func(t *testing.T) {
		root := t.TempDir()
		runner := &stubRunner{runFn: func(spec contract.RunSpec) contract.RunResult {
			if spec.Name != "semgrep" {
				t.Fatalf("runner tool = %q, want semgrep", spec.Name)
			}
			return contract.RunResult{
				Stdout:   `{"results":[{"check_id":"no-print","path":"app/main.py","start":{"line":2}}]}`,
				ExitCode: 1,
			}
		}}
		payload := "rules:\n  - id: no-print\n    languages: [python]\n    message: no print\n    severity: WARNING\n    pattern: print(...)\n"
		result, err := DryRunRule(context.Background(), runner, DryRunOptions{
			Engine:      "semgrep",
			RuleID:      "no-print",
			RulePayload: payload,
			ProjectRoot: root,
			Files:       []string{"app/main.py"},
		})
		if err != nil {
			t.Fatalf("DryRunRule error: %v", err)
		}
		if result.Hits != 1 || result.SampleFiles[0] != "app/main.py" {
			t.Fatalf("result = %+v, want one app/main.py hit", result)
		}
	})
}

func TestDryRunRuleValidation(t *testing.T) {
	root := t.TempDir()
	valid := DryRunOptions{Engine: "eslint", RuleID: "no-console", RulePayload: `"error"`, ProjectRoot: root}
	tests := []struct {
		name   string
		runner contract.Runner
		opts   DryRunOptions
		want   string
	}{
		{name: "nil runner", runner: nil, opts: valid, want: "runner is nil"},
		{name: "unsupported engine", runner: &stubRunner{}, opts: DryRunOptions{Engine: "golangci", RuleID: "x", RulePayload: "y", ProjectRoot: root}, want: "unsupported dry-run engine"},
		{name: "empty payload", runner: &stubRunner{}, opts: DryRunOptions{Engine: "eslint", RuleID: "x", ProjectRoot: root}, want: "payload is empty"},
		{name: "empty rule id", runner: &stubRunner{}, opts: DryRunOptions{Engine: "eslint", RulePayload: "y", ProjectRoot: root}, want: "rule id is empty"},
		{name: "missing tool", runner: &stubRunner{look: func(string) (string, bool) { return "", false }}, opts: valid, want: "not found"},
		{name: "missing project", runner: &stubRunner{}, opts: DryRunOptions{Engine: "eslint", RuleID: "x", RulePayload: "y", ProjectRoot: filepath.Join(root, "missing")}, want: "project root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DryRunRule(context.Background(), tt.runner, tt.opts)
			if err == nil {
				t.Fatalf("DryRunRule error = nil, want containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want containing %q", err, tt.want)
			}
		})
	}
}
