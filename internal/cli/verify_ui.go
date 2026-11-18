package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// runUISmokeForVerify invokes the optional UI smoke runner. A missing runner is
// treated as a graceful skip so verify keeps its regular exit code.
func runUISmokeForVerify(ctx context.Context, deps dependencies, root string) (contract.UISmokeResult, error) {
	if deps.ui == nil {
		return contract.UISmokeResult{Skipped: true, Reason: "UI smoke runner is not available"}, nil
	}
	return deps.ui.RunUISmoke(ctx, uiOptions{ProjectRoot: root})
}

// printUISmokeResult renders the UI smoke section of human-readable output.
func printUISmokeResult(w io.Writer, result *contract.UISmokeResult) {
	if result == nil {
		return
	}
	if result.Skipped {
		reason := result.Reason
		if reason == "" {
			reason = "no UI configuration or Playwright is not installed"
		}
		fmt.Fprintf(w, "ui smoke: skipped (%s)\n", reason)
		return
	}
	for _, finding := range result.Findings {
		if finding.File != "" {
			fmt.Fprintf(w, "ui %s: %s\n", finding.File, finding.Message)
		} else {
			fmt.Fprintf(w, "ui: %s\n", finding.Message)
		}
	}
	for _, screenshot := range result.Screenshots {
		fmt.Fprintf(w, "ui screenshot: %s\n", screenshot)
	}
	status := "PASS"
	if !result.Passed {
		status = "FAIL"
	}
	fmt.Fprintf(w, "ui smoke: %s (%d issues, %dms)\n", status, len(result.Findings), result.DurationMs)
}
