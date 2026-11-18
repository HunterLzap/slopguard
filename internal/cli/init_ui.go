package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const uiConfigRelPath = ".slopguard/ui.yaml"

// uiScaffoldConfig is the minimal UI smoke configuration written by
// `slopguard init --ui`. Keep the field names aligned with adapter.RunUISmoke.
type uiScaffoldConfig struct {
	BaseURL       string   `yaml:"base_url"`
	Routes        []string `yaml:"routes"`
	ScreenshotDir string   `yaml:"screenshot_dir"`
	TimeoutSec    int      `yaml:"timeout_sec,omitempty"`
}

// runUIScaffold writes .slopguard/ui.yaml without touching an existing file.
func runUIScaffold(root string, stdout, stderr io.Writer) int {
	path := filepath.Join(root, filepath.FromSlash(uiConfigRelPath))
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(stdout, "Kept existing %s\n", uiConfigRelPath)
		return ExitOK
	} else if !os.IsNotExist(err) {
		return commandError(stderr, "stat %s: %v", uiConfigRelPath, err)
	}

	cfg := uiScaffoldConfig{
		BaseURL:       "http://127.0.0.1:3000",
		Routes:        []string{"/", "/health"},
		ScreenshotDir: ".slopguard/ui-screenshots",
		TimeoutSec:    60,
	}
	var body bytes.Buffer
	enc := yaml.NewEncoder(&body)
	enc.SetIndent(2)
	if err := enc.Encode(&cfg); err != nil {
		return commandError(stderr, "render %s: %v", uiConfigRelPath, err)
	}
	if err := enc.Close(); err != nil {
		return commandError(stderr, "render %s: %v", uiConfigRelPath, err)
	}
	header := "# SlopGuard UI smoke configuration. Adjust base_url/routes for your app.\n"
	if err := writeFileAtomic(path, append([]byte(header), body.Bytes()...), 0o644); err != nil {
		return commandError(stderr, "write %s: %v", uiConfigRelPath, err)
	}
	fmt.Fprintf(stdout, "Created %s\n", uiConfigRelPath)
	return ExitOK
}
