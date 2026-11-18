package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRuleTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pathInside(path, root string) bool {
	root = strings.TrimRight(strings.ToLower(filepath.Clean(root)), string(filepath.Separator))
	path = strings.ToLower(filepath.Clean(path))
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func TestGenerateTempConfigESLintLegacyAndCleanup(t *testing.T) {
	root := t.TempDir()
	original := "{\n  \"rules\": {\n    \"semi\": [\"error\", \"always\"]\n  }\n}\n"
	writeRuleTestFile(t, filepath.Join(root, ".eslintrc.json"), original)

	path, cleanup, err := GenerateTempConfig("eslint", "no-console", []byte(`"error"`), root)
	if err != nil {
		t.Fatalf("GenerateTempConfig: %v", err)
	}
	if pathInside(path, root) {
		t.Fatalf("temp config %s must not live inside project %s", path, root)
	}
	if filepath.Base(path) != ".eslintrc.json" {
		t.Fatalf("temp config = %s, want legacy .eslintrc.json", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp config: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("temp config is not JSON: %v\n%s", err, data)
	}
	rules, _ := parsed["rules"].(map[string]any)
	if rules["no-console"] != "error" || rules["semi"] == nil {
		t.Fatalf("temp rules = %v, want injected no-console and preserved semi", rules)
	}
	if got, err := os.ReadFile(filepath.Join(root, ".eslintrc.json")); err != nil || string(got) != original {
		t.Fatalf("project config mutated: err=%v\n%s", err, got)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove temp config: %v", err)
	}
}

func TestGenerateTempConfigESLintFlat(t *testing.T) {
	root := t.TempDir()
	writeRuleTestFile(t, filepath.Join(root, "eslint.config.mjs"), "export default [];\n")
	path, cleanup, err := GenerateTempConfig("eslint", "no-console", []byte(`"error"`), root)
	if err != nil {
		t.Fatalf("GenerateTempConfig: %v", err)
	}
	defer cleanup()
	if filepath.Base(path) != "eslint.config.mjs" {
		t.Fatalf("temp config = %s, want eslint.config.mjs", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "file://") || !strings.Contains(content, "no-console") {
		t.Fatalf("flat temp config missing base import or rule:\n%s", content)
	}
	if !strings.Contains(content, "files:") || !strings.Contains(content, "basePath:") {
		t.Fatalf("flat temp config missing files/basePath scope:\n%s", content)
	}
	for _, ext := range []string{"**/*.js", "**/*.ts", "**/*.tsx"} {
		if !strings.Contains(content, ext) {
			t.Fatalf("flat temp config missing %s:\n%s", ext, content)
		}
	}
}

func TestGenerateTempConfigESLintFlatWithoutProjectConfig(t *testing.T) {
	root := t.TempDir()
	path, cleanup, err := GenerateTempConfig("eslint", "no-console", []byte(`"error"`), root)
	if err != nil {
		t.Fatalf("GenerateTempConfig: %v", err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "files:") || !strings.Contains(content, "basePath:") || !strings.Contains(content, "no-console") {
		t.Fatalf("default flat temp config missing scope or rule:\n%s", content)
	}
}

func TestGenerateTempConfigRuffAndSemgrep(t *testing.T) {
	t.Run("ruff", func(t *testing.T) {
		root := t.TempDir()
		path, cleanup, err := GenerateTempConfig("ruff", "E501", []byte("[lint]\nline-length = 100\n"), root)
		if err != nil {
			t.Fatalf("GenerateTempConfig: %v", err)
		}
		defer cleanup()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if !strings.Contains(content, `select = ["E501"]`) || !strings.Contains(content, "line-length = 100") {
			t.Fatalf("ruff temp config:\n%s", content)
		}
	})

	t.Run("semgrep", func(t *testing.T) {
		root := t.TempDir()
		payload := "id: no-print\nlanguages: [python]\nmessage: no print\nseverity: WARNING\npattern: print(...)\n"
		path, cleanup, err := GenerateTempConfig("semgrep", "no-print", []byte(payload), root)
		if err != nil {
			t.Fatalf("GenerateTempConfig: %v", err)
		}
		defer cleanup()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "id: no-print") {
			t.Fatalf("semgrep temp config:\n%s", data)
		}
	})

	t.Run("invalid engine", func(t *testing.T) {
		if _, _, err := GenerateTempConfig("golangci", "x", []byte("y"), t.TempDir()); err == nil {
			t.Fatal("GenerateTempConfig invalid engine = nil, want error")
		}
	})
}

func TestApplyRuleESLintBackupAndDiff(t *testing.T) {
	root := t.TempDir()
	original := "{\n  \"rules\": {\n    \"semi\": [\"error\", \"always\"]\n  }\n}\n"
	configPath := filepath.Join(root, ".eslintrc.json")
	writeRuleTestFile(t, configPath, original)

	result, err := ApplyRule("eslint", "no-console", []byte(`"error"`), root)
	if err != nil {
		t.Fatalf("ApplyRule: %v", err)
	}
	if result.ConfigPath != configPath {
		t.Fatalf("ConfigPath = %s, want %s", result.ConfigPath, configPath)
	}
	if result.BackupPath == "" {
		t.Fatal("BackupPath is empty, want backup of existing config")
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil || string(backup) != original {
		t.Fatalf("backup content = %q, err=%v", backup, err)
	}
	updated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(updated, &parsed); err != nil {
		t.Fatalf("updated config is not JSON: %v", err)
	}
	rules, _ := parsed["rules"].(map[string]any)
	if rules["no-console"] != "error" || rules["semi"] == nil {
		t.Fatalf("updated rules = %v", rules)
	}
	if !strings.Contains(result.Diff, "--- a/.eslintrc.json") || !strings.Contains(result.Diff, "+++ b/.eslintrc.json") {
		t.Fatalf("diff headers missing:\n%s", result.Diff)
	}
	if !strings.Contains(result.Diff, "no-console") {
		t.Fatalf("diff does not mention rule:\n%s", result.Diff)
	}
}

func TestApplyRuleMergesExistingFlatConfig(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "eslint.config.mjs")
	writeRuleTestFile(t, configPath, "export default [];\n")
	result, err := ApplyRule("eslint", "no-console", []byte(`"error"`), root)
	if err != nil {
		t.Fatalf("ApplyRule: %v", err)
	}
	if result.BackupPath == "" {
		t.Fatal("BackupPath is empty for existing flat config")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "no-console") {
		t.Fatalf("merged config missing rule:\n%s", content)
	}
	if !strings.Contains(content, "files:") || !strings.Contains(content, "**/*.ts") {
		t.Fatalf("merged flat config injection missing files matching:\n%s", content)
	}
}

func TestApplyRuleCreatesESLintConfig(t *testing.T) {
	root := t.TempDir()
	result, err := ApplyRule("eslint", "no-console", []byte(`"error"`), root)
	if err != nil {
		t.Fatalf("ApplyRule: %v", err)
	}
	if result.BackupPath != "" {
		t.Fatalf("BackupPath = %q, want empty for new config", result.BackupPath)
	}
	data, err := os.ReadFile(filepath.Join(root, "eslint.config.mjs"))
	if err != nil {
		t.Fatalf("read created config: %v", err)
	}
	if !strings.Contains(string(data), "no-console") {
		t.Fatalf("created config missing rule:\n%s", data)
	}
}

func TestApplyRuleRuffAndSemgrep(t *testing.T) {
	t.Run("ruff", func(t *testing.T) {
		root := t.TempDir()
		result, err := ApplyRule("ruff", "E501", []byte("[lint]\nline-length = 100\n"), root)
		if err != nil {
			t.Fatalf("ApplyRule: %v", err)
		}
		data, err := os.ReadFile(result.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `select = ["E501"]`) {
			t.Fatalf("ruff config:\n%s", data)
		}
	})

	t.Run("semgrep", func(t *testing.T) {
		root := t.TempDir()
		payload := "id: no-print\nlanguages: [python]\nmessage: no print\nseverity: WARNING\npattern: print(...)\n"
		result, err := ApplyRule("semgrep", "no-print", []byte(payload), root)
		if err != nil {
			t.Fatalf("ApplyRule: %v", err)
		}
		data, err := os.ReadFile(result.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "no-print") {
			t.Fatalf("semgrep config:\n%s", data)
		}
	})
}

func TestGlobalRuleTemplateMergeAndRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	path, err := MergeGlobalRuleTemplate("eslint", "no-console", []byte(`"error"`))
	if err != nil {
		t.Fatalf("MergeGlobalRuleTemplate: %v", err)
	}
	expected := filepath.Join(home, ".slopguard", "presets", "eslint", "rules.json")
	if path != expected {
		t.Fatalf("path = %s, want %s", path, expected)
	}
	if _, err := MergeGlobalRuleTemplate("eslint", "no-debugger", []byte(`"warn"`)); err != nil {
		t.Fatalf("second MergeGlobalRuleTemplate: %v", err)
	}
	data, err := ReadGlobalRuleTemplate("eslint")
	if err != nil {
		t.Fatalf("ReadGlobalRuleTemplate: %v", err)
	}
	var rules map[string]any
	if err := json.Unmarshal(data, &rules); err != nil {
		t.Fatalf("global template is not JSON: %v\n%s", err, data)
	}
	if rules["no-console"] != "error" || rules["no-debugger"] != "warn" {
		t.Fatalf("global rules = %v", rules)
	}
	if _, err := MergeGlobalRuleTemplate("golangci", "x", []byte("y")); err == nil {
		t.Fatal("MergeGlobalRuleTemplate invalid engine = nil, want error")
	}
}
