package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
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

	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{root: root, all: true}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want %d", code, ExitError)
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
