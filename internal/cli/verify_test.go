package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/ui"
)

func verifyAdapter() *stubAdapter {
	return &stubAdapter{
		name:   "typescript",
		exts:   []string{".ts"},
		checks: []contract.Check{{ID: "fake-check", Cmd: []string{"fake"}}},
	}
}

func TestVerifyAllPass(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	eng := &fakeEngine{listFiles: []string{"src/a.ts"}}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}})

	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "slopguard verify: PASS") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
}

func TestVerifyFindingsExit1(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	adp := verifyAdapter()
	adp.parseFn = func(checkID, _ string) []contract.Finding {
		return []contract.Finding{{File: "src/a.ts", Line: 1, Rule: "eslint:no-explicit-any", Message: "unexpected any", Severity: contract.SeverityError}}
	}
	eng := &fakeEngine{
		listFiles: []string{"src/a.ts"},
		runChecksFn: func(_ context.Context, _ string, _ int, tasks []checkTask) ([]checkOutcome, error) {
			return []checkOutcome{{Task: tasks[0], Result: contract.RunResult{ExitCode: 1, Stdout: "raw output"}}}, nil
		},
	}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{adp}})

	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr)
	if code != ExitFindings {
		t.Fatalf("code = %d, want %d; stderr = %s", code, ExitFindings, stderr.String())
	}
	if !strings.Contains(stdout.String(), "eslint:no-explicit-any") || !strings.Contains(stdout.String(), "FAIL") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
}

func TestVerifyJSONResult(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	eng := &fakeEngine{listFiles: []string{"src/a.ts"}, escapes: map[string]int{"ts_ignore": 0}, baseline: map[string]int{"ts_ignore": 0}}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}})

	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true, json: true}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var result contract.VerifyResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("JSON decode: %v\n%s", err, stdout.String())
	}
	if !result.Passed || len(result.Findings) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyRatchetBlocks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "// @ts-ignore\n")
	adp := &stubAdapter{
		name:    "typescript",
		exts:    []string{".ts"},
		escapes: []contract.EscapePattern{{Name: "ts_ignore", Regex: `@ts-ignore`}},
	}
	eng := &fakeEngine{
		listFiles:      []string{"src/a.ts"},
		escapes:        map[string]int{"ts_ignore": 2},
		baseline:       map[string]int{"ts_ignore": 1},
		ratchetResults: []contract.Finding{{Rule: "ratchet:ts_ignore", Message: "escape hatch ts_ignore increased: 2 > baseline 1 (+1)", Severity: contract.SeverityError}},
	}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{adp}})

	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr)
	if code != ExitFindings {
		t.Fatalf("code = %d, want %d", code, ExitFindings)
	}
	if !strings.Contains(stdout.String(), "ratchet:ts_ignore") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
}

func TestVerifyMissingBaseline(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	adp := &stubAdapter{
		name:    "typescript",
		exts:    []string{".ts"},
		escapes: []contract.EscapePattern{{Name: "ts_ignore", Regex: `@ts-ignore`}},
	}
	eng := &fakeEngine{
		listFiles:   []string{"src/a.ts"},
		escapes:     map[string]int{"ts_ignore": 0},
		baselineErr: os.ErrNotExist,
	}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{adp}})

	// Default mode: missing baseline uses empty baseline and logs a non-blocking warning.
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true, json: true}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("default missing baseline code = %d, want %d; stderr = %s", code, ExitOK, stderr.String())
	}
	var res contract.VerifyResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "baseline.json not found") {
		t.Fatalf("warnings = %+v, want non-blocking baseline warning", res.Warnings)
	}
	if !strings.Contains(stderr.String(), "baseline.json not found") {
		t.Fatalf("stderr = %q, want warning", stderr.String())
	}

	// StrictBaseline: true fails with ExitError.
	stdout.Reset()
	stderr.Reset()
	code = runVerifyWith(verifyOptions{root: root, all: true, strictBaseline: true}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("strictBaseline code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "run slopguard init first") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestVerifyBaselineOtherError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	adp := &stubAdapter{
		name:    "typescript",
		exts:    []string{".ts"},
		escapes: []contract.EscapePattern{{Name: "ts_ignore", Regex: `@ts-ignore`}},
	}
	eng := &fakeEngine{listFiles: []string{"src/a.ts"}, escapes: map[string]int{}, baselineErr: errors.New("bad baseline")}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{adp}})
	var stdout, stderr bytes.Buffer
	if code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr); code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "load baseline") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestVerifyNoAdaptersPass(t *testing.T) {
	root := t.TempDir()
	withDeps(t, &fakeEngine{}, &fakeAdapters{})
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true, json: true}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var result contract.VerifyResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Passed {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyConfigJobsAndOverride(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".slopguard", "config.yaml"), "version: 1\nadapters: []\nverify:\n  jobs: 5\n")
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	eng := &fakeEngine{listFiles: []string{"src/a.ts"}}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}})

	var stdout, stderr bytes.Buffer
	if code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if eng.lastJobs != 5 {
		t.Fatalf("jobs = %d, want 5 from config", eng.lastJobs)
	}
	if code := runVerifyWith(verifyOptions{root: root, all: true, jobs: 3}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	if eng.lastJobs != 3 {
		t.Fatalf("jobs = %d, want 3 from CLI", eng.lastJobs)
	}
}

func TestVerifyDefaultJobs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	eng := &fakeEngine{listFiles: []string{"src/a.ts"}}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}})
	var stdout, stderr bytes.Buffer
	if code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	if eng.lastJobs != 2 {
		t.Fatalf("jobs = %d, want 2 default", eng.lastJobs)
	}
}

func TestVerifyConfigAdapterFilterError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".slopguard", "config.yaml"), "version: 1\nadapters:\n  - python\n")
	withDeps(t, &fakeEngine{}, &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}})
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "none of the configured adapters") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestVerifyIncrementalUsesChangedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	adp := &stubAdapter{
		name:    "typescript",
		exts:    []string{".ts"},
		checks:  []contract.Check{{ID: "fake-check", Cmd: []string{"fake"}}},
		escapes: []contract.EscapePattern{{Name: "ts_ignore", Regex: `@ts-ignore`}},
	}
	fa := &fakeAdapters{detected: []contract.Adapter{adp}}
	eng := &fakeEngine{
		changedFiles: []string{"src/a.ts"},
		listFiles:    []string{"src/a.ts", "src/b.ts"},
		escapes:      map[string]int{"ts_ignore": 0},
		baseline:     map[string]int{"ts_ignore": 0},
	}
	withDeps(t, eng, fa)
	var stdout, stderr bytes.Buffer
	if code := runVerifyWith(verifyOptions{root: root, all: false}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if len(eng.lastTasks) != 1 {
		t.Fatalf("tasks = %d, want 1 (jobs=%d, detectCalls=%d, planCalls=%d, exts=%v, checks=%d)", len(eng.lastTasks), eng.lastJobs, fa.detectCalls, fa.planCalls, adp.Extensions(), len(adp.Checks(root, []string{"src/a.ts"})))
	}
}

func TestVerifyNotGitRepository(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	eng := &fakeEngine{changedErr: errors.New("not a git repository; use --all to scan every file")}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}})
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "resolve scan scope") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestVerifyCheckSystemError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	eng := &fakeEngine{
		listFiles: []string{"src/a.ts"},
		runChecksFn: func(_ context.Context, _ string, _ int, tasks []checkTask) ([]checkOutcome, error) {
			return []checkOutcome{{Task: tasks[0], Result: contract.RunResult{ExitCode: -1, Err: errors.New("boom")}}}, nil
		},
	}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}})
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "check fake-check") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestVerifySkippedChecks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	eng := &fakeEngine{listFiles: []string{"src/a.ts"}}
	adp := &fakeAdapters{
		detected: []contract.Adapter{verifyAdapter()},
		plan:     checkPlan{Runnable: []contract.Check{{ID: "fake-check", Cmd: []string{"fake"}}}, Skipped: []string{"knip"}},
	}
	withDeps(t, eng, adp)
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true, json: true}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var result contract.VerifyResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "knip" {
		t.Fatalf("skipped = %v", result.Skipped)
	}
}

func TestVerifyPlanError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	eng := &fakeEngine{listFiles: []string{"src/a.ts"}}
	adp := &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}, planErr: errors.New("缺少必需工具")}
	withDeps(t, eng, adp)
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "check toolchain") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestVerifyListFilesError(t *testing.T) {
	root := t.TempDir()
	eng := &fakeEngine{listErr: errors.New("walk failed")}
	withDeps(t, eng, &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}})
	var stdout, stderr bytes.Buffer
	if code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr); code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestVerifyUnexpectedArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Execute([]string{"verify", "extra"}, nil, &stdout, &stderr); code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestVerifyRecordWarningDoesNotChangeExitCode(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	eng := &fakeEngine{listFiles: []string{"src/a.ts"}}
	recorder := &fakeRecorder{err: errors.New("read-only database")}
	withDependencies(t, dependencies{
		engine:   eng,
		adapters: &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}},
		recorder: recorder,
	})
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true, record: true}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, want 0; stderr = %s", code, stderr.String())
	}
	if recorder.calls != 1 {
		t.Fatalf("recorder calls = %d, want 1", recorder.calls)
	}
	if !strings.Contains(stderr.String(), "warning: record scan failed") {
		t.Fatalf("stderr = %q, want non-blocking warning", stderr.String())
	}
}

func TestVerifyRecordNotCalledWithoutFlag(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	recorder := &fakeRecorder{}
	withDependencies(t, dependencies{
		engine:   &fakeEngine{listFiles: []string{"src/a.ts"}},
		adapters: &fakeAdapters{detected: []contract.Adapter{verifyAdapter()}},
		recorder: recorder,
	})
	var stdout, stderr bytes.Buffer
	if code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	if recorder.calls != 0 {
		t.Fatalf("recorder calls = %d, want 0", recorder.calls)
	}
}

type fakeBaselineStateEngine struct {
	*fakeEngine
	state baselineState
	err   error
}

func (f *fakeBaselineStateEngine) LoadBaselineState(string) (baselineState, error) {
	return f.state, f.err
}

func TestVerifySuppressesBaselineFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	writeFile(t, filepath.Join(root, "src", "generated.ts"), "// @ts-ignore\n")
	adp := &stubAdapter{
		name:    "typescript",
		exts:    []string{".ts"},
		escapes: []contract.EscapePattern{{Name: "ts_ignore", Regex: `@ts-ignore`}},
	}
	engine := &fakeBaselineStateEngine{
		fakeEngine: &fakeEngine{listFiles: []string{"src/a.ts", "src/generated.ts"}, escapes: map[string]int{"ts_ignore": 0}},
		state: baselineState{
			Metrics:         map[string]int{"ts_ignore": 0},
			IsSuppressed:    func(file string) bool { return file == "src/generated.ts" },
			HasSuppressions: true,
		},
	}
	withDependencies(t, dependencies{engine: engine, adapters: &fakeAdapters{detected: []contract.Adapter{adp}}})
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true, json: true}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var result contract.VerifyResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1", result.Suppressed)
	}
	if result.Escapes["ts_ignore"] != 0 {
		t.Fatalf("escapes = %v", result.Escapes)
	}
}

func TestVerifySkippedDetails(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x = 1\n")
	eng := &fakeEngine{listFiles: []string{"src/a.ts"}}
	adp := &fakeAdapters{
		detected: []contract.Adapter{verifyAdapter()},
		plan: checkPlan{
			Runnable: []contract.Check{{ID: "fake-check", Cmd: []string{"fake"}}},
			Skipped:  []string{"knip"},
			SkippedDetails: []contract.SkippedDetail{
				{ID: "knip", Tool: "knip", Reason: "npm install -D knip"},
			},
		},
	}
	withDeps(t, eng, adp)
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true, json: true}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var result contract.VerifyResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.SkippedDetails) != 1 || result.SkippedDetails[0].Tool != "knip" || !strings.Contains(result.SkippedDetails[0].Reason, "npm install") {
		t.Fatalf("skipped details = %+v", result.SkippedDetails)
	}
}

type fakeMistakeRecorder struct {
	fakeRecorder
	mistakes []contract.Mistake
}

func (f *fakeMistakeRecorder) RecordMistake(_ context.Context, m contract.Mistake) (int, error) {
	f.mistakes = append(f.mistakes, m)
	return len(f.mistakes), nil
}

func TestVerifyAutoCapturesUniqueMistakes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "a.ts"), "const x: any = 1\n")
	eng := &fakeEngine{
		listFiles: []string{"src/a.ts"},
		runChecksFn: func(_ context.Context, _ string, _ int, tasks []checkTask) ([]checkOutcome, error) {
			return []checkOutcome{
				{Task: tasks[0], Result: contract.RunResult{ExitCode: 1, Stdout: "Unexpected any"}},
				{Task: tasks[0], Result: contract.RunResult{ExitCode: 1, Stdout: "Unexpected any"}},
				{Task: checkTask{AdapterName: "fake-js", Check: contract.Check{ID: "tsc", Cmd: []string{"tsc"}}}, Result: contract.RunResult{ExitCode: 1, Stdout: "Type error"}},
			}, nil
		},
	}
	adp := &fakeAdapters{
		detected: []contract.Adapter{verifyAdapter()},
		plan:     checkPlan{Runnable: []contract.Check{{ID: "eslint", Cmd: []string{"eslint"}}}},
	}
	rec := &fakeMistakeRecorder{}
	withDependencies(t, dependencies{
		engine:   eng,
		adapters: adp,
		recorder: rec,
	})
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true, record: true}, &stdout, &stderr)
	if code != ExitFindings {
		t.Fatalf("code = %d, want %d; stderr = %s", code, ExitFindings, stderr.String())
	}
	if len(rec.mistakes) != 2 {
		t.Fatalf("captured mistakes = %d, want 2 (deduplicated): %+v", len(rec.mistakes), rec.mistakes)
	}
	if rec.mistakes[0].Source != "auto_verify" || rec.mistakes[0].Status != "pending" {
		t.Fatalf("mistake[0] = %+v", rec.mistakes[0])
	}
}

func TestVerifyLastCommitAndCustomSource(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "committed.ts"), "const x: any = 1\n")
	eng := &fakeEngine{
		lastCommitFiles: []string{"src/committed.ts"},
		listFiles:       []string{"src/committed.ts"},
		runChecksFn: func(_ context.Context, _ string, _ int, tasks []checkTask) ([]checkOutcome, error) {
			return []checkOutcome{
				{Task: tasks[0], Result: contract.RunResult{ExitCode: 1, Stdout: "Unexpected any"}},
			}, nil
		},
	}
	adp := &fakeAdapters{
		detected: []contract.Adapter{verifyAdapter()},
		plan:     checkPlan{Runnable: []contract.Check{{ID: "eslint", Cmd: []string{"eslint"}}}},
	}
	rec := &fakeMistakeRecorder{}
	withDependencies(t, dependencies{
		engine:   eng,
		adapters: adp,
		recorder: rec,
	})
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{
		root:          root,
		lastCommit:    true,
		record:        true,
		captureSource: "git_hook",
	}, &stdout, &stderr)
	if code != ExitFindings {
		t.Fatalf("code = %d, want %d; stderr = %s", code, ExitFindings, stderr.String())
	}
	if len(rec.mistakes) != 1 || rec.mistakes[0].Source != "git_hook" {
		t.Fatalf("mistakes = %+v, want source=git_hook", rec.mistakes)
	}
}

func TestCLIVerifyAndUIVerifyProduceConsistentResults(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"name":"demo"}`)
	writeFile(t, filepath.Join(root, "src", "a.ts"), "// @ts-ignore\nexport const x: any = 1;\n")
	writeFile(t, filepath.Join(root, "node_modules", ".bin", "eslint"), "#!/bin/sh\necho '[]'\nexit 0\n")
	writeFile(t, filepath.Join(root, "node_modules", ".bin", "tsc"), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(root, "node_modules", ".bin", "eslint"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "node_modules", ".bin", "tsc"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Run via CLI path (defaultDependencies -> verify.Run, missing baseline.json).
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true, json: true}, &stdout, &stderr)
	if code != ExitFindings {
		t.Fatalf("cli verify exit = %d, want 1; stderr = %s", code, stderr.String())
	}
	var cliResult contract.VerifyResult
	if err := json.Unmarshal(stdout.Bytes(), &cliResult); err != nil {
		t.Fatalf("decode cli result: %v", err)
	}

	// Run via UI HTTP handler path (POST /api/verify -> verify.Run).
	srv := ui.NewServer(nil, nil)
	srv.SetAuthToken("consistency-token")
	reqBody, _ := json.Marshal(map[string]any{"project": root, "all": true})
	req := httptest.NewRequest(http.MethodPost, "/api/verify", bytes.NewReader(reqBody))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SlopGuard-Token", "consistency-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ui /api/verify status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var uiResult contract.VerifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &uiResult); err != nil {
		t.Fatalf("decode ui result: %v", err)
	}

	if cliResult.Passed != uiResult.Passed {
		t.Fatalf("Passed mismatch: cli=%v ui=%v", cliResult.Passed, uiResult.Passed)
	}
	if !reflect.DeepEqual(cliResult.Findings, uiResult.Findings) {
		t.Fatalf("Findings mismatch:\ncli=%+v\nui=%+v", cliResult.Findings, uiResult.Findings)
	}
	if !reflect.DeepEqual(cliResult.Escapes, uiResult.Escapes) {
		t.Fatalf("Escapes mismatch:\ncli=%+v\nui=%+v", cliResult.Escapes, uiResult.Escapes)
	}
	if !reflect.DeepEqual(cliResult.Warnings, uiResult.Warnings) {
		t.Fatalf("Warnings mismatch:\ncli=%+v\nui=%+v", cliResult.Warnings, uiResult.Warnings)
	}
}

func TestCLIVerifyDegradesWithoutExternalLinters(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pyproject.toml"), "[project]\nname='demo'\n")
	writeFile(t, filepath.Join(root, "app.py"), "x = 1\n")
	writeFile(t, filepath.Join(root, ".slopguard", "baseline.json"), `{"version":1,"metrics":{}}`)

	// Isolate PATH to an empty directory so no external linters are found on host.
	emptyBin := t.TempDir()
	t.Setenv("PATH", emptyBin)

	var stdout, stderr bytes.Buffer
	code := runVerify([]string{"--all", "--json", "--root", root}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("zero-linter verify exit = %d, want 0; stderr = %s", code, stderr.String())
	}
	var res contract.VerifyResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.SkippedDetails) < 2 {
		t.Fatalf("expected skipped details for ruff/pyright, got %+v", res.SkippedDetails)
	}

	// Only ruff present (pyright missing) -> still succeeds, records pyright skipped.
	writeFile(t, filepath.Join(root, ".venv", "bin", "ruff"), "#!/bin/sh\necho '[]'\nexit 0\n")
	if err := os.Chmod(filepath.Join(root, ".venv", "bin", "ruff"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = runVerify([]string{"--all", "--json", "--root", root}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("only-ruff verify exit = %d, want 0; stderr = %s", code, stderr.String())
	}
	res = contract.VerifyResult{}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	foundPyrightSkipped := false
	for _, sk := range res.SkippedDetails {
		if sk.ID == "pyright" && strings.Contains(sk.Reason, "pyright (not found)") {
			foundPyrightSkipped = true
		}
	}
	if !foundPyrightSkipped {
		t.Fatalf("expected pyright (not found) in skipped_details, got %+v", res.SkippedDetails)
	}

	// --strict-tools fails with ExitError (2) when pyright is missing.
	stdout.Reset()
	stderr.Reset()
	code = runVerify([]string{"--all", "--strict-tools", "--root", root}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("--strict-tools verify exit = %d, want 2; stderr = %s", code, stderr.String())
	}
}
