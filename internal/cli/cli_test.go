package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteNoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Execute(nil, nil, &stdout, &stderr); code != ExitError {
		t.Fatalf("code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestExecuteHelp(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		var stdout, stderr bytes.Buffer
		if code := Execute([]string{arg}, nil, &stdout, &stderr); code != ExitOK {
			t.Fatalf("%s code = %d, want 0", arg, code)
		}
		if !strings.Contains(stdout.String(), "SlopGuard") {
			t.Fatalf("%s stdout = %q", arg, stdout.String())
		}
	}
}

func TestExecuteUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Execute([]string{"frobnicate"}, nil, &stdout, &stderr); code != ExitError {
		t.Fatalf("code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), `unknown command "frobnicate"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestExecuteInitHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Execute([]string{"init", "--help"}, nil, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code = %d, want 0", code)
	}
}

func TestExecuteVerifyBadJobs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Execute([]string{"verify", "--jobs", "-1"}, nil, &stdout, &stderr); code != ExitError {
		t.Fatalf("code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "--jobs must be >= 1") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestNormalizeText(t *testing.T) {
	in := append([]byte{0xEF, 0xBB, 0xBF}, []byte("a\r\nb\r\n")...)
	if got := string(normalizeText(in)); got != "a\nb\n" {
		t.Fatalf("normalizeText = %q", got)
	}
}

func TestFormatDiff(t *testing.T) {
	got := formatDiff(".slopguard/config.yaml", []byte("a\nb\n"), []byte("a\nc\n"))
	if !strings.Contains(got, "--- a/.slopguard/config.yaml") || !strings.Contains(got, "+++ b/.slopguard/config.yaml") {
		t.Fatalf("diff header missing:\n%s", got)
	}
	if !strings.Contains(got, "-b") || !strings.Contains(got, "+c") || !strings.Contains(got, " a") {
		t.Fatalf("diff body wrong:\n%s", got)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")
	if err := writeFileAtomic(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "hello" {
		t.Fatalf("data = %q, err = %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".slopguard-tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestConfirm(t *testing.T) {
	cases := map[string]bool{
		"y\n":   true,
		"YES\n": true,
		"yes\n": true,
		"n\n":   false,
		"\n":    false,
		"":      false,
		"yep\n": false,
	}
	for input, want := range cases {
		if got := confirm(strings.NewReader(input)); got != want {
			t.Errorf("confirm(%q) = %v, want %v", input, got, want)
		}
	}
	if confirm(nil) {
		t.Error("confirm(nil) = true, want false")
	}
}
