package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type fakeAdapter struct {
	name   string
	detect bool
	exts   []string
	checks []contract.Check
	escape []contract.EscapePattern
}

func (f *fakeAdapter) Name() string                                  { return f.name }
func (f *fakeAdapter) Detect(string) bool                            { return f.detect }
func (f *fakeAdapter) Extensions() []string                          { return f.exts }
func (f *fakeAdapter) Checks(string, []string) []contract.Check      { return f.checks }
func (f *fakeAdapter) EscapePatterns() []contract.EscapePattern      { return f.escape }
func (f *fakeAdapter) ParseOutput(string, string) []contract.Finding { return nil }

type stubRunner struct {
	lookFn func(string) (string, bool)
}

func (s *stubRunner) Run(context.Context, contract.RunSpec) contract.RunResult {
	return contract.RunResult{}
}
func (s *stubRunner) LookPath(name string) (string, bool) {
	if s.lookFn != nil {
		return s.lookFn(name)
	}
	return name, true
}

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	a := &fakeAdapter{name: "alpha"}
	if err := r.Register(a); err != nil {
		t.Fatalf("Register error: %v", err)
	}
	got, ok := r.Get(" alpha ")
	if !ok || got != a {
		t.Fatalf("Get = %v, %v", got, ok)
	}
	if err := r.Register(a); !errors.Is(err, ErrDuplicateAdapter) {
		t.Fatalf("duplicate error = %v, want ErrDuplicateAdapter", err)
	}
	if err := r.Register(nil); err == nil {
		t.Fatal("nil adapter should fail")
	}
	if err := r.Register(&fakeAdapter{name: "  "}); err == nil {
		t.Fatal("empty name should fail")
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get(missing) = true")
	}
}

func TestRegistryRegisterAllAtomic(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeAdapter{name: "one"}); err != nil {
		t.Fatal(err)
	}
	// second adapter duplicates an existing registry name: neither new adapter
	// should be registered.
	err := r.RegisterAll(&fakeAdapter{name: "two"}, &fakeAdapter{name: "one"})
	if !errors.Is(err, ErrDuplicateAdapter) {
		t.Fatalf("error = %v", err)
	}
	if _, ok := r.Get("two"); ok {
		t.Fatal("atomic RegisterAll partially registered 'two'")
	}
	if got := len(r.All()); got != 1 {
		t.Fatalf("len(All) = %d, want 1", got)
	}

	// duplicate inside the same batch also rejected.
	err = r.RegisterAll(&fakeAdapter{name: "three"}, &fakeAdapter{name: "three"})
	if !errors.Is(err, ErrDuplicateAdapter) {
		t.Fatalf("batch duplicate error = %v", err)
	}
}

func TestRegistryAllSortedAndDetect(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"zeta", "alpha", "middle"} {
		if err := r.Register(&fakeAdapter{name: name, detect: name != "zeta"}); err != nil {
			t.Fatal(err)
		}
	}
	all := r.All()
	if len(all) != 3 || all[0].Name() != "alpha" || all[1].Name() != "middle" || all[2].Name() != "zeta" {
		t.Fatalf("All order = %v", []string{all[0].Name(), all[1].Name(), all[2].Name()})
	}
	detected := r.Detect(".")
	if len(detected) != 2 || detected[0].Name() != "alpha" || detected[1].Name() != "middle" {
		t.Fatalf("Detect = %v", detected)
	}
}

func TestNewDefaultRegistry(t *testing.T) {
	r, err := NewDefaultRegistry()
	if err != nil {
		t.Fatalf("NewDefaultRegistry error: %v", err)
	}
	if _, ok := r.Get(TypeScriptAdapterName); !ok {
		t.Fatal("typescript adapter missing")
	}
	if _, ok := r.Get(PythonAdapterName); !ok {
		t.Fatal("python adapter missing")
	}
}

func TestPlanChecksSkipsOptionalAndResolvesRequired(t *testing.T) {
	runner := &stubRunner{lookFn: func(name string) (string, bool) {
		switch name {
		case "tsc":
			return "C:/tools/tsc.exe", true
		case "eslint":
			return "", false
		default:
			return "", false
		}
	}}
	checks := []contract.Check{
		{ID: "tsc", Cmd: []string{"tsc", "--noEmit"}},
		{ID: "eslint", Cmd: []string{"eslint", "."}, Optional: true},
	}
	plan, err := PlanChecks(runner, "", checks)
	if err != nil {
		t.Fatalf("PlanChecks error: %v", err)
	}
	if len(plan.Runnable) != 1 || plan.Runnable[0].Cmd[0] != "C:/tools/tsc.exe" {
		t.Fatalf("runnable = %+v", plan.Runnable)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0].ID != "eslint" || plan.Skipped[0].Reason == "" {
		t.Fatalf("skipped = %+v", plan.Skipped)
	}
}

func TestPlanChecksMissingRequired(t *testing.T) {
	runner := &stubRunner{lookFn: func(string) (string, bool) { return "", false }}
	_, err := PlanChecks(runner, "", []contract.Check{{ID: "eslint", Cmd: []string{"eslint"}}})
	if err == nil {
		t.Fatal("expected missing required tool error")
	}
	if want := "npm install -D eslint"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want install hint %q", err, want)
	}
}

func TestPlanChecksLocalProjectTool(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "node_modules", ".bin", "eslint")
	if err := writeTestFile(tool, "#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatal(err)
	}
	runner := &stubRunner{lookFn: func(string) (string, bool) { return "", false }}
	plan, err := PlanChecks(runner, root, []contract.Check{{ID: "eslint", Cmd: []string{"eslint", "."}}})
	if err != nil {
		t.Fatalf("PlanChecks error: %v", err)
	}
	if len(plan.Runnable) != 1 || plan.Runnable[0].Cmd[0] != tool {
		t.Fatalf("runnable = %+v, want local tool %s", plan.Runnable, tool)
	}
}

func TestPlanChecksInvalid(t *testing.T) {
	if _, err := PlanChecks(nil, "", nil); err == nil {
		t.Fatal("nil runner should error")
	}
	_, err := PlanChecks(&stubRunner{}, "", []contract.Check{{ID: "empty"}})
	if err == nil || !strings.Contains(err.Error(), "empty command") {
		t.Fatalf("err = %v", err)
	}
}

func TestService(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeAdapter{name: "x", detect: true}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(r, &stubRunner{})
	detected, err := svc.Detect(".")
	if err != nil || len(detected) != 1 {
		t.Fatalf("Detect = %v, %v", detected, err)
	}
	if svc.Registry() != r {
		t.Fatal("Registry() mismatch")
	}
	var nilSvc *Service
	if _, err := nilSvc.Detect("."); err == nil {
		t.Fatal("nil service Detect should error")
	}
}

func writeTestFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o755)
}
