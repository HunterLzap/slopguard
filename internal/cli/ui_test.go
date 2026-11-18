package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func TestRunUIPassesPortAndNoOpen(t *testing.T) {
	old := runDashboard
	t.Cleanup(func() { runDashboard = old })
	var got dashboardOptions
	called := false
	runDashboard = func(_ context.Context, _ contract.Runner, opts dashboardOptions, _ io.Writer) error {
		called = true
		got = opts
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := runUI([]string{"--port", "12345", "--no-open"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("runUI exit = %d, stderr = %s", code, stderr.String())
	}
	if !called {
		t.Fatal("dashboard runner was not called")
	}
	if got.port != 12345 || !got.noOpen {
		t.Fatalf("dashboard options = %+v", got)
	}
}

func TestRunUIInvalidPort(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runUI([]string{"--port", "70000"}, &stdout, &stderr); code != ExitError {
		t.Fatalf("runUI invalid port exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "--port must be between 1 and 65535") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunUIHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runUI([]string{"--help"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("runUI help exit = %d, want %d", code, ExitOK)
	}
}
