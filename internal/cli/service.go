package cli

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/ui"
)

const launchAgentLabel = "com.slopguard.ui"

type serviceEnv struct {
	goos          string
	homeDir       func() (string, error)
	executable    func() (string, error)
	runLaunchctl  func(args ...string) error
	isDashboardUp func(port int) bool
}

var currentServiceEnv = serviceEnv{
	goos:       runtime.GOOS,
	homeDir:    os.UserHomeDir,
	executable: os.Executable,
	runLaunchctl: func(args ...string) error {
		cmd := exec.Command("launchctl", args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
			return err
		}
		return nil
	},
	isDashboardUp: isLocalDashboardResponding,
}

func runService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printServiceUsage(stderr)
		return ExitError
	}
	sub := args[0]
	if sub == "-h" || sub == "--help" || sub == "help" {
		printServiceUsage(stdout)
		return ExitOK
	}

	fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 7788, "local dashboard port (127.0.0.1 only)")
	help, err := parseFlags(fs, args[1:])
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	if help {
		return ExitOK
	}
	if *port < 1 || *port > 65535 {
		return commandError(stderr, "--port must be between 1 and 65535")
	}

	env := currentServiceEnv
	if env.goos != "darwin" {
		fmt.Fprintf(stdout, "slopguard service is only supported on macOS (LaunchAgent); on %s, run:\n  slopguard ui --no-open --port %d &\n", env.goos, *port)
		return ExitOK
	}

	switch sub {
	case "install":
		return runServiceInstall(env, *port, stdout, stderr)
	case "uninstall":
		return runServiceUninstall(env, stdout, stderr)
	case "status":
		return runServiceStatus(env, *port, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "slopguard: unknown service subcommand %q\n", sub)
		printServiceUsage(stderr)
		return ExitError
	}
}

func printServiceUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  slopguard service install   [--port 7788]
  slopguard service uninstall
  slopguard service status    [--port 7788]
`)
}

func launchAgentPaths(env serviceEnv) (plistPath string, logPath string, err error) {
	home, err := env.homeDir()
	if err != nil {
		return "", "", fmt.Errorf("locate user home: %w", err)
	}
	plistPath = filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	logPath = filepath.Join(home, ".slopguard", "ui.log")
	return plistPath, logPath, nil
}

func runServiceInstall(env serviceEnv, port int, stdout, stderr io.Writer) int {
	plistPath, logPath, err := launchAgentPaths(env)
	if err != nil {
		return commandError(stderr, "service install: %v", err)
	}
	exe, err := env.executable()
	if err != nil {
		return commandError(stderr, "locate slopguard binary: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return commandError(stderr, "create LaunchAgents directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return commandError(stderr, "create log directory: %v", err)
	}

	plist := buildLaunchAgentPlist(exe, port, logPath)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return commandError(stderr, "write plist %s: %v", plistPath, err)
	}

	// Best-effort unload in case a previous definition is loaded.
	_ = env.runLaunchctl("unload", plistPath)
	if err := env.runLaunchctl("load", "-w", plistPath); err != nil {
		return commandError(stderr, "launchctl load %s: %v", plistPath, err)
	}
	fmt.Fprintf(stdout, "Installed SlopGuard background UI service (%s)\n  plist: %s\n  url:   http://127.0.0.1:%d\n", launchAgentLabel, plistPath, port)
	return ExitOK
}

func runServiceUninstall(env serviceEnv, stdout, stderr io.Writer) int {
	plistPath, _, err := launchAgentPaths(env)
	if err != nil {
		return commandError(stderr, "service uninstall: %v", err)
	}
	if _, statErr := os.Stat(plistPath); os.IsNotExist(statErr) {
		fmt.Fprintf(stdout, "SlopGuard background UI service is not installed (%s)\n", plistPath)
		return ExitOK
	}
	_ = env.runLaunchctl("unload", "-w", plistPath)
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return commandError(stderr, "remove plist %s: %v", plistPath, err)
	}
	fmt.Fprintf(stdout, "Uninstalled SlopGuard background UI service (%s)\n", plistPath)
	return ExitOK
}

func runServiceStatus(env serviceEnv, port int, stdout, stderr io.Writer) int {
	plistPath, _, err := launchAgentPaths(env)
	if err != nil {
		return commandError(stderr, "service status: %v", err)
	}
	_, statErr := os.Stat(plistPath)
	installed := statErr == nil
	running := env.isDashboardUp(port)
	fmt.Fprintf(stdout, "Service:   %s\nInstalled: %v (%s)\nRunning:   %v (http://127.0.0.1:%d)\n",
		launchAgentLabel, installed, plistPath, running, port)
	return ExitOK
}

func buildLaunchAgentPlist(exe string, port int, logPath string) string {
	esc := func(s string) string {
		var buf bytes.Buffer
		_ = xml.EscapeText(&buf, []byte(s))
		return buf.String()
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>ui</string>
    <string>--no-open</string>
    <string>--port</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, esc(launchAgentLabel), esc(exe), strconv.Itoa(port), esc(logPath), esc(logPath))
}

type dashboardIdentity struct {
	Service    string `json:"service"`
	InstanceID string `json:"instance_id"`
	PID        int    `json:"pid"`
	Port       int    `json:"port"`
}

func readDashboardIdentity(port int) (dashboardIdentity, bool) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/identity", port))
	if err != nil {
		return dashboardIdentity{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return dashboardIdentity{}, false
	}
	var identity dashboardIdentity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&identity); err != nil {
		return dashboardIdentity{}, false
	}
	return identity, identity.Service == "slopguard"
}

func isLocalDashboardResponding(port int) bool {
	_, ok := readDashboardIdentity(port)
	return ok
}

// isTrustedDashboardResponding accepts a running dashboard only when its
// /api/identity instance_id matches the user's local ui.lock record. This
// prevents another local process from impersonating the dashboard on the port.
func isTrustedDashboardResponding(port int) bool {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return false
	}
	_, ok := ui.ProbeTrustedDashboard(home, port)
	return ok
}
