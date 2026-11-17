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
