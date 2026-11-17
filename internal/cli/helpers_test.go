package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/contract"
)

// fakeEngine implements engineClient without touching real processes.
type fakeEngine struct {
	changedFiles   []string
	changedErr     error
	listFiles      []string
	listErr        error
	runChecksFn    func(ctx context.Context, root string, jobs int, tasks []checkTask) ([]checkOutcome, error)
	lastJobs       int
	lastTasks      []checkTask
	baseline       map[string]int
	baselineErr    error
	saveErr        error
	savedMetrics   map[string]int
	savedPath      string
	escapes        map[string]int
	escapesErr     error
	ratchetResults []contract.Finding
}

func (f *fakeEngine) ChangedFiles(context.Context, string) ([]string, error) {
	return f.changedFiles, f.changedErr
}

func (f *fakeEngine) ListFiles(string, []string) ([]string, error) {
	return f.listFiles, f.listErr
}

func (f *fakeEngine) RunChecks(ctx context.Context, root string, jobs int, tasks []checkTask) ([]checkOutcome, error) {
	f.lastJobs = jobs
	f.lastTasks = append([]checkTask(nil), tasks...)
	if f.runChecksFn != nil {
		return f.runChecksFn(ctx, root, jobs, tasks)
	}
	outcomes := make([]checkOutcome, len(tasks))
	for i, task := range tasks {
		outcomes[i] = checkOutcome{Task: task, Result: contract.RunResult{ExitCode: 0}}
	}
	return outcomes, nil
}

func (f *fakeEngine) LoadBaseline(string) (map[string]int, error) {
	if f.baselineErr != nil {
		return nil, f.baselineErr
	}
	return f.baseline, nil
}

func (f *fakeEngine) SaveBaseline(path string, metrics map[string]int) error {
	f.savedPath = path
	f.savedMetrics = metrics
	return f.saveErr
}

func (f *fakeEngine) ComputeEscapes(string, []string, []contract.EscapePattern) (map[string]int, error) {
	return f.escapes, f.escapesErr
}

func (f *fakeEngine) Ratchet(map[string]int, map[string]int) []contract.Finding {
	return f.ratchetResults
}

// fakeAdapters implements adapterProvider.
type fakeAdapters struct {
	detected    []contract.Adapter
	detectErr   error
	planFn      func(root string, checks []contract.Check) (checkPlan, error)
	plan        checkPlan
	planErr     error
	detectCalls int
	planCalls   int
}

func (f *fakeAdapters) Detect(string) ([]contract.Adapter, error) {
	f.detectCalls++
	return f.detected, f.detectErr
}

func (f *fakeAdapters) PlanChecks(root string, checks []contract.Check) (checkPlan, error) {
	f.planCalls++
	if f.planFn != nil {
		return f.planFn(root, checks)
	}
	if f.planErr != nil {
		return checkPlan{}, f.planErr
	}
	if len(f.plan.Runnable) > 0 || len(f.plan.Skipped) > 0 {
		return f.plan, nil
	}
	return checkPlan{Runnable: checks}, nil
}

func (f *fakeAdapters) ParseFindings(a contract.Adapter, checkID string, result contract.RunResult) []contract.Finding {
	return adapter.ParseFindings(a, checkID, result)
}

// stubAdapter is a minimal contract.Adapter for CLI tests.
type stubAdapter struct {
	name    string
	exts    []string
	checks  []contract.Check
	escapes []contract.EscapePattern
	parseFn func(checkID, output string) []contract.Finding
}

func (s *stubAdapter) Name() string                             { return s.name }
func (s *stubAdapter) Detect(string) bool                       { return true }
func (s *stubAdapter) Extensions() []string                     { return s.exts }
func (s *stubAdapter) Checks(string, []string) []contract.Check { return s.checks }
func (s *stubAdapter) EscapePatterns() []contract.EscapePattern { return s.escapes }
func (s *stubAdapter) ParseOutput(checkID, output string) []contract.Finding {
	if s.parseFn != nil {
		return s.parseFn(checkID, output)
	}
	return nil
}

func withDeps(t *testing.T, eng engineClient, adp adapterProvider) {
	t.Helper()
	old := newDependencies
	newDependencies = func() (dependencies, error) {
		return dependencies{engine: eng, adapters: adp}, nil
	}
	t.Cleanup(func() { newDependencies = old })
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
