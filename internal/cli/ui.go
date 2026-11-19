package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/ui"
)

type dashboardOptions struct {
	port   int
	noOpen bool
}

// runDashboard is assigned to the production implementation in wire.go.
var runDashboard func(ctx context.Context, runner contract.Runner, opts dashboardOptions, stdout io.Writer) error

var (
	checkDashboardRunning = isTrustedDashboardResponding
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
			targetURL := displayURL
			if ticketURL, err := requestDashboardTicket(displayURL); err == nil && ticketURL != "" {
				targetURL = ticketURL
			}
			if err := openDashboardWindow(targetURL); err != nil {
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
		var already *ui.AlreadyRunningError
		if errors.As(err, &already) {
			displayURL := already.URL
			if strings.TrimSpace(displayURL) == "" {
				displayURL = fmt.Sprintf("http://127.0.0.1:%d", already.Port)
			}
			if !*noOpen && openDashboardWindow != nil {
				targetURL := displayURL
				if ticketURL, ticketErr := requestDashboardTicket(displayURL); ticketErr == nil && ticketURL != "" {
					targetURL = ticketURL
				}
				if openErr := openDashboardWindow(targetURL); openErr != nil {
					fmt.Fprintf(stderr, "slopguard: warning: open dashboard window: %v\n", openErr)
				}
			}
			fmt.Fprintf(stdout, "SlopGuard dashboard already running at %s\n", displayURL)
			return ExitOK
		}
		return commandError(stderr, "dashboard: %v", err)
	}
	return ExitOK
}

// requestDashboardTicket uses the local long-term token only in an HTTP header
// and obtains a short-lived, one-time browser bootstrap URL. The long-term token
// is never appended to a URL or passed to a browser process.
func requestDashboardTicket(baseURL string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", errors.New("locate user home")
	}
	raw, err := os.ReadFile(filepath.Join(home, ".slopguard", "ui-token"))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("empty local UI token")
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/auth/ticket", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-SlopGuard-Token", token)
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ticket endpoint returned %d", resp.StatusCode)
	}
	var payload struct {
		Ticket string `json:"ticket"`
		URL    string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.Ticket) == "" {
		return "", errors.New("empty dashboard ticket")
	}
	if strings.TrimSpace(payload.URL) != "" {
		if strings.HasPrefix(payload.URL, "http://") || strings.HasPrefix(payload.URL, "https://") {
			return payload.URL, nil
		}
		return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(payload.URL, "/"), nil
	}
	return strings.TrimRight(baseURL, "/") + "/?ticket=" + payload.Ticket, nil
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
