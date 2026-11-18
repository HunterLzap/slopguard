package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAdapterYAML(t *testing.T, path, name, extension string) {
	t.Helper()
	content := "name: " + name + "\n" +
		"detect:\n" +
		"  files_any: [\"go.mod\"]\n" +
		"extensions: [\"" + extension + "\"]\n" +
		"checks:\n" +
		"  - id: check-" + name + "\n" +
		"    cmd: [\"echo\", \"ok\"]\n"
	if err := writeTestFile(path, content); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMergedRegistryProjectOverridesGlobal(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	globalDir := filepath.Join(home, ".slopguard", "adapters")
	projectDir := filepath.Join(project, ".slopguard", "adapters")
	writeAdapterYAML(t, filepath.Join(globalDir, "shared.yaml"), "shared-adapter", ".global")
	writeAdapterYAML(t, filepath.Join(globalDir, "global.yaml"), "global-only", ".global")
	writeAdapterYAML(t, filepath.Join(projectDir, "shared.yaml"), "shared-adapter", ".project")
	writeAdapterYAML(t, filepath.Join(projectDir, "project.yaml"), "project-only", ".project")

	registry, err := LoadMergedRegistry(project)
	if err != nil {
		t.Fatalf("LoadMergedRegistry error: %v", err)
	}
	if _, ok := registry.Get(TypeScriptAdapterName); !ok {
		t.Fatal("built-in TypeScript adapter missing")
	}
	if _, ok := registry.Get(PythonAdapterName); !ok {
		t.Fatal("built-in Python adapter missing")
	}
	shared, ok := registry.Get("shared-adapter")
	if !ok {
		t.Fatal("shared adapter missing")
	}
	if got := shared.Extensions(); len(got) != 1 || got[0] != ".project" {
		t.Fatalf("shared extensions = %v, want project override [.project]", got)
	}
	for _, name := range []string{"global-only", "project-only"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("merged adapter %q missing", name)
		}
	}
}

func TestLoadMergedRegistryMissingDirsAndStrictSchema(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	registry, err := LoadMergedRegistry(project)
	if err != nil {
		t.Fatalf("missing dirs should load built-ins: %v", err)
	}
	if len(registry.All()) < 2 {
		t.Fatalf("registry adapters = %d, want built-ins", len(registry.All()))
	}

	globalDir := filepath.Join(home, ".slopguard", "adapters")
	if err := writeTestFile(filepath.Join(globalDir, "bad.yaml"), "name: bad\nunknown_field: true\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMergedRegistry(project); err == nil || !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("strict schema error = %v, want unknown_field", err)
	}
}

func TestLoadMergedRegistryRejectsBuiltinConflict(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	globalDir := filepath.Join(home, ".slopguard", "adapters")
	writeAdapterYAML(t, filepath.Join(globalDir, "typescript.yaml"), TypeScriptAdapterName, ".ts")
	if _, err := LoadMergedRegistry(project); err == nil || !strings.Contains(err.Error(), "conflicts with a built-in") {
		t.Fatalf("built-in conflict error = %v", err)
	}
}

func TestProjectAdaptersDir(t *testing.T) {
	got := ProjectAdaptersDir(filepath.Join("root", "sub"))
	want := filepath.Join("root", "sub", ".slopguard", "adapters")
	if got != want {
		t.Fatalf("ProjectAdaptersDir = %q, want %q", got, want)
	}
}

func TestGlobalAdaptersDirUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	got, err := GlobalAdaptersDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".slopguard", "adapters"); got != want {
		t.Fatalf("GlobalAdaptersDir = %q, want %q", got, want)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("global adapter dir unexpectedly exists: %v", err)
	}
}
