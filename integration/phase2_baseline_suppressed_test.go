package integration_test

import (
	"path/filepath"
	"testing"

	"github.com/HunterLzap/slopguard/internal/engine"
)

// TestBaselineTightenResetFiltersSuppressedFiles ensures baseline maintenance
// uses the same suppressed_files semantics as verify.
func TestBaselineTightenResetFiltersSuppressedFiles(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d: %s", res.Code, res.Stderr)
	}

	baselinePath := filepath.Join(root, ".slopguard", "baseline.json")
	baseline, err := engine.LoadBaseline(baselinePath)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	baseline.Metrics["ts_ignore"] = 5
	baseline.SuppressedFiles = []string{"src/index.ts"}
	if err := engine.SaveBaseline(baselinePath, baseline); err != nil {
		t.Fatalf("save baseline fixture: %v", err)
	}

	res := runCLI(t, root, nil, "baseline", "--root", root, "--tighten", "--yes")
	if res.Code != 0 {
		t.Fatalf("tighten exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	baseline, err = engine.LoadBaseline(baselinePath)
	if err != nil {
		t.Fatalf("load tightened baseline: %v", err)
	}
	if got := baseline.Metrics["ts_ignore"]; got != 0 {
		t.Fatalf("tightened ts_ignore = %d, want 0 after suppressing src/index.ts (metrics=%v)", got, baseline.Metrics)
	}

	// Add a real escape outside the suppressed file; reset must count it but
	// still exclude the suppressed file's escape.
	appendFile(t, filepath.Join(root, "src", "math.ts"), "\n// @ts-ignore\nexport const extra = 1;\n")
	res = runCLI(t, root, nil, "baseline", "--root", root, "--reset", "--yes")
	if res.Code != 0 {
		t.Fatalf("reset exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	baseline, err = engine.LoadBaseline(baselinePath)
	if err != nil {
		t.Fatalf("load reset baseline: %v", err)
	}
	if got := baseline.Metrics["ts_ignore"]; got != 1 {
		t.Fatalf("reset ts_ignore = %d, want 1 (suppressed src/index.ts excluded; metrics=%v)", got, baseline.Metrics)
	}
	if len(baseline.SuppressedFiles) != 1 || baseline.SuppressedFiles[0] != "src/index.ts" {
		t.Fatalf("reset suppressed_files = %v, want src/index.ts preserved", baseline.SuppressedFiles)
	}
}
