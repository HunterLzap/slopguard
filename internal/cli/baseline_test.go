package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type fakeBaselineEngine struct {
	*fakeEngine
	doc     baselineDocument
	loadErr error
	tighten func(map[string]int) baselineDocument
	reset   func(map[string]int) baselineDocument
	saved   baselineDocument
	saveErr error
}

func (f *fakeBaselineEngine) LoadBaselineDocument(string) (baselineDocument, error) {
	if f.loadErr != nil {
		return baselineDocument{}, f.loadErr
	}
	return f.doc, nil
}

func (f *fakeBaselineEngine) TightenBaseline(_ string, current map[string]int) (baselineDocument, error) {
	if f.tighten != nil {
		return f.tighten(current), nil
	}
	return f.doc, nil
}

func (f *fakeBaselineEngine) ResetBaseline(_ string, current map[string]int) (baselineDocument, error) {
	if f.reset != nil {
		return f.reset(current), nil
	}
	return f.doc, nil
}

func (f *fakeBaselineEngine) SaveBaselineDocument(_ string, doc baselineDocument) error {
	f.saved = doc
	return f.saveErr
}

func baselineAdapter() *stubAdapter {
	return &stubAdapter{
		name:    "typescript",
		exts:    []string{".ts"},
		escapes: []contract.EscapePattern{{Name: "ts_ignore", Regex: `@ts-ignore`}},
	}
}

func TestBaselinePrint(t *testing.T) {
	root := t.TempDir()
	content := `{"version":1,"metrics":{"ts_ignore":3},"suppressed_files":[]}`
	writeFile(t, filepath.Join(root, ".slopguard", "baseline.json"), content)
	withDeps(t, &fakeEngine{}, &fakeAdapters{})

	var stdout, stderr bytes.Buffer
	if code := runBaseline([]string{"--root", root}, nil, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != content {
		t.Fatalf("stdout = %q, want baseline content", stdout.String())
	}
}

func TestBaselineMissing(t *testing.T) {
	withDeps(t, &fakeEngine{}, &fakeAdapters{})
	var stdout, stderr bytes.Buffer
	code := runBaseline([]string{"--root", t.TempDir()}, nil, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "run slopguard init first") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestBaselineMutuallyExclusive(t *testing.T) {
	withDeps(t, &fakeEngine{}, &fakeAdapters{})
	var stdout, stderr bytes.Buffer
	if code := runBaseline([]string{"--tighten", "--reset", "--root", t.TempDir()}, nil, &stdout, &stderr); code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func baselineFixture(t *testing.T) (string, *fakeBaselineEngine) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".slopguard", "baseline.json"),
		`{"version":1,"updated_at":"2026-01-01T00:00:00Z","metrics":{"ts_ignore":5},"suppressed_files":[]}`)
	eng := &fakeBaselineEngine{
		fakeEngine: &fakeEngine{listFiles: []string{"src/a.ts"}, escapes: map[string]int{"ts_ignore": 1}},
		doc: baselineDocument{
			Version:   1,
			UpdatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			Metrics:   map[string]int{"ts_ignore": 1},
		},
	}
	return root, eng
}

func TestBaselineTightenDeclineKeepsFile(t *testing.T) {
	root, eng := baselineFixture(t)
	withDependencies(t, dependencies{engine: eng, adapters: &fakeAdapters{detected: []contract.Adapter{baselineAdapter()}}})

	var stdout, stderr bytes.Buffer
	code := runBaseline([]string{"--root", root, "--tighten"}, strings.NewReader(""), &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No changes made") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if eng.saved.Version != 0 {
		t.Fatalf("SaveBaselineDocument was called on decline: %+v", eng.saved)
	}
}

func TestBaselineTightenYesWrites(t *testing.T) {
	root, eng := baselineFixture(t)
	withDependencies(t, dependencies{engine: eng, adapters: &fakeAdapters{detected: []contract.Adapter{baselineAdapter()}}})

	var stdout, stderr bytes.Buffer
	code := runBaseline([]string{"--root", root, "--tighten", "--yes"}, nil, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if eng.saved.Metrics["ts_ignore"] != 1 {
		t.Fatalf("saved metrics = %v, want ts_ignore=1", eng.saved.Metrics)
	}
	if !strings.Contains(stdout.String(), "Updated .slopguard/baseline.json") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestBaselineResetYesWrites(t *testing.T) {
	root, eng := baselineFixture(t)
	eng.reset = func(current map[string]int) baselineDocument {
		return baselineDocument{Version: 1, Metrics: current}
	}
	withDependencies(t, dependencies{engine: eng, adapters: &fakeAdapters{detected: []contract.Adapter{baselineAdapter()}}})

	var stdout, stderr bytes.Buffer
	code := runBaseline([]string{"--root", root, "--reset", "--yes"}, nil, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if eng.saved.Metrics["ts_ignore"] != 1 {
		t.Fatalf("saved metrics = %v", eng.saved.Metrics)
	}
}

func TestBaselineSaveError(t *testing.T) {
	root, eng := baselineFixture(t)
	eng.saveErr = errors.New("read-only")
	withDependencies(t, dependencies{engine: eng, adapters: &fakeAdapters{detected: []contract.Adapter{baselineAdapter()}}})

	var stdout, stderr bytes.Buffer
	code := runBaseline([]string{"--root", root, "--tighten", "--yes"}, nil, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "write .slopguard/baseline.json") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestBaselineAdminUnavailable(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".slopguard", "baseline.json"), `{"version":1,"metrics":{}}`)
	withDeps(t, &fakeEngine{}, &fakeAdapters{})
	var stdout, stderr bytes.Buffer
	code := runBaseline([]string{"--root", root, "--tighten"}, nil, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "engine does not support baseline administration") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
