package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePresetFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlanPresetsIsNonDestructiveAndApplyIsIdempotent(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	writePresetFile(t, filepath.Join(global, "eslint", "rules.json"), `{"no-console":"error"}`)
	configPath := filepath.Join(project, "eslint.config.mjs")
	original := "export default [];\n"
	writePresetFile(t, configPath, original)

	plan, err := PlanPresets(global, project)
	if err != nil {
		t.Fatalf("PlanPresets: %v", err)
	}
	if len(plan.Written) != 1 || plan.Written[0].Engine != "eslint" || plan.Written[0].RuleID != "no-console" {
		t.Fatalf("plan.Written = %+v", plan.Written)
	}
	if len(plan.Diffs) == 0 {
		t.Fatal("plan.Diffs is empty")
	}
	afterPlan, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterPlan) != original {
		t.Fatalf("PlanPresets modified project config:\n%s", afterPlan)
	}

	result, err := ApplyPresets(plan)
	if err != nil {
		t.Fatalf("ApplyPresets: %v", err)
	}
	if len(result.Written) != 1 || len(result.Backups) != 1 {
		t.Fatalf("result = %+v", result)
	}
	afterApply, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(afterApply), "no-console") || !strings.Contains(string(afterApply), "files:") {
		t.Fatalf("applied config:\n%s", afterApply)
	}

	second, err := PlanPresets(global, project)
	if err != nil {
		t.Fatalf("second PlanPresets: %v", err)
	}
	if len(second.Written) != 0 || len(second.Skipped) != 1 {
		t.Fatalf("second plan = %+v, want one skipped", second)
	}
	afterSecond, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterApply, afterSecond) {
		t.Fatalf("idempotent re-plan changed config")
	}
}

func TestPlanPresetsRuffAndSemgrep(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	writePresetFile(t, filepath.Join(global, "ruff", "rules.json"), `{"E501":"line-length = 100\n"}`)
	writePresetFile(t, filepath.Join(global, "semgrep", "rules.json"),
		`{"no-print":"id: no-print\nlanguages: [python]\nmessage: no print\nseverity: WARNING\npattern: print(...)\n"}`)

	plan, err := PlanPresets(global, project)
	if err != nil {
		t.Fatalf("PlanPresets: %v", err)
	}
	if len(plan.Written) != 2 {
		t.Fatalf("plan.Written = %+v, want ruff+semgrep", plan.Written)
	}
	if _, err := ApplyPresets(plan); err != nil {
		t.Fatalf("ApplyPresets: %v", err)
	}
	ruffData, err := os.ReadFile(filepath.Join(project, "ruff.toml"))
	if err != nil {
		t.Fatalf("ruff.toml: %v", err)
	}
	if !strings.Contains(string(ruffData), "E501") {
		t.Fatalf("ruff.toml:\n%s", ruffData)
	}
	semgrepData, err := os.ReadFile(filepath.Join(project, ".semgrep.yml"))
	if err != nil {
		t.Fatalf(".semgrep.yml: %v", err)
	}
	if !strings.Contains(string(semgrepData), "no-print") {
		t.Fatalf(".semgrep.yml:\n%s", semgrepData)
	}
}

func TestPlanPresetsMissingGlobalDirectory(t *testing.T) {
	project := t.TempDir()
	plan, err := PlanPresets(filepath.Join(t.TempDir(), "missing"), project)
	if err != nil {
		t.Fatalf("PlanPresets missing global dir: %v", err)
	}
	if len(plan.Written) != 0 || len(plan.Skipped) != 0 {
		t.Fatalf("plan = %+v, want empty", plan)
	}
}
