package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
)

// defaultDependencies wires the CLI to the production engine and adapter
// implementations. It is the only file in internal/cli that imports concrete
// core packages.
func defaultDependencies() (dependencies, error) {
	runner := engine.NewExecRunner()
	return dependencies{
		engine:   productionEngine{runner: runner},
		adapters: productionAdapters{runner: runner},
	}, nil
}

type productionEngine struct {
	runner contract.Runner
}

func (p productionEngine) ChangedFiles(ctx context.Context, root string) ([]string, error) {
	git := engine.NewGit(p.runner, root)
	if !git.IsRepository(ctx) {
		return nil, fmt.Errorf("not a git repository; use --all to scan every file")
	}
	return git.ChangedFiles(ctx)
}

func (p productionEngine) ListFiles(root string, extensions []string) ([]string, error) {
	return engine.ListFiles(root, extensions)
}

func (p productionEngine) RunChecks(ctx context.Context, root string, jobs int, tasks []checkTask) ([]checkOutcome, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	checks := make([]contract.Check, len(tasks))
	for i, task := range tasks {
		checks[i] = task.Check
	}
	results := engine.RunChecks(ctx, p.runner, root, checks, jobs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	outcomes := make([]checkOutcome, len(tasks))
	for i, task := range tasks {
		outcomes[i] = checkOutcome{Task: task, Result: results[i].Result}
	}
	return outcomes, nil
}

func (p productionEngine) LoadBaseline(path string) (map[string]int, error) {
	baseline, err := engine.LoadBaseline(path)
	if err != nil {
		return nil, err
	}
	return baseline.Metrics, nil
}

func (p productionEngine) LoadBaselineState(path string) (baselineState, error) {
	baseline, err := engine.LoadBaseline(path)
	if err != nil {
		return baselineState{}, err
	}
	return baselineState{
		Metrics:         baseline.Metrics,
		IsSuppressed:    baseline.IsSuppressed,
		HasSuppressions: len(baseline.SuppressedFiles) > 0,
	}, nil
}

func (p productionEngine) SaveBaseline(path string, metrics map[string]int) error {
	return engine.SaveBaseline(path, engine.NewBaselineFromMetrics(metrics, time.Now()))
}

func (p productionEngine) ComputeEscapes(root string, files []string, patterns []contract.EscapePattern) (map[string]int, error) {
	return engine.CountEscapes(root, files, patterns)
}

func (p productionEngine) Ratchet(escapes, baseline map[string]int) []contract.Finding {
	base := engine.NewBaselineFromMetrics(baseline, time.Time{})
	comparison := engine.Compare(escapes, base)
	findings := make([]contract.Finding, 0, len(comparison.Regressions))
	for _, delta := range comparison.Regressions {
		findings = append(findings, contract.Finding{
			Rule: "ratchet:" + delta.Name,
			Message: fmt.Sprintf("escape hatch %s increased: %d > baseline %d (+%d)",
				delta.Name, delta.Current, delta.Baseline, delta.Delta),
			Severity: contract.SeverityError,
		})
	}
	return findings
}

type productionAdapters struct {
	runner contract.Runner
}

func (p productionAdapters) Detect(root string) ([]contract.Adapter, error) {
	registry, err := adapter.NewDefaultRegistry()
	if err != nil {
		return nil, err
	}
	if err := loadProjectAdapters(registry, root); err != nil {
		return nil, err
	}
	return adapter.NewService(registry, p.runner).Detect(root)
}

// loadProjectAdapters loads project-local YAML adapters. Errors are returned
// because a broken project adapter is a configuration error for this project.
func loadProjectAdapters(registry *adapter.Registry, root string) error {
	dir := filepath.Join(root, ".slopguard", "adapters")
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return registry.RegisterYAMLDir(dir)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (p productionAdapters) PlanChecks(root string, checks []contract.Check) (checkPlan, error) {
	registry, err := adapter.NewDefaultRegistry()
	if err != nil {
		return checkPlan{}, err
	}
	planned, err := adapter.NewService(registry, p.runner).PlanChecks(root, checks)
	out := checkPlan{Runnable: planned.Runnable}
	for _, skipped := range planned.Skipped {
		out.Skipped = append(out.Skipped, skipped.ID)
		out.SkippedDetails = append(out.SkippedDetails, contract.SkippedDetail{
			ID:     skipped.ID,
			Tool:   skipped.Tool,
			Reason: skipped.Reason,
		})
	}
	return out, err
}

func (p productionAdapters) ParseFindings(a contract.Adapter, checkID string, result contract.RunResult) []contract.Finding {
	return adapter.ParseFindings(a, checkID, result)
}
