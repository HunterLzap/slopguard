package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func TestCountEscapes(t *testing.T) {
	dir := t.TempDir()
	content := strings.Join([]string{
		"const a: any = 1; // @ts-ignore // @ts-ignore",
		"const b: string = 'ok';",
		"// @ts-expect-error",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "a.ts"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	patterns := []contract.EscapePattern{
		{Name: "ts_ignore", Regex: `//\s*@ts-ignore`},
		{Name: "ts_expect_error", Regex: `//\s*@ts-expect-error`},
		{Name: "explicit_any", Regex: `:\s*any\b`},
		{Name: "unused", Regex: `NEVER_MATCH`},
	}
	counts, err := CountEscapes(dir, []string{"a.ts"}, patterns)
	if err != nil {
		t.Fatalf("CountEscapes error: %v", err)
	}
	want := map[string]int{"ts_ignore": 2, "ts_expect_error": 1, "explicit_any": 1, "unused": 0}
	for name, value := range want {
		if counts[name] != value {
			t.Fatalf("counts[%s] = %d, want %d (all: %v)", name, counts[name], value, counts)
		}
	}
	if len(counts) != len(want) {
		t.Fatalf("counts = %v, want %d keys", counts, len(want))
	}
}

func TestCountEscapesMissingFileSkipped(t *testing.T) {
	counts, err := CountEscapes(t.TempDir(), []string{"missing.ts"}, []contract.EscapePattern{
		{Name: "ts_ignore", Regex: `@ts-ignore`},
	})
	if err != nil {
		t.Fatalf("CountEscapes error: %v", err)
	}
	if counts["ts_ignore"] != 0 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestCountEscapesInvalidRegex(t *testing.T) {
	_, err := CountEscapes(t.TempDir(), nil, []contract.EscapePattern{{Name: "bad", Regex: `[`}})
	if err == nil || !strings.Contains(err.Error(), "invalid escape pattern") {
		t.Fatalf("err = %v", err)
	}
}

func TestCountEscapesEmptyPatterns(t *testing.T) {
	counts, err := CountEscapes(t.TempDir(), []string{"a.ts"}, nil)
	if err != nil {
		t.Fatalf("CountEscapes error: %v", err)
	}
	if len(counts) != 0 {
		t.Fatalf("counts = %v, want empty", counts)
	}
}

func TestCountEscapesLexicalStrippingAvoidsStringsAndComments(t *testing.T) {
	dir := t.TempDir()
	content := strings.Join([]string{
		`const s = "x: any"; // note: any`,
		"const s2 = 'y: any';",
		"const tmpl = `template: any`;",
		"/* block: any\n   still comment: any */",
		`const fakeIgnore = "// @ts-ignore";`,
		"function f(x: any) { return x; }",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "sample.ts"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	patterns := []contract.EscapePattern{
		{Name: "ts_ignore", Regex: `(?m)//\s*@ts-ignore`, Target: "comment"},
		{Name: "explicit_any", Regex: `(?m):\s*any\b`, Target: "code"},
	}
	counts, err := CountEscapes(dir, []string{"sample.ts"}, patterns)
	if err != nil {
		t.Fatalf("CountEscapes error: %v", err)
	}
	if counts["explicit_any"] != 1 {
		t.Fatalf("explicit_any = %d, want 1 (only function f(x: any))", counts["explicit_any"])
	}
	if counts["ts_ignore"] != 0 {
		t.Fatalf("ts_ignore = %d, want 0 (string literal must be ignored)", counts["ts_ignore"])
	}

	// File with ONLY string/comment `: any` yields 0.
	onlyFalsePositives := "const s = \"x: any\"; // note: any\nconst t = `z: any`;\n"
	if err := os.WriteFile(filepath.Join(dir, "clean.ts"), []byte(onlyFalsePositives), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanCounts, err := CountEscapes(dir, []string{"clean.ts"}, patterns)
	if err != nil {
		t.Fatal(err)
	}
	if cleanCounts["explicit_any"] != 0 {
		t.Fatalf("clean explicit_any = %d, want 0", cleanCounts["explicit_any"])
	}
}
