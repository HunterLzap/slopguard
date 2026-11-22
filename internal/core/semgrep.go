package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core/adapters"
)

var SemgrepTool = contract.ToolRequirement{Name: "semgrep", InstallCommand: "pip install semgrep"}

type Semgrep struct {
	Runner ProcessRunner
}

type ScanReport struct {
	Diagnostics  []contract.Diagnostic
	ScannedFiles []string
	Missing      bool
	Configured   bool
}

func RulesDir(project string) string {
	return filepath.Join(project, ProjectDirectory, "rules")
}

func (s Semgrep) RunCustomRules(ctx context.Context, project string, files []string) (ScanReport, error) {
	entries, err := os.ReadDir(RulesDir(project))
	if errors.Is(err, fs.ErrNotExist) {
		return ScanReport{}, nil
	}
	if err != nil {
		return ScanReport{}, err
	}
	var configs []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && filepath.Ext(entry.Name()) == ".yaml" {
			configs = append(configs, filepath.Join(RulesDir(project), entry.Name()))
		}
	}
	if len(configs) == 0 {
		return ScanReport{}, nil
	}
	bin, err := s.Runner.LookPath(SemgrepTool.Name)
	if err != nil {
		return ScanReport{Missing: true, Configured: true}, nil
	}
	report, err := s.scan(ctx, bin, project, configs, files)
	report.Configured = true
	return report, err
}

func (s Semgrep) ValidateRuleFile(ctx context.Context, ruleFilePath string) error {
	bin, err := s.Runner.LookPath(SemgrepTool.Name)
	if err != nil {
		return fmt.Errorf("semgrep is required (%s): %w", SemgrepTool.InstallCommand, err)
	}
	return s.validate(ctx, bin, ruleFilePath)
}

func (s Semgrep) validate(ctx context.Context, bin, ruleFilePath string) error {
	r, err := s.Runner.Run(ctx, filepath.Dir(ruleFilePath), bin, "--validate", "--config", ruleFilePath, "--metrics=off", "--disable-version-check")
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("Semgrep rule validation: %s", processMessage(r))
	}
	return nil
}

func (s Semgrep) scan(ctx context.Context, bin, root string, configs, files []string) (ScanReport, error) {
	args := []string{"--json", "--metrics=off", "--disable-version-check", "--no-rewrite-rule-ids", "--strict"}
	for _, file := range configs {
		args = append(args, "--config", file)
	}
	var report ScanReport
	seen := make(map[string]bool)
	for _, batch := range fileBatches(root, files) {
		r, err := s.Runner.Run(ctx, root, bin, append(append([]string{}, args...), batch...)...)
		if err != nil {
			return report, err
		}
		var output struct {
			Paths   struct{ Scanned []string }
			Errors  []json.RawMessage
			Results []struct {
				ID    string `json:"check_id"`
				Path  string
				Start struct{ Line, Col int }
				Extra struct{ Message string }
			}
		}
		if err := json.Unmarshal([]byte(r.Stdout), &output); err != nil {
			return report, fmt.Errorf("Semgrep JSON: %w (%s)", err, processMessage(r))
		}
		for _, path := range output.Paths.Scanned {
			if rel, ok := relativeFile(root, filepath.FromSlash(path)); ok && !seen[rel] {
				seen[rel] = true
				report.ScannedFiles = append(report.ScannedFiles, rel)
			}
		}
		for _, finding := range output.Results {
			rel, ok := relativeFile(root, filepath.FromSlash(finding.Path))
			if !ok || !seen[rel] {
				return report, fmt.Errorf("Semgrep finding outside scanned files: %q", finding.Path)
			}
			language, _ := adapters.Match(rel)
			report.Diagnostics = append(report.Diagnostics, contract.Diagnostic{
				Language: language.ID, Checker: SemgrepTool.Name, RuleID: finding.ID,
				Severity: "error", File: rel, Line: finding.Start.Line, Column: finding.Start.Col,
				Message: finding.Extra.Message, CodeSnippet: snippet(root, rel, finding.Start.Line),
			})
		}
		if len(output.Errors) > 0 {
			return report, fmt.Errorf("Semgrep scan: %s", output.Errors[0])
		}
		if r.ExitCode != 0 {
			return report, fmt.Errorf("Semgrep scan: %s", processMessage(r))
		}
	}
	return report, nil
}

func fileBatches(root string, files []string) [][]string {
	argumentBudget := 14000
	if runtime.GOOS == "windows" {
		// npm and other Windows shims can use cmd.exe's shorter command line.
		argumentBudget = 6500
	}
	var batches [][]string
	var batch []string
	size := 0
	for _, file := range files {
		path := filepath.Join(root, file)
		if size+len(path)+3 > argumentBudget && len(batch) > 0 {
			batches = append(batches, batch)
			batch = nil
			size = 0
		}
		batch = append(batch, path)
		size += len(path) + 3
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}

func scanFiles(root string, files []string) []string {
	var result []string
	for _, file := range files {
		first, _, _ := strings.Cut(filepath.ToSlash(file), "/")
		if !ignoredDirectory(first) && adapters.Regular(filepath.Join(root, file)) {
			result = append(result, file)
		}
	}
	return result
}
