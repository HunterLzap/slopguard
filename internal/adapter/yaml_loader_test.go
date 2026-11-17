package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
name: go-standard
detect:
  files_any: ["go.mod"]
extensions: [".go"]
checks:
  - id: vet
    cmd: ["go", "vet", "./..."]
    timeout_sec: 30
  - id: lint
    cmd: ["golangci-lint", "run"]
    optional: true
    timeout_sec: 60
escape_patterns:
  - name: nolint
    regex: "//nolint"
`

func TestLoadYAMLValid(t *testing.T) {
	adapters, err := LoadYAML([]byte(validYAML), "test.yaml")
	if err != nil {
		t.Fatalf("LoadYAML error: %v", err)
	}
	if len(adapters) != 1 {
		t.Fatalf("len(adapters) = %d", len(adapters))
	}
	a := adapters[0]
	if a.Name() != "go-standard" {
		t.Fatalf("Name = %q", a.Name())
	}
	if len(a.Extensions()) != 1 || a.Extensions()[0] != ".go" {
		t.Fatalf("Extensions = %v", a.Extensions())
	}
	checks := a.Checks("", nil)
	if len(checks) != 2 {
		t.Fatalf("checks = %+v", checks)
	}
	if checks[0].ID != "vet" || checks[0].Timeout != 30*time.Second || checks[0].Optional {
		t.Fatalf("checks[0] = %+v", checks[0])
	}
	if checks[1].ID != "lint" || !checks[1].Optional || checks[1].Timeout != 60*time.Second {
		t.Fatalf("checks[1] = %+v", checks[1])
	}
	escapes := a.EscapePatterns()
	if len(escapes) != 1 || escapes[0].Name != "nolint" || escapes[0].Regex != "//nolint" {
		t.Fatalf("escapes = %+v", escapes)
	}
}

func TestYAMLAdapterDetect(t *testing.T) {
	adapters, err := LoadYAML([]byte(validYAML), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	a := adapters[0]
	if a.Detect(t.TempDir()) {
		t.Fatal("empty dir should not match")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !a.Detect(dir) {
		t.Fatal("go.mod should match detect.files_any")
	}
}

func TestYAMLAdapterChecksFilePlaceholder(t *testing.T) {
	src := `
name: probe
detect:
  files_any: ["probe.txt"]
extensions: [".txt"]
checks:
  - id: custom
    cmd: ["tool", "--files", "{files}", "--done"]
`
	adapters, err := LoadYAML([]byte(src), "probe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	checks := adapters[0].Checks("", []string{"a.txt", "b.txt"})
	want := []string{"tool", "--files", "a.txt", "b.txt", "--done"}
	if len(checks) != 1 || len(checks[0].Cmd) != len(want) {
		t.Fatalf("checks = %+v", checks)
	}
	for i := range want {
		if checks[0].Cmd[i] != want[i] {
			t.Fatalf("cmd = %v, want %v", checks[0].Cmd, want)
		}
	}
}

func TestLoadYAMLStrictSchema(t *testing.T) {
	cases := map[string]string{
		"unknown field": `
name: x
detect:
  files_any: ["a"]
extensions: [".a"]
checks:
  - id: c
    cmd: ["t"]
unknown: true
`,
		"missing name": `
detect:
  files_any: ["a"]
extensions: [".a"]
checks:
  - id: c
    cmd: ["t"]
`,
		"missing detect": `
name: x
extensions: [".a"]
checks:
  - id: c
    cmd: ["t"]
`,
		"missing extensions": `
name: x
detect:
  files_any: ["a"]
checks:
  - id: c
    cmd: ["t"]
`,
		"missing checks": `
name: x
detect:
  files_any: ["a"]
extensions: [".a"]
`,
		"bad extension": `
name: x
detect:
  files_any: ["a"]
extensions: ["a"]
checks:
  - id: c
    cmd: ["t"]
`,
		"bad regex": `
name: x
detect:
  files_any: ["a"]
extensions: [".a"]
checks:
  - id: c
    cmd: ["t"]
escape_patterns:
  - name: e
    regex: "["
`,
		"duplicate check id": `
name: x
detect:
  files_any: ["a"]
extensions: [".a"]
checks:
  - id: c
    cmd: ["t"]
  - id: c
    cmd: ["u"]
`,
		"duplicate escape": `
name: x
detect:
  files_any: ["a"]
extensions: [".a"]
checks:
  - id: c
    cmd: ["t"]
escape_patterns:
  - name: e
    regex: "x"
  - name: e
    regex: "y"
`,
		"parent dir pattern": `
name: x
detect:
  files_any: ["../evil"]
extensions: [".a"]
checks:
  - id: c
    cmd: ["t"]
`,
		"empty cmd": `
name: x
detect:
  files_any: ["a"]
extensions: [".a"]
checks:
  - id: c
    cmd: []
`,
		"timeout too large": `
name: x
detect:
  files_any: ["a"]
extensions: [".a"]
checks:
  - id: c
    cmd: ["t"]
    timeout_sec: 4000
`,
	}
	for name, src := range cases {
		if _, err := LoadYAML([]byte(src), name+".yaml"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadYAMLMultiDocument(t *testing.T) {
	src := validYAML + "\n---\n" + strings.ReplaceAll(validYAML, "go-standard", "go-second")
	adapters, err := LoadYAML([]byte(src), "multi.yaml")
	if err != nil {
		t.Fatalf("LoadYAML error: %v", err)
	}
	if len(adapters) != 2 || adapters[0].Name() != "go-standard" || adapters[1].Name() != "go-second" {
		t.Fatalf("adapters = %+v", adapters)
	}
}

func TestLoadYAMLNoDefinitions(t *testing.T) {
	if _, err := LoadYAML([]byte(""), "empty.yaml"); err == nil || !strings.Contains(err.Error(), "no adapter definitions") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadYAMLFileAndDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(strings.ReplaceAll(validYAML, "go-standard", "b-adapter")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.yml"), []byte(strings.ReplaceAll(validYAML, "go-standard", "a-adapter")), 0o644); err != nil {
		t.Fatal(err)
	}
	adapters, err := LoadYAMLDir(dir)
	if err != nil {
		t.Fatalf("LoadYAMLDir error: %v", err)
	}
	if len(adapters) != 2 || adapters[0].Name() != "a-adapter" || adapters[1].Name() != "b-adapter" {
		t.Fatalf("adapters = %+v", adapters)
	}
	if got, err := LoadYAMLDir(filepath.Join(dir, "missing")); err != nil || got != nil {
		t.Fatalf("missing dir = %v, %v", got, err)
	}
}

func TestLoadYAMLDirDuplicateName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadYAMLDir(dir); err == nil || !strings.Contains(err.Error(), "duplicate adapter name") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadYAMLFileErrors(t *testing.T) {
	if _, err := LoadYAMLFile(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing file should error")
	}
	if _, err := LoadYAMLFile(t.TempDir()); err == nil {
		t.Fatal("directory should error")
	}
	big := filepath.Join(t.TempDir(), "big.yaml")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", maxYAMLFileSize+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadYAMLFile(big); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
}

func TestRegisterYAMLFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	if err := r.RegisterYAMLFile(path); err != nil {
		t.Fatalf("RegisterYAMLFile error: %v", err)
	}
	if _, ok := r.Get("go-standard"); !ok {
		t.Fatal("yaml adapter not registered")
	}
}
