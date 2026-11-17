package adapter

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestPythonAdapterDetect(t *testing.T) {
	adapter := NewPythonAdapter()
	if adapter.Detect(t.TempDir()) {
		t.Fatal("empty directory detected as Python project")
	}
	for _, marker := range []string{"pyproject.toml", "setup.py", "requirements.txt", "Pipfile", "uv.lock"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, marker), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !adapter.Detect(dir) {
			t.Fatalf("%s not detected", marker)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "script.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !adapter.Detect(dir) {
		t.Fatal("root .py file not detected")
	}
}

func TestPythonAdapterExtensions(t *testing.T) {
	exts := NewPythonAdapter().Extensions()
	if len(exts) != 2 || exts[0] != ".py" || exts[1] != ".pyi" {
		t.Fatalf("extensions = %v", exts)
	}
}

func TestPythonChecks(t *testing.T) {
	root := t.TempDir()
	checks := NewPythonAdapter().Checks(root, []string{"app/main.py", "app/model.pyi", "README.md"})
	if len(checks) != 3 {
		t.Fatalf("len(checks) = %d, want 3", len(checks))
	}
	if checks[0].ID != "ruff" || checks[1].ID != "pyright" || checks[2].ID != "semgrep" {
		t.Fatalf("check ids = %s/%s/%s", checks[0].ID, checks[1].ID, checks[2].ID)
	}
	if !checks[2].Optional {
		t.Fatal("semgrep should be optional")
	}
	for _, check := range checks {
		if !containsArg(check.Cmd, "app/main.py") || !containsArg(check.Cmd, "app/model.pyi") {
			t.Fatalf("%s missing files: %v", check.ID, check.Cmd)
		}
		if containsArg(check.Cmd, "README.md") {
			t.Fatalf("%s includes non-python file: %v", check.ID, check.Cmd)
		}
	}
}

func TestPythonChecksNoFilesUsesDot(t *testing.T) {
	checks := NewPythonAdapter().Checks(t.TempDir(), nil)
	for _, check := range checks {
		if !containsArg(check.Cmd, ".") {
			t.Fatalf("%s cmd = %v, want dot target", check.ID, check.Cmd)
		}
	}
}

func TestPythonEscapePatterns(t *testing.T) {
	patterns := NewPythonAdapter().EscapePatterns()
	want := map[string]string{
		"type_ignore_py": "# type: ignore",
		"noqa":           "# noqa",
		"pyright_ignore": "# pyright: ignore",
	}
	if len(patterns) != len(want) {
		t.Fatalf("patterns = %+v", patterns)
	}
	for _, p := range patterns {
		sample, ok := want[p.Name]
		if !ok {
			t.Fatalf("unexpected pattern %q", p.Name)
		}
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			t.Fatalf("%s invalid: %v", p.Name, err)
		}
		if !re.MatchString(sample) {
			t.Fatalf("%s does not match %q", p.Name, sample)
		}
	}
}
