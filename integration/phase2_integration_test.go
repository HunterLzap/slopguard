package integration_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/HunterLzap/slopguard/internal/engine"
	"github.com/HunterLzap/slopguard/internal/radar"
)

const knipWindowsStub = `@echo off
echo {"issues":[{"file":"src/math.ts","issues":[{"type":"unused","symbol":"math","line":1}]}]}
exit /b 0
`

const knipUnixStub = `#!/bin/sh
echo '{"issues":[{"file":"src/math.ts","issues":[{"type":"unused","symbol":"math","line":1}]}]}'
exit 0
`

const jscpdWindowsStub = `@echo off
echo Clone found:
echo  - src/ref.ts [1:1 - 2:2] (2 lines, 10 tokens)
exit /b 0
`

const jscpdUnixStub = `#!/bin/sh
printf 'Clone found:\n - src/ref.ts [1:1 - 2:2] (2 lines, 10 tokens)\n'
exit 0
`

func TestRadarCLIJSON(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	if err := os.WriteFile(filepath.Join(root, "src", "ref.ts"), []byte(`import { describe } from "./index";
export const value = describe(1);
`), 0o644); err != nil {
		t.Fatal(err)
	}
	installStub(t, root, "knip", knipWindowsStub, knipUnixStub)
	installStub(t, root, "jscpd", jscpdWindowsStub, jscpdUnixStub)

	res := runCLI(t, root, nil, "radar", "--root", root, "--top", "10", "--json")
	if res.Code != 0 {
		t.Fatalf("radar exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	var result radar.Result
	if err := json.Unmarshal([]byte(res.Stdout), &result); err != nil {
		t.Fatalf("decode radar JSON: %v\n%s", err, res.Stdout)
	}
	if len(result.Hotspots) == 0 {
		t.Fatal("radar returned no hotspots")
	}
	var indexHotspot *radar.Hotspot
	var mathHotspot, refHotspot *radar.Hotspot
	for i := range result.Hotspots {
		hotspot := &result.Hotspots[i]
		switch hotspot.File {
		case "src/index.ts":
			indexHotspot = hotspot
		case "src/math.ts":
			mathHotspot = hotspot
		case "src/ref.ts":
			refHotspot = hotspot
		}
	}
	if indexHotspot == nil {
		t.Fatalf("src/index.ts missing from hotspots: %+v", result.Hotspots)
	}
	if indexHotspot.References < 1 || indexHotspot.Escapes < 1 || indexHotspot.RiskScore <= 0 {
		t.Fatalf("src/index.ts metrics = %+v", *indexHotspot)
	}
	if mathHotspot == nil || !mathHotspot.DeadCode {
		t.Fatalf("src/math.ts dead_code not marked: %+v", result.Hotspots)
	}
	if refHotspot == nil || !refHotspot.Duplicate {
		t.Fatalf("src/ref.ts duplicate not marked: %+v", result.Hotspots)
	}
}

func TestRadarCLIDegradesWhenToolsMissing(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	res := runCLI(t, root, nil, "radar", "--root", root, "--top", "3", "--json")
	if res.Code != 0 {
		t.Fatalf("radar exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	var result radar.Result
	if err := json.Unmarshal([]byte(res.Stdout), &result); err != nil {
		t.Fatalf("decode radar JSON: %v", err)
	}
	dimensions := map[string]bool{}
	for _, skipped := range result.Skipped {
		dimensions[skipped.Dimension] = true
	}
	if !dimensions["dead_code"] || !dimensions["duplicate"] {
		t.Fatalf("skipped = %+v, want dead_code and duplicate", result.Skipped)
	}
}

func TestRadarReworkFromRealGit(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	appendFile(t, filepath.Join(root, "src", "math.ts"), "\n// @ts-ignore\nexport const reworkMarker = 1;\n")
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.email", "qa@example.com")
	runGit(t, root, "config", "user.name", "QA")
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-qm", "initial")
	appendFile(t, filepath.Join(root, "src", "math.ts"), "\n// fix bug in math\nexport const fixedValue = 2;\n")
	runGit(t, root, "add", "src/math.ts")
	runGit(t, root, "commit", "-qm", "fix: handle math edge case")

	res := runCLI(t, root, nil, "radar", "--root", root, "--top", "10", "--json")
	if res.Code != 0 {
		t.Fatalf("radar exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	var result radar.Result
	if err := json.Unmarshal([]byte(res.Stdout), &result); err != nil {
		t.Fatalf("decode radar JSON: %v", err)
	}
	var math *radar.Hotspot
	for i := range result.Hotspots {
		if result.Hotspots[i].File == "src/math.ts" {
			math = &result.Hotspots[i]
		}
	}
	if math == nil {
		t.Fatalf("src/math.ts missing from hotspots: %+v", result.Hotspots)
	}
	if math.Rework != 1 {
		t.Fatalf("src/math.ts rework = %d, want 1", math.Rework)
	}
}

func TestBaselineTightenAndResetCLI(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d: %s", res.Code, res.Stderr)
	}
	appendFile(t, filepath.Join(root, "src", "index.ts"), "\n// @ts-ignore\nexport const extra = 2;\n")

	baselinePath := filepath.Join(root, ".slopguard", "baseline.json")
	baseline, err := engine.LoadBaseline(baselinePath)
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	if baseline.Metrics["ts_ignore"] != 1 {
		t.Fatalf("initial baseline ts_ignore = %d, want 1", baseline.Metrics["ts_ignore"])
	}
	baseline.Metrics["ts_ignore"] = 5
	if err := engine.SaveBaseline(baselinePath, baseline); err != nil {
		t.Fatalf("save loose baseline: %v", err)
	}

	res := runCLI(t, root, nil, "baseline", "--root", root, "--tighten", "--yes")
	if res.Code != 0 {
		t.Fatalf("tighten exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	baseline, err = engine.LoadBaseline(baselinePath)
	if err != nil {
		t.Fatalf("load tightened baseline: %v", err)
	}
	if baseline.Metrics["ts_ignore"] != 2 {
		t.Fatalf("tightened ts_ignore = %d, want 2 (metrics=%v)", baseline.Metrics["ts_ignore"], baseline.Metrics)
	}
	if !strings.Contains(res.Stdout, "Updated .slopguard/baseline.json") {
		t.Fatalf("tighten stdout:\n%s", res.Stdout)
	}

	appendFile(t, filepath.Join(root, "src", "index.ts"), "// @ts-ignore\nexport const extra2 = 3;\n")
	res = runCLI(t, root, nil, "baseline", "--root", root, "--reset", "--yes")
	if res.Code != 0 {
		t.Fatalf("reset exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	baseline, err = engine.LoadBaseline(baselinePath)
	if err != nil {
		t.Fatalf("load reset baseline: %v", err)
	}
	if baseline.Metrics["ts_ignore"] != 3 {
		t.Fatalf("reset ts_ignore = %d, want 3 (metrics=%v)", baseline.Metrics["ts_ignore"], baseline.Metrics)
	}
}

func TestVerifyRecordScanAndGracefulFailure(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)
	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d: %s", res.Code, res.Stderr)
	}

	home := t.TempDir()
	homeEnv := []string{"USERPROFILE=" + home, "HOME=" + home}
	res := runCLI(t, root, homeEnv, "verify", "--all", "--json", "--record", "--root", root)
	if res.Code != 0 {
		t.Fatalf("verify --record exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	dbPath := filepath.Join(home, ".slopguard", "slopguard.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("scan_runs database not created at %s: %v", dbPath, err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open recorded database: %v", err)
	}
	var scanCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scan_runs`).Scan(&scanCount); err != nil {
		db.Close()
		t.Fatalf("query scan_runs: %v", err)
	}
	db.Close()
	if scanCount != 1 {
		t.Fatalf("scan_runs rows = %d, want 1", scanCount)
	}

	// A non-writable home path must only warn and keep verify exit 0.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = runCLI(t, root, []string{"USERPROFILE=" + blocked, "HOME=" + blocked}, "verify", "--all", "--json", "--record", "--root", root)
	if res.Code != 0 {
		t.Fatalf("verify --record with blocked home exit = %d, want 0\nstderr:\n%s", res.Code, res.Stderr)
	}
	if !bytes.Contains([]byte(res.Stderr), []byte("warning: record scan failed")) {
		t.Fatalf("stderr = %q, want record warning", res.Stderr)
	}
}
