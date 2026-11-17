package radar

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type radarStubRunner struct {
	looks map[string]bool
	run   func(contract.RunSpec) contract.RunResult
}

func (r *radarStubRunner) Run(_ context.Context, spec contract.RunSpec) contract.RunResult {
	if r.run != nil {
		return r.run(spec)
	}
	return contract.RunResult{ExitCode: 0}
}

func (r *radarStubRunner) LookPath(name string) (string, bool) {
	if ok, exists := r.looks[name]; exists {
		return name, ok
	}
	return name, true
}

func writeRadarFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeTSRadarProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeRadarFile(t, root, "package.json", `{"name":"radar-fixture"}`)
	writeRadarFile(t, root, "src/a.ts", `import { b } from "./b";
import { c } from "./c";
export function a(x: number): number {
  if (x > 0) {
    return x;
  }
  return b + c;
}
`)
	writeRadarFile(t, root, "src/b.ts", "export const b = 1;\n")
	writeRadarFile(t, root, "src/c.ts", "export const c = 1;\n")
	writeRadarFile(t, root, "src/d.ts", "// @ts-ignore\nexport const d = 1;\n")
	return root
}

func fullRadarRunner() *radarStubRunner {
	hash := strings.Repeat("a", 40)
	return &radarStubRunner{
		looks: map[string]bool{"knip": true, "jscpd": true, "git": true},
		run: func(spec contract.RunSpec) contract.RunResult {
			switch {
			case spec.Name == "knip":
				return contract.RunResult{ExitCode: 0, Stdout: `{"issues":[{"file":"src/b.ts","issues":[{"type":"unused","symbol":"b","line":1}]}]}`}
			case spec.Name == "jscpd":
				return contract.RunResult{ExitCode: 0, Stdout: "Clone found:\n - src/c.ts [1:1 - 2:2] (2 lines, 10 tokens)\n"}
			case spec.Name == "git" && containsArg(spec.Args, "rev-parse"):
				return contract.RunResult{ExitCode: 0, Stdout: "true\n"}
			case spec.Name == "git" && containsArg(spec.Args, "log"):
				return contract.RunResult{ExitCode: 0, Stdout: hash + "\tfix: crash\r\nsrc/c.ts\r\n"}
			default:
				return contract.RunResult{ExitCode: 0}
			}
		},
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestScanReportRiskFormulaAndFields(t *testing.T) {
	root := makeTSRadarProject(t)
	result, err := ScanReport(context.Background(), Options{Root: root, Top: 3, Runner: fullRadarRunner()})
	if err != nil {
		t.Fatalf("ScanReport error: %v", err)
	}
	if len(result.Hotspots) != 3 {
		t.Fatalf("hotspots = %+v, want 3", result.Hotspots)
	}
	if result.Hotspots[0].File != "src/c.ts" {
		t.Fatalf("first hotspot = %+v, want src/c.ts", result.Hotspots[0])
	}
	c := result.Hotspots[0]
	if c.References != 1 || c.Complexity != 1 || c.Escapes != 0 || c.Rework != 1 || c.RiskScore != 2 {
		t.Fatalf("src/c.ts metrics = %+v, want refs=1 complexity=1 rework=1 risk=2", c)
	}
	if !c.Duplicate {
		t.Fatalf("src/c.ts duplicate = false, want true")
	}
	b := result.Hotspots[1]
	if b.File != "src/b.ts" || b.RiskScore != 1 || !b.DeadCode {
		t.Fatalf("src/b.ts = %+v, want dead code risk 1", b)
	}
	d := result.Hotspots[2]
	if d.File != "src/d.ts" || d.Escapes != 1 || d.RiskScore != 0 {
		t.Fatalf("src/d.ts = %+v, want escapes=1 risk=0", d)
	}
	if len(result.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", result.Skipped)
	}
}

func TestScanTopLimit(t *testing.T) {
	root := makeTSRadarProject(t)
	hotspots, err := Scan(context.Background(), Options{Root: root, Top: 1, Runner: fullRadarRunner()})
	if err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	if len(hotspots) != 1 || hotspots[0].File != "src/c.ts" {
		t.Fatalf("hotspots = %+v, want only src/c.ts", hotspots)
	}
}

func TestScanReportDegradesWhenToolsMissing(t *testing.T) {
	root := makeTSRadarProject(t)
	runner := &radarStubRunner{looks: map[string]bool{"knip": false, "jscpd": false, "git": false}}
	result, err := ScanReport(context.Background(), Options{Root: root, Top: 10, Runner: runner})
	if err != nil {
		t.Fatalf("ScanReport error: %v", err)
	}
	if len(result.Skipped) != 3 {
		t.Fatalf("skipped = %+v, want dead_code/duplicate/rework", result.Skipped)
	}
	dimensions := map[string]bool{}
	for _, skipped := range result.Skipped {
		dimensions[skipped.Dimension] = true
	}
	for _, want := range []string{"dead_code", "duplicate", "rework"} {
		if !dimensions[want] {
			t.Fatalf("missing skipped dimension %q: %+v", want, result.Skipped)
		}
	}
	for _, hotspot := range result.Hotspots {
		if hotspot.DeadCode || hotspot.Duplicate {
			t.Fatalf("hotspot has dead/duplicate despite missing tools: %+v", hotspot)
		}
	}
}

func TestScanReportNilRunnerDoesNotPanic(t *testing.T) {
	root := makeTSRadarProject(t)
	result, err := ScanReport(context.Background(), Options{Root: root, Top: 10})
	if err != nil {
		t.Fatalf("ScanReport error: %v", err)
	}
	if len(result.Skipped) == 0 {
		t.Fatalf("skipped = %+v, want graceful degradation", result.Skipped)
	}
}

func TestScanReportNoAdapters(t *testing.T) {
	result, err := ScanReport(context.Background(), Options{Root: t.TempDir(), Runner: &radarStubRunner{}})
	if err != nil {
		t.Fatalf("ScanReport error: %v", err)
	}
	if len(result.Hotspots) != 0 {
		t.Fatalf("hotspots = %+v, want empty", result.Hotspots)
	}
}

func TestScanReportInvalidRoot(t *testing.T) {
	if _, err := ScanReport(context.Background(), Options{Root: ""}); err == nil {
		t.Fatal("empty root should error")
	}
	if _, err := ScanReport(context.Background(), Options{Root: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing root should error")
	}
}

func TestEstimateComplexity(t *testing.T) {
	content := strings.Join([]string{
		"// if this comment should be ignored",
		"if (x) {",
		"  for (;;) {}",
		"}",
		"const ok = a && b || c;",
	}, "\n")
	// base 1 + if + for + && + || = 5; comment line ignored.
	if got := estimateComplexity(content); got != 5 {
		t.Fatalf("estimateComplexity = %d, want 5", got)
	}
	if got := countLines(""); got != 0 {
		t.Fatalf("countLines(empty) = %d, want 0", got)
	}
	if got := countLines("a\nb"); got != 2 {
		t.Fatalf("countLines = %d, want 2", got)
	}
}

func TestExtractImportsTSAndPython(t *testing.T) {
	ts := `import { b } from "./b";
export { c } from "./c";
const d = require("./d");
const e = import("./e");`
	specs := extractImports("src/a.ts", ts)
	wantTS := map[string]bool{"./b": true, "./c": true, "./d": true, "./e": true}
	if len(specs) != len(wantTS) {
		t.Fatalf("TS specs = %v", specs)
	}
	for _, spec := range specs {
		if !wantTS[spec] {
			t.Fatalf("unexpected TS spec %q (all %v)", spec, specs)
		}
	}

	py := `from app.util import helper, other
import app.models
`
	specs = extractImports("app/main.py", py)
	if len(specs) < 2 {
		t.Fatalf("Python specs = %v", specs)
	}
	found := false
	for _, spec := range specs {
		if spec == "app.util" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Python specs missing app.util: %v", specs)
	}
}

func TestBuildReferencesPython(t *testing.T) {
	metrics := map[string]*fileMetric{
		"app/main.py": {imports: extractImports("app/main.py", "from app.util import helper\n")},
		"app/util.py": {imports: nil},
	}
	refs := buildReferences(metrics, []string{".py"})
	if refs["app/util.py"] != 1 {
		t.Fatalf("refs = %v, want app/util.py=1", refs)
	}
}

func TestScanReportPythonVulture(t *testing.T) {
	root := t.TempDir()
	writeRadarFile(t, root, "pyproject.toml", "[project]\nname = \"radar-py\"\n")
	writeRadarFile(t, root, "app/main.py", `from app.util import helper

def run(value):
    if value:
        return helper()
    return 0
`)
	writeRadarFile(t, root, "app/util.py", "def helper():\n    return 1\n")
	runner := &radarStubRunner{
		looks: map[string]bool{"vulture": true, "jscpd": false, "git": false},
		run: func(spec contract.RunSpec) contract.RunResult {
			if spec.Name == "vulture" {
				return contract.RunResult{ExitCode: 0, Stdout: "app/util.py:1: unused function 'helper' (60% confidence)\n"}
			}
			return contract.RunResult{ExitCode: 0}
		},
	}
	result, err := ScanReport(context.Background(), Options{Root: root, Top: 10, Runner: runner})
	if err != nil {
		t.Fatalf("ScanReport error: %v", err)
	}
	var util *Hotspot
	for i := range result.Hotspots {
		if result.Hotspots[i].File == "app/util.py" {
			util = &result.Hotspots[i]
		}
	}
	if util == nil || !util.DeadCode || util.References != 1 {
		t.Fatalf("app/util.py hotspot = %+v, all = %+v", util, result.Hotspots)
	}
	foundDuplicateSkip := false
	for _, skipped := range result.Skipped {
		if skipped.Dimension == "duplicate" {
			foundDuplicateSkip = true
		}
	}
	if !foundDuplicateSkip {
		t.Fatalf("expected duplicate skipped when jscpd missing: %+v", result.Skipped)
	}
}
