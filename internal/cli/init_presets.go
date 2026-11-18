package cli

import (
	"fmt"
	"io"
	"strings"
)

// runPresetSync previews global rule presets, asks for confirmation, then
// applies the plan. Planning never writes; ApplyPresets performs backups and
// the non-destructive merge.
func runPresetSync(deps dependencies, root string, yes bool, stdin io.Reader, stdout, stderr io.Writer) int {
	if deps.presets == nil {
		return commandError(stderr, "preset sync is not available")
	}
	plan, err := deps.presets.PlanPresets("", root)
	if err != nil {
		return commandError(stderr, "plan presets: %v", err)
	}
	if len(plan.Written) == 0 && len(plan.Skipped) == 0 {
		fmt.Fprintln(stdout, "No global presets found in ~/.slopguard/presets.")
		return ExitOK
	}

	fmt.Fprintf(stdout, "Preset merge plan: %d to write, %d skipped\n", len(plan.Written), len(plan.Skipped))
	for _, item := range plan.Written {
		fmt.Fprintf(stdout, "  write %s/%s", item.Engine, item.RuleID)
		if item.ConfigPath != "" {
			fmt.Fprintf(stdout, " -> %s", item.ConfigPath)
		}
		fmt.Fprintln(stdout)
	}
	for _, item := range plan.Skipped {
		fmt.Fprintf(stdout, "  skip  %s/%s", item.Engine, item.RuleID)
		if item.Reason != "" {
			fmt.Fprintf(stdout, ": %s", item.Reason)
		}
		fmt.Fprintln(stdout)
	}
	for _, diff := range plan.Diffs {
		if strings.TrimSpace(diff) != "" {
			fmt.Fprintln(stdout, "Preview diff:")
			fmt.Fprint(stdout, diff)
			if !strings.HasSuffix(diff, "\n") {
				fmt.Fprintln(stdout)
			}
		}
	}

	if len(plan.Written) == 0 {
		fmt.Fprintln(stdout, "No preset changes required.")
		return ExitOK
	}
	apply := yes
	if !apply {
		fmt.Fprint(stdout, "Apply these preset changes? [y/N] ")
		apply = confirm(stdin)
	}
	if !apply {
		fmt.Fprintln(stdout, "No preset changes made. Re-run with --yes to apply.")
		return ExitOK
	}

	result, err := deps.presets.ApplyPresets(plan)
	if err != nil {
		return commandError(stderr, "apply presets: %v", err)
	}
	fmt.Fprintf(stdout, "Presets written: %d, skipped: %d\n", len(result.Written), len(result.Skipped))
	for _, item := range result.Written {
		fmt.Fprintf(stdout, "  written %s/%s", item.Engine, item.RuleID)
		if item.ConfigPath != "" {
			fmt.Fprintf(stdout, " -> %s", item.ConfigPath)
		}
		fmt.Fprintln(stdout)
	}
	for _, item := range result.Skipped {
		fmt.Fprintf(stdout, "  skipped %s/%s", item.Engine, item.RuleID)
		if item.Reason != "" {
			fmt.Fprintf(stdout, ": %s", item.Reason)
		}
		fmt.Fprintln(stdout)
	}
	if len(result.Backups) > 0 {
		fmt.Fprintf(stdout, "Backups: %s\n", strings.Join(result.Backups, ", "))
	}
	return ExitOK
}
