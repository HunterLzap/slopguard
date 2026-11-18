package integration_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writePhase4File(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func envWithOverrides(overrides map[string]string) []string {
	keys := make(map[string]struct{}, len(overrides))
	for key := range overrides {
		keys[strings.ToUpper(key)] = struct{}{}
	}
	out := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		index := strings.Index(entry, "=")
		if index <= 0 {
			continue
		}
		if _, replaced := keys[strings.ToUpper(entry[:index])]; replaced {
			continue
		}
		out = append(out, entry)
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}

func runCLIWithInput(t *testing.T, workDir string, env map[string]string, input string, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command(slopguardBin, args...)
	cmd.Dir = workDir
	cmd.Env = envWithOverrides(env)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return cliResult{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
}

func phase4Home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".slopguard", "presets", "eslint"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".slopguard", "presets", "eslint", "rules.json"),
		[]byte(`{"no-console":"error"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func writeTSProject(t *testing.T, root string) {
	t.Helper()
	writePhase4File(t, filepath.Join(root, "package.json"), `{"name":"phase4-fixture","version":"1.0.0"}`)
	writePhase4File(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"strict":true}}`)
	writePhase4File(t, filepath.Join(root, "src", "app.ts"), "export const value = 1;\n")
}

func TestPhase4InitUsesGlobalAdapters(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writePhase4File(t, filepath.Join(root, "go.mod"), "module example.com/phase4\n")
	writePhase4File(t, filepath.Join(home, ".slopguard", "adapters", "go.yaml"),
		"name: go-standard\ndetect:\n  files_any: [\"go.mod\"]\nextensions: [\".go\"]\nchecks:\n  - id: vet\n    cmd: [\"go\", \"vet\", \"./...\"]\n")
	env := map[string]string{"HOME": home, "USERPROFILE": home}

	result := runCLIWithInput(t, root, env, "", "init", "--root", root)
	if result.Code != 0 {
		t.Fatalf("init with global adapter exit = %d\nstdout:\n%s\nstderr:\n%s", result.Code, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "Applicable adapters: go-standard") {
		t.Fatalf("global adapter not detected by init:\n%s", result.Stdout)
	}
	config, err := os.ReadFile(filepath.Join(root, ".slopguard", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "- go-standard") {
		t.Fatalf("config missing global adapter:\n%s", config)
	}
}

func TestPhase4InitPresetsIntegration(t *testing.T) {
	home := phase4Home(t)
	root := t.TempDir()
	writeTSProject(t, root)
	configPath := filepath.Join(root, "eslint.config.mjs")
	writePhase4File(t, configPath, "export default [];\n")
	env := map[string]string{"HOME": home, "USERPROFILE": home}

	result := runCLIWithInput(t, root, env, "", "init", "--presets", "--yes", "--root", root)
	if result.Code != 0 {
		t.Fatalf("init --presets --yes exit = %d\nstdout:\n%s\nstderr:\n%s", result.Code, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "Preset merge plan: 1 to write") {
		t.Fatalf("stdout missing preset plan:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "Preview diff:") {
		t.Fatalf("stdout missing preview diff:\n%s", result.Stdout)
	}
	first, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	firstText := string(first)
	if !strings.Contains(firstText, "no-console") {
		t.Fatalf("preset rule not written:\n%s", firstText)
	}
	if !strings.Contains(firstText, "files:") {
		t.Fatalf("preset merge missing files scope:\n%s", firstText)
	}
	backupPath := configPath + ".bak"
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("preset apply did not create backup %s: %v", backupPath, err)
	}

	second := runCLIWithInput(t, root, env, "", "init", "--presets", "--yes", "--root", root)
	if second.Code != 0 {
		t.Fatalf("second init exit = %d\nstderr:\n%s", second.Code, second.Stderr)
	}
	if !strings.Contains(second.Stdout, "0 to write, 1 skipped") {
		t.Fatalf("second init output not idempotent:\n%s", second.Stdout)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, after) {
		t.Fatalf("second init changed config:\nfirst:\n%s\nafter:\n%s", first, after)
	}
}

func TestPhase4InitPresetsInteractiveConfirmation(t *testing.T) {
	home := phase4Home(t)
	root := t.TempDir()
	writeTSProject(t, root)
	configPath := filepath.Join(root, "eslint.config.mjs")
	original := "export default [];\n"
	writePhase4File(t, configPath, original)
	env := map[string]string{"HOME": home, "USERPROFILE": home}

	declined := runCLIWithInput(t, root, env, "n\n", "init", "--presets", "--root", root)
	if declined.Code != 0 {
		t.Fatalf("declined init exit = %d\nstderr:\n%s", declined.Code, declined.Stderr)
	}
	if !strings.Contains(declined.Stdout, "Apply these preset changes?") {
		t.Fatalf("confirmation prompt missing:\n%s", declined.Stdout)
	}
	if !strings.Contains(declined.Stdout, "No preset changes made") {
		t.Fatalf("decline output missing:\n%s", declined.Stdout)
	}
	afterDecline, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterDecline) != original {
		t.Fatalf("declined preset changed config:\n%s", afterDecline)
	}

	accepted := runCLIWithInput(t, root, env, "y\n", "init", "--presets", "--root", root)
	if accepted.Code != 0 {
		t.Fatalf("accepted init exit = %d\nstderr:\n%s", accepted.Code, accepted.Stderr)
	}
	if !strings.Contains(accepted.Stdout, "Presets written: 1") {
		t.Fatalf("accepted output missing write count:\n%s", accepted.Stdout)
	}
	afterAccept, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(afterAccept), "no-console") {
		t.Fatalf("accepted preset not written:\n%s", afterAccept)
	}
}

func TestPhase4InitUIConfigIntegration(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	env := map[string]string{"HOME": home, "USERPROFILE": home}
	result := runCLIWithInput(t, root, env, "", "init", "--ui", "--root", root)
	if result.Code != 0 {
		t.Fatalf("init --ui exit = %d\nstdout:\n%s\nstderr:\n%s", result.Code, result.Stdout, result.Stderr)
	}
	uiPath := filepath.Join(root, ".slopguard", "ui.yaml")
	data, err := os.ReadFile(uiPath)
	if err != nil {
		t.Fatalf("ui.yaml not created: %v", err)
	}
	text := string(data)
	for _, field := range []string{"base_url:", "routes:", "screenshot_dir:", "timeout_sec:"} {
		if !strings.Contains(text, field) {
			t.Fatalf("ui.yaml missing %s:\n%s", field, text)
		}
	}
	before := append([]byte(nil), data...)
	second := runCLIWithInput(t, root, env, "", "init", "--ui", "--root", root)
	if second.Code != 0 {
		t.Fatalf("second init --ui exit = %d\nstderr:\n%s", second.Code, second.Stderr)
	}
	if !strings.Contains(second.Stdout, "Kept existing .slopguard/ui.yaml") {
		t.Fatalf("existing ui config not preserved:\n%s", second.Stdout)
	}
	after, err := os.ReadFile(uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("existing ui.yaml was overwritten:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestPhase4VerifyUISkippedWhenPlaywrightMissing(t *testing.T) {
	home := t.TempDir()
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)
	env := map[string]string{"HOME": home, "USERPROFILE": home}
	if result := runCLIWithInput(t, root, env, "", "init", "--root", root); result.Code != 0 {
		t.Fatalf("init exit = %d: %s", result.Code, result.Stderr)
	}
	writePhase4File(t, filepath.Join(root, ".slopguard", "ui.yaml"),
		"base_url: http://127.0.0.1:3999\nroutes: [\"/\"]\nscreenshot_dir: .slopguard/ui-screenshots\n")

	emptyBin := filepath.Join(t.TempDir(), "empty-bin")
	if err := os.MkdirAll(emptyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	env["PATH"] = emptyBin
	result := runCLIWithInput(t, root, env, "", "verify", "--all", "--ui", "--json", "--root", root)
	if result.Code != 0 {
		t.Fatalf("verify --ui skipped exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", result.Code, result.Stdout, result.Stderr)
	}
	verifyResult := decodeResult(t, result.Stdout)
	if verifyResult.UI == nil {
		t.Fatal("verify result missing ui object")
	}
	if !verifyResult.UI.Skipped {
		t.Fatalf("ui.skipped = false, want graceful skip (ui=%+v)", verifyResult.UI)
	}
	if !strings.Contains(verifyResult.UI.Reason, "Playwright") {
		t.Fatalf("ui.reason = %q, want Playwright hint", verifyResult.UI.Reason)
	}
	if !verifyResult.Passed {
		t.Fatalf("verify passed = false, want true when UI skipped")
	}
}

func TestPhase4VerifyUIDefaultOff(t *testing.T) {
	home := t.TempDir()
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)
	env := map[string]string{"HOME": home, "USERPROFILE": home}
	if result := runCLIWithInput(t, root, env, "", "init", "--root", root); result.Code != 0 {
		t.Fatalf("init exit = %d: %s", result.Code, result.Stderr)
	}
	writePhase4File(t, filepath.Join(root, ".slopguard", "ui.yaml"),
		"base_url: http://127.0.0.1:3999\nroutes: [\"/\"]\nscreenshot_dir: .slopguard/ui-screenshots\n")
	installStub(t, root, "playwright",
		"@echo off\necho pageerror: should-not-run\nexit /b 1\n",
		"#!/bin/sh\necho 'pageerror: should-not-run'\nexit 1\n")

	result := runCLIWithInput(t, root, env, "", "verify", "--all", "--json", "--root", root)
	if result.Code != 0 {
		t.Fatalf("verify without --ui exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", result.Code, result.Stdout, result.Stderr)
	}
	verifyResult := decodeResult(t, result.Stdout)
	if verifyResult.UI != nil {
		t.Fatalf("verify without --ui returned ui = %+v, want nil", verifyResult.UI)
	}
	if !verifyResult.Passed {
		t.Fatalf("verify without --ui passed = false")
	}
}

func TestPhase4VerifyUIFailureExitOne(t *testing.T) {
	home := t.TempDir()
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)
	env := map[string]string{"HOME": home, "USERPROFILE": home}
	if result := runCLIWithInput(t, root, env, "", "init", "--root", root); result.Code != 0 {
		t.Fatalf("init exit = %d: %s", result.Code, result.Stderr)
	}
	writePhase4File(t, filepath.Join(root, ".slopguard", "ui.yaml"),
		"base_url: http://127.0.0.1:3999\nroutes: [\"/\"]\nscreenshot_dir: .slopguard/ui-screenshots\n")
	installStub(t, root, "playwright",
		"@echo off\necho pageerror: boom\nexit /b 1\n",
		"#!/bin/sh\necho 'pageerror: boom'\nexit 1\n")

	emptyBin := filepath.Join(t.TempDir(), "empty-bin")
	if err := os.MkdirAll(emptyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	env["PATH"] = emptyBin
	result := runCLIWithInput(t, root, env, "", "verify", "--all", "--ui", "--json", "--root", root)
	if result.Code != 1 {
		t.Fatalf("verify --ui failure exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", result.Code, result.Stdout, result.Stderr)
	}
	verifyResult := decodeResult(t, result.Stdout)
	if verifyResult.Passed {
		t.Fatalf("verify passed = true on UI failure")
	}
	if verifyResult.UI == nil || verifyResult.UI.Skipped || verifyResult.UI.Passed {
		t.Fatalf("ui result = %+v, want failed non-skipped", verifyResult.UI)
	}
	if len(verifyResult.UI.Findings) == 0 || !strings.Contains(verifyResult.UI.Findings[0].Message, "pageerror: boom") {
		t.Fatalf("ui findings = %+v", verifyResult.UI.Findings)
	}
}
