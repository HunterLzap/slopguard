package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceInstallUninstallStatusDarwin(t *testing.T) {
	home := t.TempDir()
	oldEnv := currentServiceEnv
	t.Cleanup(func() { currentServiceEnv = oldEnv })

	var launchctlCalls [][]string
	running := false
	currentServiceEnv = serviceEnv{
		goos:       "darwin",
		homeDir:    func() (string, error) { return home, nil },
		executable: func() (string, error) { return "/usr/local/bin/slopguard", nil },
		runLaunchctl: func(args ...string) error {
			launchctlCalls = append(launchctlCalls, args)
			return nil
		},
		isDashboardUp: func(port int) bool { return running && port == 7788 },
	}

	var stdout, stderr bytes.Buffer
	if code := Execute([]string{"service", "install", "--port", "7788"}, nil, &stdout, &stderr); code != ExitOK {
		t.Fatalf("service install exit = %d, stderr = %s", code, stderr.String())
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	plist := string(data)
	for _, want := range []string{
		launchAgentLabel,
		"/usr/local/bin/slopguard",
		"<string>--no-open</string>",
		"<string>7788</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
	if len(launchctlCalls) < 2 || launchctlCalls[1][0] != "load" {
		t.Fatalf("launchctlCalls = %+v, want load", launchctlCalls)
	}

	running = true
	stdout.Reset()
	stderr.Reset()
	if code := Execute([]string{"service", "status", "--port", "7788"}, nil, &stdout, &stderr); code != ExitOK {
		t.Fatalf("service status exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Installed: true") || !strings.Contains(stdout.String(), "Running:   true") {
		t.Fatalf("status output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Execute([]string{"service", "uninstall"}, nil, &stdout, &stderr); code != ExitOK {
		t.Fatalf("service uninstall exit = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("plist still exists after uninstall: err=%v", err)
	}
}

func TestServiceNonDarwinGracefulMessage(t *testing.T) {
	oldEnv := currentServiceEnv
	t.Cleanup(func() { currentServiceEnv = oldEnv })

	currentServiceEnv = serviceEnv{
		goos: "linux",
	}

	var stdout, stderr bytes.Buffer
	if code := Execute([]string{"service", "install"}, nil, &stdout, &stderr); code != ExitOK {
		t.Fatalf("non-darwin service exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "only supported on macOS") {
		t.Fatalf("stdout = %q, want graceful message", stdout.String())
	}
}
