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

func TestRunUISmartLaunchWhenAlreadyRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldCheck := checkDashboardRunning
	oldOpen := openDashboardWindow
	oldRun := runDashboard
	t.Cleanup(func() {
		checkDashboardRunning = oldCheck
		openDashboardWindow = oldOpen
		runDashboard = oldRun
	})

	checkDashboardRunning = func(port int) bool { return port == 7788 }
	var openedURL string
	openDashboardWindow = func(url string) error {
		openedURL = url
		return nil
	}
	runCalled := false
	runDashboard = func(_ context.Context, _ contract.Runner, _ dashboardOptions, _ io.Writer) error {
		runCalled = true
		return nil
	}

	var stdout, stderr bytes.Buffer
	if code := runUI([]string{"--port", "7788"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("runUI exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if runCalled {
		t.Fatal("runDashboard should not be called when service is already responding")
	}
	if openedURL != "http://127.0.0.1:7788" {
		t.Fatalf("openedURL = %q, want http://127.0.0.1:7788", openedURL)
	}
	if !strings.Contains(stdout.String(), "already running") {
		t.Fatalf("stdout = %q, want already running message", stdout.String())
	}
}
