package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
	"github.com/HunterLzap/slopguard/internal/store"
)

func TestMistakeClusteringFingerprintConsistentAcrossVarsAndFiles(t *testing.T) {
	project := t.TempDir()

	f1 := contract.Finding{
		File:     "app/auth.py",
		Line:     12,
		Rule:     "pyright:reportOptionalMemberAccess",
		Message:  `Cannot access attribute "user_id" for class "None"`,
		Severity: contract.SeverityError,
	}
	f2 := contract.Finding{
		File:     "app/billing/invoice.py",
		Line:     88,
		Rule:     "pyright:reportOptionalMemberAccess",
		Message:  `Cannot access attribute "total_cents" for class "None"`,
		Severity: contract.SeverityError,
	}

	fp1 := FindingFingerprint(project, f1, "python")
	fp2 := FindingFingerprint(project, f2, "python")
	if fp1 == "" || fp1 != fp2 {
		t.Fatalf("same adapter+ruleID produced different fingerprints: %q vs %q", fp1, fp2)
	}

	// Empty ruleID falls back to message sanitization (stripping quoted identifiers, paths, and numbers).
	noRule1 := contract.Finding{
		File:    "src/users.ts",
		Line:    10,
		Rule:    "eslint",
		Message: `Unexpected reference to 'fooBar' in src/users.ts:10:4`,
	}
	noRule2 := contract.Finding{
		File:    "src/orders.ts",
		Line:    245,
		Rule:    "eslint",
		Message: `Unexpected reference to 'bazQux' in src/orders.ts:245:19`,
	}
	noRuleFP1 := FindingFingerprint(project, noRule1, "typescript")
	noRuleFP2 := FindingFingerprint(project, noRule2, "typescript")
	if noRuleFP1 != noRuleFP2 {
		t.Fatalf("empty ruleID fingerprints diverged: %q vs %q", noRuleFP1, noRuleFP2)
	}

	distinct := contract.Finding{
		Rule:    "eslint",
		Message: `Missing return statement in function`,
	}
	if fpOther := FindingFingerprint(project, distinct, "typescript"); fpOther == noRuleFP1 {
		t.Fatalf("distinct error messages collapsed to same fingerprint %q", fpOther)
	}
}

func TestAutoCaptureClustersIntoSQLiteCount(t *testing.T) {
	project := t.TempDir()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	type stubRecorder struct {
		s *store.Store
	}
	rec := &struct {
		*store.Store
	}{db}

	for _, varName := range []string{"foo", "bar"} {
		annotated := []annotatedFinding{{
			adapter: "python",
			finding: contract.Finding{
				File:     "app/" + varName + ".py",
				Line:     7,
				Rule:     "pyright:reportOptionalMemberAccess",
				Message:  `Cannot access attribute "` + varName + `" for class "None"`,
				Severity: contract.SeverityError,
			},
		}}
		res := contract.VerifyResult{Passed: false, Findings: []contract.Finding{annotated[0].finding}}
		autoCaptureMistakes(context.Background(), rec, project, annotated, res.Findings, "auto_verify", &res)
	}

	candidates, err := db.ListCandidatesScoped(context.Background(), project, 2)
	if err != nil {
		t.Fatalf("ListCandidatesScoped: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 clustered candidate with count>=2, got %+v", candidates)
	}
	_ = stubRecorder{}
}

func TestRunMissingBaselineUnifiedNonBlockingBehavior(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"name":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "src", "index.ts"), []byte("export const x = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := stubRunner{}
	res, err := Run(context.Background(), Options{
		ProjectRoot: project,
		All:         true,
		Runner:      runner,
	})
	if err != nil {
		t.Fatalf("default missing baseline should not fail, got err: %v", err)
	}
	if !res.Passed {
		t.Fatalf("expected passed=true, got %+v", res)
	}
	joinedWarnings := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joinedWarnings, "baseline.json not found") {
		t.Fatalf("expected non-blocking baseline warning, got %+v", res.Warnings)
	}

	// Explicit StrictBaseline returns an error.
	_, err = Run(context.Background(), Options{
		ProjectRoot:    project,
		All:            true,
		StrictBaseline: true,
		Runner:         runner,
	})
	if err == nil || !strings.Contains(err.Error(), "run slopguard init first") {
		t.Fatalf("StrictBaseline error = %v, want init guidance", err)
	}
}

type stubRunner struct{}

func (stubRunner) Run(context.Context, contract.RunSpec) contract.RunResult {
	return contract.RunResult{ExitCode: 0}
}

func (stubRunner) LookPath(name string) (string, bool) {
	return "/usr/bin/" + name, true
}

type selectiveRunner struct {
	available map[string]bool
	runFn     func(contract.RunSpec) contract.RunResult
}

func (r selectiveRunner) LookPath(name string) (string, bool) {
	if r.available[name] {
		return "/usr/bin/" + name, true
	}
	return "", false
}

func (r selectiveRunner) Run(_ context.Context, spec contract.RunSpec) contract.RunResult {
	if r.runFn != nil {
		return r.runFn(spec)
	}
	return contract.RunResult{ExitCode: 0}
}

func TestRunGracefulDegradationPythonOnlyRuffAndZeroLinters(t *testing.T) {
	// Scenario 1: Python repo with only ruff installed (pyright missing) -> passes and records pyright in skipped.
	pyProj := t.TempDir()
	if err := os.WriteFile(filepath.Join(pyProj, "pyproject.toml"), []byte("[project]\nname='demo'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pyProj, "app.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pyProj, ".slopguard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pyProj, ".slopguard", "baseline.json"), []byte(`{"version":1,"metrics":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	onlyRuff := selectiveRunner{available: map[string]bool{"ruff": true}}
	res, err := Run(context.Background(), Options{ProjectRoot: pyProj, All: true, Runner: onlyRuff})
	if err != nil {
		t.Fatalf("only-ruff Run failed: %v", err)
	}
	if !res.Passed {
		t.Fatalf("only-ruff expected Passed=true, got %+v", res)
	}
	if len(res.SkippedDetails) == 0 || res.SkippedDetails[0].ID != "pyright" || !strings.Contains(res.SkippedDetails[0].Reason, "pyright (not found)") {
		t.Fatalf("skipped_details = %+v, want pyright (not found)", res.SkippedDetails)
	}

	// Scenario 2: Zero external linters, but built-in escape ratchet still catches new escape hatch!
	if err := os.WriteFile(filepath.Join(pyProj, "app.py"), []byte("x = 1  # noqa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zeroLinters := selectiveRunner{available: map[string]bool{}}
	resZero, err := Run(context.Background(), Options{ProjectRoot: pyProj, All: true, Runner: zeroLinters})
	if err != nil {
		t.Fatalf("zero-linters Run failed: %v", err)
	}
	if resZero.Passed {
		t.Fatalf("expected built-in ratchet to block # noqa even with zero external linters")
	}
	if len(resZero.Findings) == 0 || resZero.Findings[0].Rule != "ratchet:noqa" {
		t.Fatalf("findings = %+v, want ratchet:noqa", resZero.Findings)
	}
	if len(resZero.Skipped) < 2 {
		t.Fatalf("skipped = %v, want ruff/pyright/semgrep recorded", resZero.Skipped)
	}

	// Scenario 3: StrictTools=true rejects missing pyright.
	if _, err := Run(context.Background(), Options{ProjectRoot: pyProj, All: true, StrictTools: true, Runner: onlyRuff}); err == nil {
		t.Fatal("expected error under StrictTools=true when pyright is missing")
	}
}

func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestRunIncrementalDiffRatchetProbesAndStaged(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "app.ts"), []byte("// @ts-ignore\nexport const baseVal = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".slopguard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".slopguard", "baseline.json"), []byte(`{"version":1,"metrics":{"ts_ignore":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	runGitCmd(t, repo, "init", "-q")
	runGitCmd(t, repo, "config", "user.email", "test@example.com")
	runGitCmd(t, repo, "config", "user.name", "Test")
	runGitCmd(t, repo, "add", "-A")
	runGitCmd(t, repo, "commit", "-qm", "init")

	// Stage a change that removes @ts-ignore (improvement), while keeping an unstaged workaround comment.
	if err := os.WriteFile(filepath.Join(repo, "src", "app.ts"), []byte("export const baseVal = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, repo, "add", "src/app.ts")
	// Now add unstaged probe comment + new @ts-ignore in working tree (not staged).
	if err := os.WriteFile(filepath.Join(repo, "src", "app.ts"), []byte("// @ts-ignore\n// do not remove\n// workaround for bug\nexport const baseVal = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	realRunner := engine.NewExecRunner()
	t.Setenv("PATH", filepath.Dir(mustWhichGit(t))+":"+t.TempDir())

	// --staged sees ONLY the staged removal -> passes and reports Improvements + tighten hint!
	stagedRes, err := Run(context.Background(), Options{ProjectRoot: repo, Staged: true, Runner: realRunner})
	if err != nil {
		t.Fatalf("staged verify err: %v", err)
	}
	if !stagedRes.Passed {
		t.Fatalf("staged verify expected pass, got findings: %+v", stagedRes.Findings)
	}
	if len(stagedRes.Improvements) != 1 || stagedRes.Improvements[0].Name != "ts_ignore" || stagedRes.Improvements[0].Delta != -1 {
		t.Fatalf("staged improvements = %+v, want ts_ignore -1", stagedRes.Improvements)
	}
	if !strings.Contains(stagedRes.Hint, "slopguard baseline --tighten") {
		t.Fatalf("staged hint = %q, want tighten command", stagedRes.Hint)
	}

	// Default working-tree diff sees the added @ts-ignore (net 0 vs HEAD, wait: HEAD had 1, working tree has 1 -> delta 0!)
	// AND sees line 2 (do not remove) + line 3 (workaround for bug)!
	workRes, err := Run(context.Background(), Options{ProjectRoot: repo, Runner: realRunner})
	if err != nil {
		t.Fatalf("work verify err: %v", err)
	}
	if workRes.Passed {
		t.Fatalf("expected probes to block working tree diff")
	}
	var gotConstraint, gotWorkaround bool
	for _, f := range workRes.Findings {
		if f.Rule == engine.ProbeConstraintRuleID && f.Line == 2 {
			gotConstraint = true
		}
		if f.Rule == engine.ProbeWorkaroundRuleID && f.Line == 3 {
			gotWorkaround = true
		}
	}
	if !gotConstraint || !gotWorkaround {
		t.Fatalf("expected constraint@2 and workaround@3, got %+v", workRes.Findings)
	}

	// Configure probes.severity: warning -> non-blocking!
	if err := os.WriteFile(filepath.Join(repo, ".slopguard", "config.yaml"), []byte("version: 1\nadapters:\n  - typescript\nprobes:\n  enabled: true\n  severity: warning\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	warnRes, err := Run(context.Background(), Options{ProjectRoot: repo, Runner: realRunner})
	if err != nil {
		t.Fatalf("warn verify err: %v", err)
	}
	if !warnRes.Passed || len(warnRes.Findings) != 2 {
		t.Fatalf("expected Passed=true with 2 warning findings, got passed=%v findings=%+v", warnRes.Passed, warnRes.Findings)
	}
}

func TestRunVerifyWithAblationConfig(t *testing.T) {
	mustWhichGit(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary required")
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/verifyablation\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "calc.go"), []byte("package demo\nfunc Mul(a, b int) int { return a + b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "calc_test.go"), []byte("package demo\nimport \"testing\"\nfunc TestZero(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, repo, "init", "-q")
	runGitCmd(t, repo, "config", "user.email", "test@example.com")
	runGitCmd(t, repo, "config", "user.name", "Test")
	runGitCmd(t, repo, "add", "-A")
	runGitCmd(t, repo, "commit", "-qm", "buggy head")

	// Missing test_cmd returns error.
	if _, err := Run(context.Background(), Options{ProjectRoot: repo, Ablation: true}); err == nil || !strings.Contains(err.Error(), "verify.test_cmd") {
		t.Fatalf("expected missing verify.test_cmd error, got %v", err)
	}

	if err := os.MkdirAll(filepath.Join(repo, ".slopguard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".slopguard", "config.yaml"), []byte("version: 1\nadapters: []\nverify:\n  test_cmd: \"go test .\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Real fix + regression test -> passed with ablation metrics.
	if err := os.WriteFile(filepath.Join(repo, "calc.go"), []byte("package demo\nfunc Mul(a, b int) int { return a * b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "calc_test.go"), []byte("package demo\nimport \"testing\"\nfunc TestMul(t *testing.T) { if Mul(3,4) != 12 { t.Fatal(\"bad\") } }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(context.Background(), Options{ProjectRoot: repo, Ablation: true})
	if err != nil {
		t.Fatalf("verify --ablation error: %v", err)
	}
	if !res.Passed || res.Ablation == nil || res.Ablation.Status != "passed" {
		t.Fatalf("expected passed ablation result, got passed=%v ablation=%+v findings=%+v", res.Passed, res.Ablation, res.Findings)
	}
}

func mustWhichGit(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary required for git diff test")
	}
	return p
}
