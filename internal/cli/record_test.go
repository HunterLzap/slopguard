package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/store"
)

func setTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func TestRecordCommandCountsDuplicates(t *testing.T) {
	home := setTestHome(t)
	project := t.TempDir()
	args := []string{
		"--project", project,
		"--category", "type",
		"--summary", "avoid any in exported API",
		"--bad-snippet", "let x: any",
		"--good-snippet", "let x: unknown",
	}

	var stdout, stderr bytes.Buffer
	if code := runRecord(args, &stdout, &stderr); code != ExitOK {
		t.Fatalf("first record exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "count=1") {
		t.Fatalf("first record stdout = %q, want count=1", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runRecord(args, &stdout, &stderr); code != ExitOK {
		t.Fatalf("second record exit = %d\nstderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "count=2") {
		t.Fatalf("second record stdout = %q, want count=2", stdout.String())
	}

	db, err := store.Open(filepath.Join(home, ".slopguard", "slopguard.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	candidates, err := db.ListCandidates(context.Background(), 2)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %v, want one cluster", candidates)
	}
	if candidates[0].Summary != "avoid any in exported API" || candidates[0].Category != "type" {
		t.Fatalf("candidate = %+v", candidates[0])
	}
	if candidates[0].BadSnippet != "let x: any" || candidates[0].GoodSnippet != "let x: unknown" {
		t.Fatalf("snippets = %q / %q", candidates[0].BadSnippet, candidates[0].GoodSnippet)
	}
}

func TestRecordCommandRequiresSummary(t *testing.T) {
	setTestHome(t)
	var stdout, stderr bytes.Buffer
	code := runRecord([]string{"--project", t.TempDir()}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "--summary is required") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRecordCommandStoreError(t *testing.T) {
	homeFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(homeFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeFile)
	t.Setenv("USERPROFILE", homeFile)

	var stdout, stderr bytes.Buffer
	code := runRecord([]string{"--project", t.TempDir(), "--summary", "x"}, &stdout, &stderr)
	if code != ExitError {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitError, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "open mistake store") {
		t.Fatalf("stderr = %q, want store open error", stderr.String())
	}
}

func TestMistakeFingerprintStable(t *testing.T) {
	first := mistakeFingerprint("/project", "type", "same")
	if second := mistakeFingerprint("/project", "type", "same"); first != second {
		t.Fatalf("fingerprint not stable: %q != %q", first, second)
	}
	if other := mistakeFingerprint("/project", "type", "different"); first == other {
		t.Fatal("different summaries produced the same fingerprint")
	}
	if other := mistakeFingerprint("/other", "type", "same"); first == other {
		t.Fatal("different projects produced the same fingerprint")
	}
}
