package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeQAAdapterYAML(t *testing.T, path, name, extension string) {
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

// TestQAProjectAdapterWinsByNameAcrossFilenames verifies that the project-level
// adapter wins by adapter name, not merely because two files share a filename.
func TestQAProjectAdapterWinsByNameAcrossFilenames(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	writeQAAdapterYAML(t, filepath.Join(home, ".slopguard", "adapters", "global-custom.yaml"), "qa-shared", ".global")
	writeQAAdapterYAML(t, filepath.Join(project, ".slopguard", "adapters", "project-custom.yaml"), "qa-shared", ".project")

	registry, err := LoadMergedRegistry(project)
	if err != nil {
		t.Fatalf("LoadMergedRegistry: %v", err)
	}
	shared, ok := registry.Get("qa-shared")
	if !ok {
		t.Fatal("qa-shared adapter missing")
	}
	exts := shared.Extensions()
	if len(exts) != 1 || exts[0] != ".project" {
		t.Fatalf("qa-shared extensions = %v, want project override [.project]", exts)
	}
}

func TestQAGlobalAdapterParticipatesInDetect(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module example.com/qa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeQAAdapterYAML(t, filepath.Join(home, ".slopguard", "adapters", "go-custom.yaml"), "qa-go", ".go")

	registry, err := LoadMergedRegistry(project)
	if err != nil {
		t.Fatalf("LoadMergedRegistry: %v", err)
	}
	var found bool
	for _, current := range registry.Detect(project) {
		if current.Name() == "qa-go" {
			found = true
			if exts := current.Extensions(); len(exts) != 1 || exts[0] != ".go" {
				t.Fatalf("qa-go extensions = %v", exts)
			}
		}
	}
	if !found {
		t.Fatal("global qa-go adapter was not detected for project with go.mod")
	}
}

func TestQAMergedRegistryLoadsBothDirectoriesSorted(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	writeQAAdapterYAML(t, filepath.Join(home, ".slopguard", "adapters", "a.yaml"), "qa-global", ".global")
	writeQAAdapterYAML(t, filepath.Join(project, ".slopguard", "adapters", "z.yaml"), "qa-project", ".project")

	registry, err := LoadMergedRegistry(project)
	if err != nil {
		t.Fatalf("LoadMergedRegistry: %v", err)
	}
	names := make([]string, 0)
	for _, current := range registry.All() {
		names = append(names, current.Name())
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "qa-global") || !strings.Contains(joined, "qa-project") {
		t.Fatalf("merged registry names = %v", names)
	}
}
