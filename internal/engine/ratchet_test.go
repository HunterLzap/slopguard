package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadBaselineWithBOMAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"metrics":{"ts_ignore":3}}`)...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	baseline, err := LoadBaseline(path)
	if err != nil {
		t.Fatalf("LoadBaseline error: %v", err)
	}
	if baseline.Version != BaselineVersion {
		t.Fatalf("Version = %d, want %d", baseline.Version, BaselineVersion)
	}
	if baseline.Metrics["ts_ignore"] != 3 {
		t.Fatalf("metrics = %v", baseline.Metrics)
	}
	if baseline.SuppressedFiles == nil || len(baseline.SuppressedFiles) != 0 {
		t.Fatalf("SuppressedFiles = %v", baseline.SuppressedFiles)
	}
}

func TestLoadBaselineRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"negative.json": `{"version":1,"metrics":{"ts_ignore":-1}}`,
		"version.json":  `{"version":2,"metrics":{}}`,
		"broken.json":   `{not json`,
	}
	for name, content := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBaseline(path); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestLoadBaselineOrEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "baseline.json")
	baseline, err := LoadBaselineOrEmpty(missing)
	if err != nil {
		t.Fatalf("LoadBaselineOrEmpty error: %v", err)
	}
	if baseline == nil || len(baseline.Metrics) != 0 {
		t.Fatalf("baseline = %+v", baseline)
	}
}

func TestSaveBaselineStableJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "baseline.json")
	baseline := NewBaselineFromMetrics(map[string]int{"b": 2, "a": 1}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := SaveBaseline(path, baseline); err != nil {
		t.Fatalf("SaveBaseline error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatal("baseline does not end with newline")
	}
	text := string(data)
	ai := strings.Index(text, `"a": 1`)
	bi := strings.Index(text, `"b": 2`)
	if ai < 0 || bi < 0 || ai > bi {
		t.Fatalf("metrics not sorted by key:\n%s", text)
	}
	if strings.Contains(text, "\r\n") {
		t.Fatal("baseline contains CRLF")
	}
	if _, err := LoadBaseline(path); err != nil {
		t.Fatalf("round-trip LoadBaseline: %v", err)
	}
}

func TestCompareRatchet(t *testing.T) {
	baseline := NewBaselineFromMetrics(map[string]int{"ts_ignore": 1, "eslint_disable": 2, "explicit_any": 0}, time.Time{})
	current := map[string]int{
		"ts_ignore":      2, // regression +1
		"eslint_disable": 1, // improvement -1
		"explicit_any":   0,
		"new_metric":     3, // missing in baseline => regression 3
	}
	result := Compare(current, baseline)
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if len(result.Regressions) != 2 {
		t.Fatalf("regressions = %+v", result.Regressions)
	}
	if result.Regressions[0].Name != "new_metric" || result.Regressions[1].Name != "ts_ignore" {
		t.Fatalf("regressions order = %+v", result.Regressions)
	}
	if result.Regressions[1].Delta != 1 || result.Regressions[1].Baseline != 1 || result.Regressions[1].Current != 2 {
		t.Fatalf("ts_ignore delta = %+v", result.Regressions[1])
	}
	if len(result.Improvements) != 1 || result.Improvements[0].Name != "eslint_disable" || result.Improvements[0].Delta != -1 {
		t.Fatalf("improvements = %+v", result.Improvements)
	}
}

func TestComparePassAndNilBaseline(t *testing.T) {
	baseline := NewBaselineFromMetrics(map[string]int{"ts_ignore": 2}, time.Time{})
	result := Compare(map[string]int{"ts_ignore": 2}, baseline)
	if !result.Passed || len(result.Regressions) != 0 {
		t.Fatalf("result = %+v, want pass", result)
	}
	result = Compare(map[string]int{}, nil)
	if !result.Passed {
		t.Fatalf("nil baseline result = %+v, want pass", result)
	}
	result = Compare(map[string]int{"ts_ignore": 1}, nil)
	if result.Passed || len(result.Regressions) != 1 {
		t.Fatalf("nil baseline with current 1 = %+v, want one regression", result)
	}
}

func TestTightenOnlyDecreases(t *testing.T) {
	baseline := NewBaselineFromMetrics(map[string]int{"a": 5, "b": 3, "c": 2}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	before := baseline.UpdatedAt
	changed := baseline.Tighten(map[string]int{"a": 4, "b": 5, "d": 1}, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if len(changed) != 1 || changed[0].Name != "a" || changed[0].Baseline != 5 || changed[0].Current != 4 {
		t.Fatalf("changed = %+v", changed)
	}
	if baseline.Metrics["a"] != 4 || baseline.Metrics["b"] != 3 || baseline.Metrics["c"] != 2 {
		t.Fatalf("metrics = %v", baseline.Metrics)
	}
	if _, exists := baseline.Metrics["d"]; exists {
		t.Fatal("Tighten added a new metric")
	}
	if !baseline.UpdatedAt.After(before) {
		t.Fatal("UpdatedAt was not advanced")
	}

	unchanged := baseline.Tighten(map[string]int{"a": 4}, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if len(unchanged) != 0 {
		t.Fatalf("unchanged = %+v", unchanged)
	}
}

func TestResetBaseline(t *testing.T) {
	baseline := NewBaselineFromMetrics(map[string]int{"a": 9}, time.Time{})
	baseline.SuppressedFiles = []string{"src/*.gen.ts"}
	baseline.Reset(map[string]int{"a": 2, "b": 1}, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if baseline.Metrics["a"] != 2 || baseline.Metrics["b"] != 1 {
		t.Fatalf("metrics = %v", baseline.Metrics)
	}
	if len(baseline.SuppressedFiles) != 1 {
		t.Fatalf("suppressed files not preserved: %v", baseline.SuppressedFiles)
	}
	if baseline.Version != BaselineVersion || baseline.UpdatedAt.IsZero() {
		t.Fatalf("baseline metadata = %+v", baseline)
	}
}

func TestIsSuppressed(t *testing.T) {
	baseline := NewBaseline(time.Time{})
	baseline.SuppressedFiles = []string{"src/generated.ts", "src/*.gen.ts"}
	if !baseline.IsSuppressed(filepath.FromSlash("src/generated.ts")) {
		t.Fatal("exact match not suppressed")
	}
	if !baseline.IsSuppressed(filepath.FromSlash("src/api.gen.ts")) {
		t.Fatal("glob match not suppressed")
	}
	if baseline.IsSuppressed(filepath.FromSlash("src/deep/api.gen.ts")) {
		t.Fatal("path.Match should not cross directory separators")
	}
	if baseline.IsSuppressed("other.ts") {
		t.Fatal("unrelated file suppressed")
	}
	var nilBaseline *Baseline
	if nilBaseline.IsSuppressed("anything.ts") {
		t.Fatal("nil baseline suppression should be false")
	}
}
