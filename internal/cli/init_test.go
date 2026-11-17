package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func initAdapter() *stubAdapter {
	return &stubAdapter{
		name:    "typescript",
		exts:    []string{".ts"},
		escapes: []contract.EscapePattern{{Name: "ts_ignore", Regex: `@ts-ignore`}},
	}
}

func TestInitCreatesConfigAndBaseline(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), "{}")
	eng := &fakeEngine{
		listFiles: []string{"src/a.ts"},
		escapes:   map[string]int{"ts_ignore": 2},
	}
	adp := &fakeAdapters{detected: []contract.Adapter{initAdapter()}}
	withDeps(t, eng, adp)

	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--root", root}, strings.NewReader(""), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	config, err := os.ReadFile(filepath.Join(root, ".slopguard", "config.yaml"))
	if err != nil {
		t.Fatalf("config not created: %v", err)
	}
	text := string(config)
	if !strings.Contains(text, "version: 1") || !strings.Contains(text, "- typescript") || !strings.Contains(text, "jobs: 2") {
		t.Fatalf("config content:\n%s", text)
	}
	if eng.savedMetrics["ts_ignore"] != 2 {
		t.Fatalf("saved metrics = %v", eng.savedMetrics)
	}
	if !strings.HasSuffix(eng.savedPath, filepath.Join(".slopguard", "baseline.json")) {
		t.Fatalf("saved path = %q", eng.savedPath)
	}
	if !strings.Contains(stdout.String(), "Created .slopguard/config.yaml") || !strings.Contains(stdout.String(), "init complete.") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
}

func TestInitExistingConfigDeclineKeepsFile(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".slopguard", "config.yaml")
	original := "version: 1\nadapters: []\nverify:\n  jobs: 9\n"
	writeFile(t, configPath, original)
	eng := &fakeEngine{}
	adp := &fakeAdapters{}
	withDeps(t, eng, adp)

	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--root", root}, strings.NewReader(""), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("config changed without confirmation:\n%s", after)
	}
	if !strings.Contains(stdout.String(), "--- a/.slopguard/config.yaml") {
		t.Fatalf("diff not printed:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "No changes made") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
}

func TestInitExistingConfigConfirmApplies(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".slopguard", "config.yaml")
	writeFile(t, configPath, "version: 1\nadapters: []\nverify:\n  jobs: 9\n")
	eng := &fakeEngine{}
	withDeps(t, eng, &fakeAdapters{})

	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--root", root}, strings.NewReader("y\n"), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "# SlopGuard project configuration") || !strings.Contains(string(after), "jobs: 2") {
		t.Fatalf("config not updated:\n%s", after)
	}
	if !strings.Contains(stdout.String(), "Updated .slopguard/config.yaml") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
}

func TestInitYesFlagAppliesWithoutPrompt(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".slopguard", "config.yaml")
	writeFile(t, configPath, "version: 1\nadapters: []\nverify:\n  jobs: 9\n")
	withDeps(t, &fakeEngine{}, &fakeAdapters{})

	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--root", root, "--yes"}, nil, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	after, _ := os.ReadFile(configPath)
	if !strings.Contains(string(after), "jobs: 2") {
		t.Fatalf("config not updated:\n%s", after)
	}
	if strings.Contains(stdout.String(), "[y/N]") {
		t.Fatalf("prompt should not appear with --yes:\n%s", stdout.String())
	}
}

func TestInitKeepsValidBaseline(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".slopguard", "baseline.json"), `{"version":1,"metrics":{"ts_ignore":0},"suppressed_files":[]}`)
	eng := &fakeEngine{baseline: map[string]int{"ts_ignore": 0}}
	withDeps(t, eng, &fakeAdapters{})

	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--root", root}, nil, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Kept existing .slopguard/baseline.json") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
	if eng.savedMetrics != nil {
		t.Fatal("existing baseline should not be rewritten")
	}
}

func TestInitInvalidBaseline(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".slopguard", "baseline.json"), `{broken`)
	eng := &fakeEngine{baselineErr: errors.New("invalid json")}
	withDeps(t, eng, &fakeAdapters{})

	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--root", root}, nil, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "existing .slopguard/baseline.json is invalid") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestInitNoAdapters(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module x")
	eng := &fakeEngine{}
	withDeps(t, eng, &fakeAdapters{})

	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--root", root}, nil, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if len(eng.savedMetrics) != 0 {
		t.Fatalf("saved metrics = %v, want empty", eng.savedMetrics)
	}
	if !strings.Contains(stdout.String(), "Applicable adapters: none") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
}

func TestInitBadRoot(t *testing.T) {
	withDeps(t, &fakeEngine{}, &fakeAdapters{})
	var stdout, stderr bytes.Buffer
	code := runInit([]string{"--root", filepath.Join(t.TempDir(), "missing")}, nil, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want %d", code, ExitError)
	}
}

func TestInitExtraArgs(t *testing.T) {
	withDeps(t, &fakeEngine{}, &fakeAdapters{})
	var stdout, stderr bytes.Buffer
	code := runInit([]string{"unexpected"}, nil, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want %d", code, ExitError)
	}
}
