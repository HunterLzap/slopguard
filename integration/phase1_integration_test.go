package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
)

var (
	slopguardBin string
	projectRoot  string
)

func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "getwd: %v\n", err)
		os.Exit(1)
	}
	projectRoot = filepath.Clean(filepath.Join(wd, ".."))
	tmp, err := os.MkdirTemp("", "slopguard-e2e-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		os.Exit(1)
	}
	name := "slopguard"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	slopguardBin = filepath.Join(tmp, name)
	build := exec.Command("go", "build", "-o", slopguardBin, "./cmd/slopguard")
	build.Dir = projectRoot
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build slopguard: %v\n%s\n", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

type cliResult struct {
	Stdout  string
	Stderr  string
	Code    int
	Elapsed time.Duration
}

func runCLI(t *testing.T, workDir string, extraEnv []string, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command(slopguardBin, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started)
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return cliResult{Stdout: stdout.String(), Stderr: stderr.String(), Code: code, Elapsed: elapsed}
}

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	dst := t.TempDir()
	copyFixtureInto(t, name, dst)
	return dst
}

func copyFixtureInto(t *testing.T, name, dst string) {
	t.Helper()
	src := filepath.Join(projectRoot, "testdata", "fixtures", name)
	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func installStub(t *testing.T, root, tool, windowsScript, unixScript string) {
	t.Helper()
	dir := filepath.Join(root, "node_modules", ".bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if err := os.WriteFile(filepath.Join(dir, tool+".cmd"), []byte(windowsScript), 0o755); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(filepath.Join(dir, tool), []byte(unixScript), 0o755); err != nil {
		t.Fatal(err)
	}
}

const tscWindows = `@echo off
if "%FAKE_TSC_FAIL%"=="1" goto fail
exit /b 0
:fail
echo src/index.ts:1:1 - error TS2322: fake type error
exit /b 2
`

const tscUnix = `#!/bin/sh
if [ "$FAKE_TSC_FAIL" = "1" ]; then
  echo "src/index.ts:1:1 - error TS2322: fake type error"
  exit 2
fi
exit 0
`

const eslintWindows = `@echo off
if not "%FAKE_ARGS_LOG%"=="" echo %*>>"%FAKE_ARGS_LOG%"
if "%FAKE_ESLINT_FAIL%"=="1" goto fail
echo []
exit /b 0
:fail
echo [{"filePath":"src/index.ts","messages":[{"ruleId":"no-explicit-any","severity":2,"message":"fake lint","line":1,"column":1}]}]
exit /b 1
`

const eslintUnix = `#!/bin/sh
if [ -n "$FAKE_ARGS_LOG" ]; then echo "$@" >> "$FAKE_ARGS_LOG"; fi
if [ "$FAKE_ESLINT_FAIL" = "1" ]; then
  echo '[{"filePath":"src/index.ts","messages":[{"ruleId":"no-explicit-any","severity":2,"message":"fake lint","line":1,"column":1}]}]'
  exit 1
fi
echo '[]'
exit 0
`

const ruffWindows = `@echo off
if "%FAKE_RUFF_FAIL%"=="1" goto fail
echo []
exit /b 0
:fail
echo [{"code":"F401","message":"fake unused import","filename":"app/main.py","location":{"row":1,"column":8}}]
exit /b 1
`

const ruffUnix = `#!/bin/sh
if [ "$FAKE_RUFF_FAIL" = "1" ]; then
  echo '[{"code":"F401","message":"fake unused import","filename":"app/main.py","location":{"row":1,"column":8}}]'
  exit 1
fi
echo '[]'
exit 0
`

const pyrightWindows = `@echo off
if "%FAKE_PYRIGHT_FAIL%"=="1" goto fail
echo {"generalDiagnostics":[]}
exit /b 0
:fail
echo {"generalDiagnostics":[{"file":"app/main.py","severity":"error","message":"fake type","rule":"reportAssignmentType","range":{"start":{"line":0,"character":0}}}]}
exit /b 1
`

const pyrightUnix = `#!/bin/sh
if [ "$FAKE_PYRIGHT_FAIL" = "1" ]; then
  echo '{"generalDiagnostics":[{"file":"app/main.py","severity":"error","message":"fake type","rule":"reportAssignmentType","range":{"start":{"line":0,"character":0}}}]}'
  exit 1
fi
echo '{"generalDiagnostics":[]}'
exit 0
`

const goWindows = `@echo off
echo go version go1.23.0 fake/windows
exit /b 0
`

const goUnix = `#!/bin/sh
echo "go version go1.23.0 fake/unix"
exit 0
`

func installTSStubs(t *testing.T, root string) {
	t.Helper()
	installStub(t, root, "tsc", tscWindows, tscUnix)
	installStub(t, root, "eslint", eslintWindows, eslintUnix)
}

func installPythonStubs(t *testing.T, root string) {
	t.Helper()
	installStub(t, root, "ruff", ruffWindows, ruffUnix)
	installStub(t, root, "pyright", pyrightWindows, pyrightUnix)
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if _, err := handle.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func readBaseline(t *testing.T, root string) *engine.Baseline {
	t.Helper()
	baseline, err := engine.LoadBaseline(filepath.Join(root, ".slopguard", "baseline.json"))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	return baseline
}

func decodeResult(t *testing.T, stdout string) contract.VerifyResult {
	t.Helper()
	var result contract.VerifyResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode verify JSON: %v\n%s", err, stdout)
	}
	return result
}

func hasRule(findings []contract.Finding, rule string) bool {
	for _, finding := range findings {
		if finding.Rule == rule {
			return true
		}
	}
	return false
}

func TestTypeScriptRepoInitVerifyAndRatchet(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)

	res := runCLI(t, root, nil, "init", "--root", root)
	if res.Code != 0 {
		t.Fatalf("init exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	baseline := readBaseline(t, root)
	if baseline.Metrics["ts_ignore"] != 1 {
		t.Fatalf("baseline ts_ignore = %d, want 1 (metrics=%v)", baseline.Metrics["ts_ignore"], baseline.Metrics)
	}
	if baseline.Metrics["explicit_any"] != 1 {
		t.Fatalf("baseline explicit_any = %d, want 1 (metrics=%v)", baseline.Metrics["explicit_any"], baseline.Metrics)
	}
	config, err := os.ReadFile(filepath.Join(root, ".slopguard", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "- typescript") {
		t.Fatalf("config missing typescript adapter:\n%s", config)
	}

	res = runCLI(t, root, nil, "verify", "--all", "--json")
	if res.Code != 0 {
		t.Fatalf("verify exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	result := decodeResult(t, res.Stdout)
	if !result.Passed || len(result.Findings) != 0 {
		t.Fatalf("verify result = %+v", result)
	}
	if result.Escapes["ts_ignore"] != 1 {
		t.Fatalf("escapes = %v, want ts_ignore=1", result.Escapes)
	}

	appendFile(t, filepath.Join(root, "src", "index.ts"), "\n// @ts-ignore\nexport const extra = 1;\n")
	res = runCLI(t, root, nil, "verify", "--all", "--json")
	if res.Code != 1 {
		t.Fatalf("verify after new escape exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	result = decodeResult(t, res.Stdout)
	if result.Passed || !hasRule(result.Findings, "ratchet:ts_ignore") {
		t.Fatalf("findings = %+v, want ratchet:ts_ignore", result.Findings)
	}
	ratchetMessage := ""
	for _, finding := range result.Findings {
		if finding.Rule == "ratchet:ts_ignore" {
			ratchetMessage = finding.Message
		}
	}
	if !strings.Contains(ratchetMessage, "2 > baseline 1") {
		t.Fatalf("ratchet message = %q", ratchetMessage)
	}

	res = runCLI(t, root, []string{"FAKE_TSC_FAIL=1"}, "verify", "--all", "--json")
	if res.Code != 1 {
		t.Fatalf("verify with tsc failure exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	result = decodeResult(t, res.Stdout)
	if !hasRule(result.Findings, "tsc:TS2322") {
		t.Fatalf("findings = %+v, want tsc:TS2322", result.Findings)
	}
}

func TestTypeScriptRepoESLintFailure(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)
	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d: %s", res.Code, res.Stderr)
	}
	res := runCLI(t, root, []string{"FAKE_ESLINT_FAIL=1"}, "verify", "--all", "--json")
	if res.Code != 1 {
		t.Fatalf("verify exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	result := decodeResult(t, res.Stdout)
	if !hasRule(result.Findings, "eslint:no-explicit-any") {
		t.Fatalf("findings = %+v, want eslint:no-explicit-any", result.Findings)
	}
}

func TestPythonRepoInitVerifyAndRatchet(t *testing.T) {
	root := copyFixture(t, "python-repo")
	installPythonStubs(t, root)

	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	baseline := readBaseline(t, root)
	if baseline.Metrics["type_ignore_py"] != 1 || baseline.Metrics["noqa"] != 1 {
		t.Fatalf("baseline metrics = %v", baseline.Metrics)
	}

	res := runCLI(t, root, nil, "verify", "--all", "--json")
	if res.Code != 0 {
		t.Fatalf("verify exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if result := decodeResult(t, res.Stdout); !result.Passed {
		t.Fatalf("result = %+v", result)
	}

	appendFile(t, filepath.Join(root, "app", "main.py"), "\nvalue2: Any = None  # type: ignore\n")
	res = runCLI(t, root, nil, "verify", "--all", "--json")
	if res.Code != 1 {
		t.Fatalf("verify after new type ignore exit = %d, want 1", res.Code)
	}
	result := decodeResult(t, res.Stdout)
	if !hasRule(result.Findings, "ratchet:type_ignore_py") {
		t.Fatalf("findings = %+v, want ratchet:type_ignore_py", result.Findings)
	}
}

func TestPythonRepoRuffFailure(t *testing.T) {
	root := copyFixture(t, "python-repo")
	installPythonStubs(t, root)
	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d: %s", res.Code, res.Stderr)
	}
	res := runCLI(t, root, []string{"FAKE_RUFF_FAIL=1"}, "verify", "--all", "--json")
	if res.Code != 1 {
		t.Fatalf("verify exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	result := decodeResult(t, res.Stdout)
	if !hasRule(result.Findings, "ruff:F401") {
		t.Fatalf("findings = %+v, want ruff:F401", result.Findings)
	}
}

func TestYAMLRepoInitVerify(t *testing.T) {
	root := copyFixture(t, "yaml-repo")
	installStub(t, root, "go", goWindows, goUnix)

	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	baseline := readBaseline(t, root)
	if baseline.Metrics["nolint"] != 1 {
		t.Fatalf("baseline nolint = %d, want 1 (metrics=%v)", baseline.Metrics["nolint"], baseline.Metrics)
	}
	config, err := os.ReadFile(filepath.Join(root, ".slopguard", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "- go-standard") {
		t.Fatalf("config missing go-standard adapter:\n%s", config)
	}

	res := runCLI(t, root, nil, "verify", "--all", "--json")
	if res.Code != 0 {
		t.Fatalf("verify exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	result := decodeResult(t, res.Stdout)
	if !result.Passed || result.Escapes["nolint"] != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestIncrementalVerifyGitRepo(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)
	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d: %s", res.Code, res.Stderr)
	}
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.email", "qa@example.com")
	runGit(t, root, "config", "user.name", "QA")
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-qm", "fixture baseline")

	appendFile(t, filepath.Join(root, "src", "math.ts"), "\n// incremental change\n")
	argsLog := filepath.Join(t.TempDir(), "eslint-args.log")
	res := runCLI(t, root, []string{"FAKE_ARGS_LOG=" + argsLog}, "verify", "--json")
	if res.Code != 0 {
		t.Fatalf("incremental verify exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	result := decodeResult(t, res.Stdout)
	if !result.Passed {
		t.Fatalf("result = %+v", result)
	}
	if res.Elapsed >= 3*time.Second {
		t.Fatalf("incremental verify took %s, want < 3s", res.Elapsed)
	}
	if int64(result.Duration) >= 3000 {
		t.Fatalf("reported duration = %d, want < 3000ms", int64(result.Duration))
	}
	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("eslint args log: %v", err)
	}
	if !strings.Contains(string(args), "src/math.ts") && !strings.Contains(string(args), `src\math.ts`) {
		t.Fatalf("eslint did not receive changed file; args=%q", args)
	}
	if strings.Contains(string(args), "src/index.ts") || strings.Contains(string(args), `src\index.ts`) {
		t.Fatalf("eslint received unchanged file; args=%q", args)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestVerifyWithoutGitRepoNeedsAll(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)
	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d: %s", res.Code, res.Stderr)
	}
	res := runCLI(t, root, nil, "verify", "--json")
	if res.Code != 2 {
		t.Fatalf("verify exit = %d, want 2\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "not a git repository") {
		t.Fatalf("stderr = %q", res.Stderr)
	}
}

func TestInitIdempotent(t *testing.T) {
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)
	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("first init exit = %d: %s", res.Code, res.Stderr)
	}
	before, err := os.ReadFile(filepath.Join(root, ".slopguard", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, root, nil, "init", "--root", root)
	if res.Code != 0 {
		t.Fatalf("second init exit = %d: %s", res.Code, res.Stderr)
	}
	after, err := os.ReadFile(filepath.Join(root, ".slopguard", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("second init changed config:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if !strings.Contains(res.Stdout, "already up to date") || !strings.Contains(res.Stdout, "Kept existing .slopguard/baseline.json") {
		t.Fatalf("second init stdout:\n%s", res.Stdout)
	}
}

func TestCLIExitCodes(t *testing.T) {
	root := t.TempDir()
	if res := runCLI(t, root, nil); res.Code != 2 {
		t.Fatalf("no-args exit = %d, want 2", res.Code)
	}
	if res := runCLI(t, root, nil, "help"); res.Code != 0 {
		t.Fatalf("help exit = %d, want 0", res.Code)
	}
	if res := runCLI(t, root, nil, "nope"); res.Code != 2 {
		t.Fatalf("unknown command exit = %d, want 2", res.Code)
	}
}

// TestPathWithSpacesEndToEnd covers Windows projects under paths containing
// spaces plus a source file name containing spaces (the .cmd quoting trap).
func TestPathWithSpacesEndToEnd(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo with space")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFixtureInto(t, "typescript-repo", root)
	installTSStubs(t, root)
	spacedFile := filepath.Join(root, "src", "my file.ts")
	if err := os.WriteFile(spacedFile, []byte("export const spaced = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	res := runCLI(t, root, nil, "verify", "--all", "--json")
	if res.Code != 0 {
		t.Fatalf("verify exit = %d\nstdout:\n%s\nstderr:\n%s", res.Code, res.Stdout, res.Stderr)
	}
	if result := decodeResult(t, res.Stdout); !result.Passed {
		t.Fatalf("result = %+v", result)
	}
}
