package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type dashboardOptions struct {
	port   int
	noOpen bool
}

// runDashboard is assigned to the production implementation in wire.go.
var runDashboard func(ctx context.Context, runner contract.Runner, opts dashboardOptions, stdout io.Writer) error

var (
	checkDashboardRunning = isLocalDashboardResponding
	openDashboardWindow   = defaultOpenDashboardWindow
)

func runUI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 7788, "local dashboard port (127.0.0.1 only)")
	noOpen := fs.Bool("no-open", false, "do not open the system browser")
	help, err := parseFlags(fs, args)
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	if help {
		return ExitOK
	}
	if *port < 1 || *port > 65535 {
		return commandError(stderr, "--port must be between 1 and 65535")
	}

	if checkDashboardRunning != nil && checkDashboardRunning(*port) {
		displayURL := fmt.Sprintf("http://127.0.0.1:%d", *port)
		if !*noOpen && openDashboardWindow != nil {
			if err := openDashboardWindow(appendLocalUIToken(displayURL)); err != nil {
				fmt.Fprintf(stderr, "slopguard: warning: open dashboard window: %v\n", err)
			}
		}
		fmt.Fprintf(stdout, "SlopGuard dashboard already running at %s\n", displayURL)
		return ExitOK
	}

	deps, err := newDependencies()
	if err != nil {
		return commandError(stderr, "prepare dashboard: %v", err)
	}
	if runDashboard == nil {
		return commandError(stderr, "dashboard is not wired")
	}
	ctx, stop := newCommandContext(deps.engine)
	defer stop()
	if err := runDashboard(ctx, deps.runner, dashboardOptions{port: *port, noOpen: *noOpen}, stdout); err != nil {
		return commandError(stderr, "dashboard: %v", err)
	}
	return ExitOK
}

func appendLocalUIToken(rawURL string) string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return rawURL
	}
	raw, err := os.ReadFile(filepath.Join(home, ".slopguard", "ui-token"))
	if err != nil {
		return rawURL
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set("token", tok)
	u.RawQuery = q.Encode()
	return u.String()
}

func defaultOpenDashboardWindow(targetURL string) error {
	name, args := standaloneWindowCommand(runtime.GOOS, targetURL)
	return exec.Command(name, args...).Start()
}

func standaloneWindowCommand(goos, targetURL string) (string, []string) {
	appArgs := []string{"--app=" + targetURL, "--window-size=1180,780"}
	if goos == "darwin" {
		appRelPaths := []string{
			"Google Chrome.app/Contents/MacOS/Google Chrome",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"Brave Browser.app/Contents/MacOS/Brave Browser",
			"Arc.app/Contents/MacOS/Arc",
			"Chromium.app/Contents/MacOS/Chromium",
		}
		var candidates []string
		for _, rel := range appRelPaths {
			candidates = append(candidates, filepath.Join("/Applications", rel))
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			for _, rel := range appRelPaths {
				candidates = append(candidates, filepath.Join(home, "Applications", rel))
			}
		}
		for _, c := range candidates {
			if info, err := os.Stat(c); err == nil && !info.IsDir() {
				return c, appArgs
			}
		}
		return "open", []string{targetURL}
	}
	if goos == "windows" {
		for _, bin := range []string{"chrome.exe", "msedge.exe", "brave.exe", "chromium.exe"} {
			if p, err := exec.LookPath(bin); err == nil && p != "" {
				return p, appArgs
			}
		}
		return "rundll32", []string{"url.dll,FileProtocolHandler", targetURL}
	}
	for _, bin := range []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"} {
		if p, err := exec.LookPath(bin); err == nil && p != "" {
			return p, appArgs
		}
	}
	return "xdg-open", []string{targetURL}
}
