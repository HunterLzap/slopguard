package adapter

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestTypeScriptAdapterDetect(t *testing.T) {
	adapter := NewTypeScriptAdapter()
	empty := t.TempDir()
	if adapter.Detect(empty) {
		t.Fatal("empty directory detected as TS project")
	}
	for _, marker := range []string{"package.json", "tsconfig.json", "jsconfig.json"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, marker), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !adapter.Detect(dir) {
			t.Fatalf("%s not detected", marker)
		}
	}
}

func TestTypeScriptAdapterExtensions(t *testing.T) {
	exts := NewTypeScriptAdapter().Extensions()
	want := map[string]bool{".ts": true, ".tsx": true, ".mts": true, ".cts": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true}
	if len(exts) != len(want) {
		t.Fatalf("extensions = %v", exts)
	}
	for _, ext := range exts {
		if !want[ext] {
			t.Fatalf("unexpected extension %q", ext)
		}
	}
}

func TestTypeScriptChecksWithTSConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tsconfig.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := NewTypeScriptAdapter().Checks(root, nil)
	if len(checks) != 4 {
		t.Fatalf("len(checks) = %d, want 4: %+v", len(checks), checks)
	}
	byID := map[string]int{}
	for i, check := range checks {
		byID[check.ID] = i
	}
	tsc := checks[byID["tsc"]]
	if tsc.Cmd[0] != "tsc" || !containsArg(tsc.Cmd, "--project") {
		t.Fatalf("tsc cmd = %v", tsc.Cmd)
	}
	if !strings.HasSuffix(tsc.Cmd[len(tsc.Cmd)-1], "tsconfig.json") {
		t.Fatalf("tsc cmd missing tsconfig path: %v", tsc.Cmd)
	}
	eslint := checks[byID["eslint"]]
	if !containsArg(eslint.Cmd, ".") {
		t.Fatalf("eslint cmd = %v", eslint.Cmd)
	}
	if !checks[byID["knip"]].Optional || !checks[byID["jscpd"]].Optional {
		t.Fatal("knip/jscpd should be optional")
	}
}

func TestTypeScriptChecksWithFiles(t *testing.T) {
	root := t.TempDir()
	files := []string{"src/a.ts", "src/b.py", "src/c.jsx"}
	checks := NewTypeScriptAdapter().Checks(root, files)
	var eslintCmd, tscCmd []string
	for _, check := range checks {
		switch check.ID {
		case "eslint":
			eslintCmd = check.Cmd
		case "tsc":
			tscCmd = check.Cmd
		}
	}
	if !containsArg(eslintCmd, "src/a.ts") || !containsArg(eslintCmd, "src/c.jsx") {
		t.Fatalf("eslint cmd = %v", eslintCmd)
	}
	if containsArg(eslintCmd, "src/b.py") {
		t.Fatalf("eslint cmd should not include python file: %v", eslintCmd)
	}
	if len(tscCmd) == 0 || !containsArg(tscCmd, "src/a.ts") {
		t.Fatalf("tsc cmd = %v, want explicit ts file", tscCmd)
	}
	if containsArg(tscCmd, "src/b.py") {
		t.Fatalf("tsc should not receive python file: %v", tscCmd)
	}
}

func TestTypeScriptChecksTSFilesWithoutTSConfig(t *testing.T) {
	root := t.TempDir()
	checks := NewTypeScriptAdapter().Checks(root, []string{"src/a.ts"})
	found := false
	for _, check := range checks {
		if check.ID == "tsc" {
			found = true
			if !containsArg(check.Cmd, "src/a.ts") {
				t.Fatalf("tsc cmd = %v", check.Cmd)
			}
		}
	}
	if !found {
		t.Fatal("tsc check missing for .ts file without tsconfig")
	}
}

func TestTypeScriptChecksTSConfigRelativePathWithSpaces(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project with spaces")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tsconfig.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := NewTypeScriptAdapter().Checks(root, nil)
	var tscCmd []string
	for _, check := range checks {
		if check.ID == "tsc" {
			tscCmd = check.Cmd
		}
	}
	if len(tscCmd) == 0 {
		t.Fatal("tsc check missing")
	}
	projectArg := -1
	for i, arg := range tscCmd {
		if arg == "--project" {
			projectArg = i
			break
		}
	}
	if projectArg < 0 || projectArg+1 >= len(tscCmd) {
		t.Fatalf("tsc cmd missing --project value: %v", tscCmd)
	}
	if got := tscCmd[projectArg+1]; got != "tsconfig.json" {
		t.Fatalf("--project value = %q, want relative tsconfig.json", got)
	}
	for _, arg := range tscCmd {
		if strings.Contains(arg, root) {
			t.Fatalf("tsc cmd contains absolute project path %q: %v", root, tscCmd)
		}
	}
}

func TestTypeScriptEscapePatterns(t *testing.T) {
	patterns := NewTypeScriptAdapter().EscapePatterns()
	wantNames := []string{"ts_ignore", "ts_expect_error", "eslint_disable", "explicit_any"}
	if len(patterns) != len(wantNames) {
		t.Fatalf("patterns = %+v", patterns)
	}
	for i, name := range wantNames {
		if patterns[i].Name != name {
			t.Fatalf("patterns[%d].Name = %q, want %q", i, patterns[i].Name, name)
		}
		if _, err := regexp.Compile(patterns[i].Regex); err != nil {
			t.Fatalf("pattern %s invalid: %v", name, err)
		}
	}
	samples := map[string]string{
		"ts_ignore":       "// @ts-ignore",
		"ts_expect_error": "// @ts-expect-error",
		"eslint_disable":  "// eslint-disable-next-line",
		"explicit_any":    "const x: any = 1",
	}
	for name, sample := range samples {
		var pattern string
		for _, p := range patterns {
			if p.Name == name {
				pattern = p.Regex
			}
		}
		re := regexp.MustCompile(pattern)
		if !re.MatchString(sample) {
			t.Fatalf("%s pattern %q does not match %q", name, pattern, sample)
		}
	}
}

func TestFilterFilesByExtensions(t *testing.T) {
	files := []string{"a.TS", "b.py", "c.tsx", "d"}
	got := filterFilesByExtensions(files, []string{".ts", ".tsx"})
	if len(got) != 2 || got[0] != "a.TS" || got[1] != "c.tsx" {
		t.Fatalf("filtered = %v", got)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
