package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type uiFakeRunner struct {
	lookPath func(string) (string, bool)
	run      func(context.Context, contract.RunSpec) contract.RunResult
}

func (f *uiFakeRunner) LookPath(name string) (string, bool) {
	if f.lookPath != nil {
		return f.lookPath(name)
	}
	return "", false
}

func (f *uiFakeRunner) Run(ctx context.Context, spec contract.RunSpec) contract.RunResult {
	if f.run != nil {
		return f.run(ctx, spec)
	}
	return contract.RunResult{}
}

func writeUIConfig(t *testing.T, root, content string) {
	t.Helper()
	if err := writeTestFile(filepath.Join(root, ".slopguard", "ui.yaml"), content); err != nil {
		t.Fatal(err)
	}
}

func TestRunUISmokeSkipsWithoutConfig(t *testing.T) {
	root := t.TempDir()
	called := false
	runner := &uiFakeRunner{
		lookPath: func(string) (string, bool) { return "playwright", true },
		run: func(context.Context, contract.RunSpec) contract.RunResult {
			called = true
			return contract.RunResult{}
		},
	}
	result, err := RunUISmoke(context.Background(), runner, UIOptions{ProjectRoot: root})
	if err != nil {
		t.Fatalf("RunUISmoke error: %v", err)
	}
	if !result.Skipped || !strings.Contains(result.Reason, "ui.yaml") {
		t.Fatalf("result = %+v, want skipped with ui.yaml reason", result)
	}
	if called {
		t.Fatal("runner should not be called without UI config")
	}
}

func TestRunUISmokeSkipsWithoutPlaywright(t *testing.T) {
	root := t.TempDir()
	writeUIConfig(t, root, "base_url: http://127.0.0.1:3000\nroutes: [/]\n")
	runner := &uiFakeRunner{lookPath: func(string) (string, bool) { return "", false }}
	result, err := RunUISmoke(context.Background(), runner, UIOptions{ProjectRoot: root})
	if err != nil {
		t.Fatalf("RunUISmoke error: %v", err)
	}
	if !result.Skipped || !strings.Contains(result.Reason, "Playwright") {
		t.Fatalf("result = %+v, want skipped with Playwright reason", result)
	}
}

func TestRunUISmokePassesAndReturnsScreenshots(t *testing.T) {
	root := t.TempDir()
	writeUIConfig(t, root, "base_url: http://127.0.0.1:3333\nroutes: [/home]\nscreenshot_dir: artifacts/ui\ntimeout_sec: 15\n")
	var specPath, configPath, baseURL, routes, screenshotDir string
	runner := &uiFakeRunner{
		lookPath: func(name string) (string, bool) {
			if name == "playwright" {
				return "C:/fake/playwright.exe", true
			}
			return "", false
		},
		run: func(_ context.Context, spec contract.RunSpec) contract.RunResult {
			baseURL = spec.Env["SLOPGUARD_BASE_URL"]
			routes = spec.Env["SLOPGUARD_ROUTES"]
			screenshotDir = spec.Env["SLOPGUARD_SCREENSHOT_DIR"]
			for i, arg := range spec.Args {
				if arg == "--config" && i+1 < len(spec.Args) {
					configPath = spec.Args[i+1]
				}
			}
			specPath = spec.Args[len(spec.Args)-1]
			if err := os.MkdirAll(screenshotDir, 0o755); err != nil {
				t.Fatalf("mkdir screenshot dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(screenshotDir, "home.png"), []byte("png"), 0o644); err != nil {
				t.Fatalf("write screenshot: %v", err)
			}
			if _, err := os.Stat(specPath); err != nil {
				t.Fatalf("spec not written: %v", err)
			}
			if _, err := os.Stat(configPath); err != nil {
				t.Fatalf("playwright config not written: %v", err)
			}
			return contract.RunResult{ExitCode: 0}
		},
	}
	result, err := RunUISmoke(context.Background(), runner, UIOptions{ProjectRoot: root})
	if err != nil {
		t.Fatalf("RunUISmoke error: %v", err)
	}
	if !result.Passed || result.Skipped {
		t.Fatalf("result = %+v, want passed", result)
	}
	if baseURL != "http://127.0.0.1:3333" || routes != `["/home"]` {
		t.Fatalf("env baseURL=%q routes=%q", baseURL, routes)
	}
	if len(result.Screenshots) != 1 || result.Screenshots[0] != "artifacts/ui/home.png" {
		t.Fatalf("screenshots = %v", result.Screenshots)
	}
	if _, err := os.Stat(specPath); !os.IsNotExist(err) {
		t.Fatalf("temporary spec was not cleaned up: %v", err)
	}
}

func TestRunUISmokeReportsFindingsOnFailure(t *testing.T) {
	root := t.TempDir()
	writeUIConfig(t, root, "base_url: http://127.0.0.1:3000\n")
	runner := &uiFakeRunner{
		lookPath: func(string) (string, bool) { return "playwright", true },
		run: func(context.Context, contract.RunSpec) contract.RunResult {
			return contract.RunResult{
				ExitCode: 1,
				Stdout:   "SLOPGUARD_UI_FINDING\t/\tconsole.error: boom\nError: test failed\n",
			}
		},
	}
	result, err := RunUISmoke(context.Background(), runner, UIOptions{ProjectRoot: root})
	if err != nil {
		t.Fatalf("RunUISmoke error: %v", err)
	}
	if result.Passed || result.Skipped {
		t.Fatalf("result = %+v, want failed", result)
	}
	if len(result.Findings) == 0 || !strings.Contains(result.Findings[0].Message, "console.error: boom") || result.Findings[0].File != "/" {
		t.Fatalf("findings = %+v", result.Findings)
	}
	if !strings.Contains(result.Reason, "失败") {
		t.Fatalf("reason = %q", result.Reason)
	}
}

func TestRunUISmokeRejectsUnknownConfigField(t *testing.T) {
	root := t.TempDir()
	writeUIConfig(t, root, "base_url: http://127.0.0.1:3000\nunknown: true\n")
	runner := &uiFakeRunner{lookPath: func(string) (string, bool) { return "playwright", true }}
	_, err := RunUISmoke(context.Background(), runner, UIOptions{ProjectRoot: root})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("error = %v, want unknown field", err)
	}
}
