package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/verify"
)

type verifyOptions struct {
	root           string
	all            bool
	staged         bool
	lastCommit     bool
	commitSHA      string
	json           bool
	jobs           int
	record         bool
	autoCapture    bool
	captureSource  string
	strictBaseline bool
	strictTools    bool
	ablation       bool
	ui             bool
	paths          []string
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "scan all project files instead of only git-changed files")
	staged := fs.Bool("staged", false, "scan only git staged changes (used by pre-commit hook)")
	lastCommit := fs.Bool("last-commit", false, "scan files touched by HEAD commit (used by post-commit hook)")
	commitSHA := fs.String("commit", "", "scan files touched by a specific commit SHA (used by post-commit hook)")
	jsonOut := fs.Bool("json", false, "emit a structured contract.VerifyResult as JSON")
	jobs := fs.Int("jobs", 0, "maximum concurrent checks (default: config value or 2)")
	record := fs.Bool("record", false, "record a scan_runs row after verify (pass or fail)")
	source := fs.String("source", "auto_verify", "mistake source label when auto-capturing failures")
	strictBaseline := fs.Bool("strict-baseline", false, "fail if .slopguard/baseline.json is missing")
	strictTools := fs.Bool("strict-tools", false, "fail if required external tools are missing")
	ablation := fs.Bool("ablation", false, "run isolated RED->GREEN test ablation in a temporary git worktree")
	ui := fs.Bool("ui", false, "run Playwright UI smoke checks after the regular checks pass")
	rootFlag := fs.String("root", ".", "project root directory")
	help, err := parseFlags(fs, args)
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	if help {
		return ExitOK
	}

	if *jobs < 0 {
		return commandError(stderr, "--jobs must be >= 1")
	}
	scopeCount := 0
	for _, set := range []bool{*all, *staged, *lastCommit, strings.TrimSpace(*commitSHA) != ""} {
		if set {
			scopeCount++
		}
	}
	if scopeCount > 1 {
		return commandError(stderr, "--all, --staged, --last-commit, and --commit are mutually exclusive")
	}
	if *ablation && (*all || *lastCommit || strings.TrimSpace(*commitSHA) != "") {
		return commandError(stderr, "--ablation cannot be combined with --all, --last-commit, or --commit")
	}
	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		return commandError(stderr, "resolve project root: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return commandError(stderr, "project root %s: %v", root, err)
	}

	return runVerifyWith(verifyOptions{
		root:           root,
		all:            *all,
		staged:         *staged,
		lastCommit:     *lastCommit,
		commitSHA:      strings.TrimSpace(*commitSHA),
		json:           *jsonOut,
		jobs:           *jobs,
		record:         *record,
		captureSource:  *source,
		strictBaseline: *strictBaseline,
		strictTools:    *strictTools,
		ablation:       *ablation,
		ui:             *ui,
	}, stdout, stderr)
}

func runVerifyWith(opts verifyOptions, stdout, stderr io.Writer) int {
	deps, err := newDependencies()
	if err != nil {
		return commandError(stderr, "prepare engine: %v", err)
	}

	var rec verify.Recorder
	if deps.recorder != nil {
		rec = cliVerifyRecorder{recorder: deps.recorder}
	}
	var hooks *verify.Hooks
	if !deps.isProduction {
		hooks = buildVerifyHooks(deps)
	}

	result, err := verify.Run(context.Background(), verify.Options{
		ProjectRoot:    opts.root,
		All:            opts.all,
		Staged:         opts.staged,
		LastCommit:     opts.lastCommit,
		CommitSHA:      opts.commitSHA,
		Paths:          opts.paths,
		Jobs:           opts.jobs,
		StrictBaseline: opts.strictBaseline,
		StrictTools:    opts.strictTools,
		Ablation:       opts.ablation,
		UI:             opts.ui,
		Record:         opts.record,
		AutoCapture:    opts.autoCapture,
		CaptureSource:  opts.captureSource,
		Runner:         deps.runner,
		Recorder:       rec,
		Hooks:          hooks,
	})
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	for _, w := range result.Warnings {
		fmt.Fprintf(stderr, "slopguard: warning: %s\n", w)
	}
	writeResult(stdout, opts.json, result)
	if result.Passed {
		return ExitOK
	}
	return ExitFindings
}

type cliVerifyRecorder struct {
	recorder scanRecorder
}

func (r cliVerifyRecorder) RecordScan(ctx context.Context, projectPath string, durationMs int64, escapes int, passed bool) error {
	if r.recorder == nil {
		return nil
	}
	return r.recorder.RecordScan(ctx, projectPath, durationMs, escapes, passed)
}

func (r cliVerifyRecorder) RecordMistake(ctx context.Context, m contract.Mistake) (int, error) {
	if mr, ok := r.recorder.(mistakeRecorder); ok && mr != nil {
		return mr.RecordMistake(ctx, m)
	}
	return 0, nil
}

func buildVerifyHooks(deps dependencies) *verify.Hooks {
	hooks := &verify.Hooks{}
	if deps.engine != nil {
		if provider, ok := deps.engine.(signalContextProvider); ok {
			hooks.SignalContext = provider.SignalContext
		}
		hooks.LoadBaseline = func(path string) (verify.BaselineState, error) {
			b, err := loadBaselineForVerify(deps.engine, path)
			if err != nil {
				return verify.BaselineState{}, err
			}
			return verify.BaselineState{
				Metrics:         b.Metrics,
				IsSuppressed:    b.IsSuppressed,
				HasSuppressions: b.HasSuppressions,
			}, nil
		}
		hooks.ListFiles = deps.engine.ListFiles
		hooks.ChangedFiles = deps.engine.ChangedFiles
		if lcp, ok := deps.engine.(lastCommitProvider); ok {
			hooks.LastCommitFiles = lcp.LastCommitFiles
		}
		if cfp, ok := deps.engine.(commitFilesProvider); ok {
			hooks.CommitFiles = cfp.CommitFiles
		}
		hooks.RunChecks = func(ctx context.Context, root string, jobs int, tasks []verify.CheckTask) ([]verify.CheckOutcome, error) {
			cliTasks := make([]checkTask, len(tasks))
			for i, t := range tasks {
				cliTasks[i] = checkTask{AdapterName: t.AdapterName, Check: t.Check}
			}
			outcomes, err := deps.engine.RunChecks(ctx, root, jobs, cliTasks)
			if err != nil {
				return nil, err
			}
			out := make([]verify.CheckOutcome, len(outcomes))
			for i, o := range outcomes {
				out[i] = verify.CheckOutcome{
					Task:   verify.CheckTask{AdapterName: o.Task.AdapterName, Check: o.Task.Check},
					Result: o.Result,
				}
			}
			return out, nil
		}
		hooks.ComputeEscapes = deps.engine.ComputeEscapes
		hooks.Ratchet = func(escapes, baseline map[string]int) ([]contract.Finding, []contract.MetricDelta) {
			return deps.engine.Ratchet(escapes, baseline), nil
		}
	}
	if deps.adapters != nil {
		hooks.DetectAdapters = deps.adapters.Detect
		hooks.PlanChecks = func(root string, checks []contract.Check, _ bool, _ []string) (verify.CheckPlan, error) {
			plan, err := deps.adapters.PlanChecks(root, checks)
			if err != nil {
				return verify.CheckPlan{}, err
			}
			return verify.CheckPlan{
				Runnable:       plan.Runnable,
				Skipped:        plan.Skipped,
				SkippedDetails: plan.SkippedDetails,
			}, nil
		}
		hooks.ParseFindings = deps.adapters.ParseFindings
	}
	if deps.ui != nil {
		hooks.RunUISmoke = func(ctx context.Context, root string) (contract.UISmokeResult, error) {
			return deps.ui.RunUISmoke(ctx, uiOptions{ProjectRoot: root})
		}
	}
	return hooks
}

// loadBaselineForVerify uses the richer baseline view when the engine provides
// it, and falls back to metric-only loading for simple implementations.
func loadBaselineForVerify(engine engineClient, path string) (baselineState, error) {
	if provider, ok := engine.(baselineProvider); ok {
		return provider.LoadBaselineState(path)
	}
	metrics, err := engine.LoadBaseline(path)
	if err != nil {
		return baselineState{}, err
	}
	return baselineState{Metrics: metrics}, nil
}

// newCommandContext prefers the engine's signal-aware context (Ctrl+C and
// SIGTERM) and falls back to os.Interrupt for simple test doubles.
func newCommandContext(engine engineClient) (context.Context, context.CancelFunc) {
	if provider, ok := engine.(signalContextProvider); ok {
		return provider.SignalContext(context.Background())
	}
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func writeResult(w io.Writer, asJSON bool, result contract.VerifyResult) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(result)
		return
	}
	printHumanResult(w, result)
}

func printHumanResult(w io.Writer, result contract.VerifyResult) {
	for _, f := range result.Findings {
		switch {
		case f.File != "" && f.Line > 0 && f.Column > 0:
			fmt.Fprintf(w, "%s:%d:%d: %s: %s\n", f.File, f.Line, f.Column, f.Rule, f.Message)
		case f.File != "" && f.Line > 0:
			fmt.Fprintf(w, "%s:%d: %s: %s\n", f.File, f.Line, f.Rule, f.Message)
		case f.File != "":
			fmt.Fprintf(w, "%s: %s: %s\n", f.File, f.Rule, f.Message)
		default:
			fmt.Fprintf(w, "%s: %s\n", f.Rule, f.Message)
		}
		if f.ActionHint != "" {
			fmt.Fprintf(w, "  action_hint: %s\n", f.ActionHint)
		}
	}

	if len(result.Escapes) > 0 {
		fmt.Fprintf(w, "escape hatches: %s\n", formatIntMap(result.Escapes))
	}
	if len(result.Improvements) > 0 {
		parts := make([]string, 0, len(result.Improvements))
		for _, imp := range result.Improvements {
			parts = append(parts, fmt.Sprintf("%s (%d)", imp.Name, imp.Delta))
		}
		fmt.Fprintf(w, "improvements: %s\n", strings.Join(parts, ", "))
	}
	if result.Hint != "" {
		fmt.Fprintf(w, "hint: %s\n", result.Hint)
	}
	if result.Suppressed > 0 {
		fmt.Fprintf(w, "suppressed files: %d\n", result.Suppressed)
	}
	if len(result.SkippedDetails) > 0 {
		for _, skipped := range result.SkippedDetails {
			switch {
			case skipped.Tool != "" && skipped.Reason != "":
				fmt.Fprintf(w, "skipped %s (%s): %s\n", skipped.ID, skipped.Tool, skipped.Reason)
			case skipped.Reason != "":
				fmt.Fprintf(w, "skipped %s: %s\n", skipped.ID, skipped.Reason)
			default:
				fmt.Fprintf(w, "skipped %s\n", skipped.ID)
			}
		}
	} else if len(result.Skipped) > 0 {
		fmt.Fprintf(w, "skipped checks: %s\n", strings.Join(result.Skipped, ", "))
	}
	if result.Ablation != nil {
		switch result.Ablation.Status {
		case "passed":
			fmt.Fprintf(w, "ablation: passed (red=%dms, green=%dms)\n", result.Ablation.RedDurationMs, result.Ablation.GreenDurationMs)
		case "skipped":
			fmt.Fprintf(w, "ablation: skipped (%s)\n", result.Ablation.Reason)
		default:
			fmt.Fprintf(w, "ablation: failed (%s)\n", result.Ablation.Reason)
		}
	}
	printUISmokeResult(w, result.UI)

	status := "PASS"
	if !result.Passed {
		status = "FAIL"
	}
	uiIssues := 0
	if result.UI != nil && !result.UI.Skipped {
		uiIssues = len(result.UI.Findings)
	}
	if uiIssues > 0 {
		fmt.Fprintf(w, "slopguard verify: %s (%d findings, %d ui issues, %dms)\n",
			status, len(result.Findings), uiIssues, result.DurationMs)
		return
	}
	fmt.Fprintf(w, "slopguard verify: %s (%d findings, %dms)\n",
		status, len(result.Findings), result.DurationMs)
}

// filterAdaptersByName applies an optional allowlist from config.yaml. An empty
// allowlist means all detected adapters are enabled.
func filterAdaptersByName(adapters []contract.Adapter, names []string) []contract.Adapter {
	if len(names) == 0 {
		return adapters
	}
	want := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			want[name] = struct{}{}
		}
	}
	out := make([]contract.Adapter, 0, len(adapters))
	for _, adapter := range adapters {
		if _, ok := want[adapter.Name()]; ok {
			out = append(out, adapter)
		}
	}
	return out
}

// filterSuppressedFiles splits files into kept and suppressed groups according
// to baseline.suppressed_files.
func filterSuppressedFiles(files []string, baseline baselineState) (kept, suppressed []string) {
	kept = make([]string, 0, len(files))
	for _, file := range files {
		if baseline.Suppresses(file) {
			suppressed = append(suppressed, file)
			continue
		}
		kept = append(kept, file)
	}
	return kept, suppressed
}

func collectExtensions(adapters []contract.Adapter) []string {
	set := make(map[string]struct{})
	for _, adapter := range adapters {
		for _, ext := range adapter.Extensions() {
			ext = strings.ToLower(strings.TrimSpace(ext))
			if ext == "" {
				continue
			}
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			set[ext] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for ext := range set {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

func collectEscapePatterns(adapters []contract.Adapter) []contract.EscapePattern {
	seen := make(map[string]struct{})
	var patterns []contract.EscapePattern
	for _, adapter := range adapters {
		for _, pattern := range adapter.EscapePatterns() {
			if pattern.Name == "" || pattern.Regex == "" {
				continue
			}
			if _, ok := seen[pattern.Name]; ok {
				continue
			}
			seen[pattern.Name] = struct{}{}
			patterns = append(patterns, pattern)
		}
	}
	sort.Slice(patterns, func(i, j int) bool { return patterns[i].Name < patterns[j].Name })
	return patterns
}

func formatIntMap(values map[string]int) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, values[key]))
	}
	return strings.Join(parts, ", ")
}
