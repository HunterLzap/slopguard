package radar

import (
	"os"
	"path/filepath"
	"testing"
)

func writeBaselineFixture(t *testing.T, root, content string) {
	t.Helper()
	writeRadarFile(t, root, ".slopguard/baseline.json", content)
}

func TestLoadBaselineStatusComputesCurrentValues(t *testing.T) {
	root := makeTSRadarProject(t)
	writeRadarFile(t, root, "src/generated.ts", "// @ts-ignore\n// @ts-ignore\nexport const generated = 1;\n")
	writeBaselineFixture(t, root, `{
  "version": 1,
  "updated_at": "2026-11-18T00:00:00Z",
  "metrics": {"ts_ignore": 0, "eslint_disable": 2},
  "suppressed_files": ["src/generated.ts"]
}`)

	status, err := LoadBaselineStatus(root)
	if err != nil {
		t.Fatalf("LoadBaselineStatus error: %v", err)
	}
	if !status.Available || status.Passed {
		t.Fatalf("status = %+v, want available and failed", status)
	}
	if status.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should be populated")
	}
	var tsIgnore, eslintDisable *BaselineMetric
	for i := range status.Metrics {
		switch status.Metrics[i].Name {
		case "ts_ignore":
			tsIgnore = &status.Metrics[i]
		case "eslint_disable":
			eslintDisable = &status.Metrics[i]
		}
	}
	if tsIgnore == nil || tsIgnore.Baseline != 0 || tsIgnore.Current != 1 || tsIgnore.Delta != 1 {
		t.Fatalf("ts_ignore metric = %+v", tsIgnore)
	}
	if eslintDisable == nil || eslintDisable.Baseline != 2 || eslintDisable.Current != 0 || eslintDisable.Delta != -2 {
		t.Fatalf("eslint_disable metric = %+v", eslintDisable)
	}
	if len(status.SuppressedFiles) != 1 || status.SuppressedFiles[0] != "src/generated.ts" {
		t.Fatalf("suppressed files = %v", status.SuppressedFiles)
	}
}

func TestLoadBaselineStatusMissingRootAndBaseline(t *testing.T) {
	status, err := LoadBaselineStatus(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("missing root error: %v", err)
	}
	if status.Available || len(status.Metrics) != 0 {
		t.Fatalf("missing root status = %+v", status)
	}

	status, err = LoadBaselineStatus(t.TempDir())
	if err != nil {
		t.Fatalf("missing baseline error: %v", err)
	}
	if status.Available || len(status.Metrics) != 0 {
		t.Fatalf("missing baseline status = %+v", status)
	}
}

func TestLoadBaselineStatusRejectsBrokenBaseline(t *testing.T) {
	root := t.TempDir()
	writeBaselineFixture(t, root, "{broken")
	if _, err := LoadBaselineStatus(root); err == nil {
		t.Fatal("broken baseline should return an error")
	}
}

func TestLoadBaselineStatusMissingRootFile(t *testing.T) {
	root := t.TempDir()
	writeBaselineFixture(t, root, `{"version":1,"metrics":{}}`)
	status, err := LoadBaselineStatus(root)
	if err != nil {
		t.Fatalf("LoadBaselineStatus error: %v", err)
	}
	if !status.Available || !status.Passed {
		t.Fatalf("status = %+v, want available/pass", status)
	}
	if len(status.Metrics) != 0 {
		t.Fatalf("metrics = %v, want empty", status.Metrics)
	}
	if _, err := os.Stat(filepath.Join(root, ".slopguard", "baseline.json")); err != nil {
		t.Fatal(err)
	}
}
