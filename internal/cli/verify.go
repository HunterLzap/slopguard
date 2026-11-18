package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type verifyOptions struct {
	root   string
	all    bool
	json   bool
	jobs   int
	record bool
	paths  []string
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "scan all project files instead of only git-changed files")
	jsonOut := fs.Bool("json", false, "emit a structured contract.VerifyResult as JSON")
	jobs := fs.Int("jobs", 0, "maximum concurrent checks (default: config value or 2)")
	record := fs.Bool("record", false, "record a scan_runs row after a passing verify")
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
		root:   root,
		all:    *all,
		json:   *jsonOut,
		jobs:   *jobs,
		record: *record,
	}, stdout, stderr)
}

func runVerifyWith(opts verifyOptions, stdout, stderr io.Writer) int {
	started := time.Now()

	deps, err := newDependencies()
	if err != nil {
		return commandError(stderr, "prepare engine: %v", err)
	}

	jobs := opts.jobs
	cfg, cfgErr := loadConfig(opts.root)
	if cfgErr != nil && !errors.Is(cfgErr, os.ErrNotExist) {
		return commandError(stderr, "%v", cfgErr)
	}
	if jobs <= 0 {
		jobs = cfg.Verify.Jobs
	}
	if jobs <= 0 {
		jobs = 2
	}
	if jobs < 1 {
		return commandError(stderr, "--jobs must be >= 1")
	}

	detected, err := deps.adapters.Detect(opts.root)
	if err != nil {
		return commandError(stderr, "detect adapters: %v", err)
	}
	adapters := filterAdaptersByName(detected, cfg.Adapters)
	if len(adapters) == 0 {
		if len(cfg.Adapters) > 0 {
			return commandError(stderr, "none of the configured adapters %v apply to this project; run slopguard init", cfg.Adapters)
		}
		result := emptyResult(started)
		writeResult(stdout, opts.json, result)
		return ExitOK
	}

	patterns := collectEscapePatterns(adapters)
	baseline, err := loadBaselineForVerify(deps.engine, baselinePath(opts.root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if len(patterns) > 0 {
				return commandError(stderr, "%s not found; run slopguard init first", baselineRelPath)
			}
			baseline = baselineState{Metrics: map[string]int{}}
		} else {
			return commandError(stderr, "load baseline: %v", err)
		}
	}

	ctx, stop := newCommandContext(deps.engine)
	defer stop()

	extensions := collectExtensions(adapters)
	var files []string
	switch {
	case len(opts.paths) > 0:
		files = filterFilesByExtensions(resolveScopePaths(opts.root, opts.paths), extensions)
	case opts.all:
		files, err = deps.engine.ListFiles(opts.root, extensions)
	default:
		files, err = deps.engine.ChangedFiles(ctx, opts.root)
		if err == nil {
			files = filterFilesByExtensions(files, extensions)
		}
	}
	if err != nil {
		return commandError(stderr, "resolve scan scope: %v", err)
	}
	files, suppressedFromChecks := filterSuppressedFiles(files, baseline)

	var tasks []checkTask
	skippedIDs := make([]string, 0)
	skippedDetails := make([]contract.SkippedDetail, 0)
	for _, adapter := range adapters {
		subset := filterFilesByExtensions(files, adapter.Extensions())
		if len(subset) == 0 {
			continue
		}
		plan, err := deps.adapters.PlanChecks(opts.root, adapter.Checks(opts.root, subset))
		if err != nil {
			return commandError(stderr, "check toolchain: %v", err)
		}
		for _, skipped := range plan.Skipped {
			skippedIDs = append(skippedIDs, skipped)
		}
		if len(plan.SkippedDetails) > 0 {
			skippedDetails = append(skippedDetails, plan.SkippedDetails...)
		} else {
			for _, skipped := range plan.Skipped {
				skippedDetails = append(skippedDetails, contract.SkippedDetail{ID: skipped})
			}
		}
		for _, check := range plan.Runnable {
			if len(check.Cmd) == 0 {
				continue
			}
			tasks = append(tasks, checkTask{AdapterName: adapter.Name(), Check: check})
		}
	}

	var outcomes []checkOutcome
	if len(tasks) > 0 {
		outcomes, err = deps.engine.RunChecks(ctx, opts.root, jobs, tasks)
		if err != nil {
			return commandError(stderr, "run checks: %v", err)
		}
	}

	findings := make([]contract.Finding, 0)
	for _, outcome := range outcomes {
		result := outcome.Result
		if result.Err != nil {
			return commandError(stderr, "check %s: %v", outcome.Task.Check.ID, result.Err)
		}
		if result.Skipped {
			detail := contract.SkippedDetail{
				ID:     outcome.Task.Check.ID,
				Reason: "tool missing at execution time",
			}
			if len(outcome.Task.Check.Cmd) > 0 {
				detail.Tool = outcome.Task.Check.Cmd[0]
			}
			skippedIDs = append(skippedIDs, detail.ID)
			skippedDetails = append(skippedDetails, detail)
			continue
		}
		if result.ExitCode == 0 {
			continue
		}

		var parsed []contract.Finding
		if adapter := findAdapter(adapters, outcome.Task.AdapterName); adapter != nil {
			parsed = deps.adapters.ParseFindings(adapter, outcome.Task.Check.ID, result)
		}
		if len(parsed) == 0 {
			message := firstNonEmptyLine(result.Stderr, result.Stdout)
			if message == "" {
				message = fmt.Sprintf("check failed with exit code %d", result.ExitCode)
			}
			parsed = []contract.Finding{{
				Rule:     outcome.Task.Check.ID,
				Message:  message,
				Severity: contract.SeverityError,
			}}
		}
		findings = append(findings, parsed...)
	}

	escapes := map[string]int{}
	escapeFiles := files
	if !opts.all && (len(patterns) > 0 || baseline.HasSuppressions) {
		// The ratchet compares project-wide escape totals with the baseline, so
		// incremental mode still needs a full file list for counting. Only the
		// external lint/type checks are limited to changed files.
		escapeFiles, err = deps.engine.ListFiles(opts.root, extensions)
		if err != nil {
			return commandError(stderr, "list files for escape counting: %v", err)
		}
	}
	escapeFiles, suppressedFromEscapes := filterSuppressedFiles(escapeFiles, baseline)
	if len(patterns) > 0 && len(escapeFiles) > 0 {
		escapes, err = deps.engine.ComputeEscapes(opts.root, escapeFiles, patterns)
		if err != nil {
			return commandError(stderr, "count escape hatches: %v", err)
		}
	}
	if len(patterns) > 0 {
		findings = append(findings, deps.engine.Ratchet(escapes, baseline.Metrics)...)
	}

	result := contract.VerifyResult{
		Passed:         len(findings) == 0,
		Findings:       findings,
		Escapes:        escapes,
		Baseline:       baseline.Metrics,
		Suppressed:     countUniqueFiles(suppressedFromChecks, suppressedFromEscapes),
		DurationMs:     time.Since(started).Milliseconds(),
		Skipped:        skippedIDs,
		SkippedDetails: skippedDetails,
	}
	if result.Passed && opts.record && deps.recorder != nil {
		if err := deps.recorder.RecordScan(ctx, opts.root, result.DurationMs, sumEscapes(result.Escapes), true); err != nil {
			fmt.Fprintf(stderr, "slopguard: warning: record scan failed: %v\n", err)
		}
	}
	writeResult(stdout, opts.json, result)
	if result.Passed {
		return ExitOK
	}
	return ExitFindings
}

func emptyResult(started time.Time) contract.VerifyResult {
	return contract.VerifyResult{
		Passed:     true,
		Findings:   []contract.Finding{},
		Escapes:    map[string]int{},
		DurationMs: time.Since(started).Milliseconds(),
	}
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
	}

	if len(result.Escapes) > 0 {
		fmt.Fprintf(w, "escape hatches: %s\n", formatIntMap(result.Escapes))
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

	status := "PASS"
	if !result.Passed {
		status = "FAIL"
	}
	fmt.Fprintf(w, "slopguard verify: %s (%d findings, %dms)\n",
		status, len(result.Findings), result.DurationMs)
}

func firstNonEmptyLine(values ...string) string {
	for _, value := range values {
		for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				if len(line) > 300 {
					line = line[:300] + "..."
				}
				return line
			}
		}
	}
	return ""
}

func findAdapter(adapters []contract.Adapter, name string) contract.Adapter {
	for _, adapter := range adapters {
		if adapter.Name() == name {
			return adapter
		}
	}
	return nil
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

func countUniqueFiles(groups ...[]string) int {
	seen := make(map[string]struct{})
	for _, group := range groups {
		for _, file := range group {
			seen[file] = struct{}{}
		}
	}
	return len(seen)
}

func sumEscapes(escapes map[string]int) int {
	total := 0
	for _, count := range escapes {
		total += count
	}
	return total
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

func filterFilesByExtensions(files []string, extensions []string) []string {
	set := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		set[strings.ToLower(ext)] = struct{}{}
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		if _, ok := set[strings.ToLower(filepath.Ext(file))]; ok {
			out = append(out, file)
		}
	}
	sort.Strings(out)
	return out
}

// resolveScopePaths normalizes explicit file paths (relative or absolute) into
// repository-relative paths suitable for engine checks and escape counting.
func resolveScopePaths(root string, paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		file := raw
		if filepath.IsAbs(file) {
			rel, err := filepath.Rel(root, file)
			if err != nil {
				continue
			}
			file = rel
		} else {
			file = filepath.Clean(filepath.FromSlash(file))
		}
		if file == "." || filepath.IsAbs(file) || strings.HasPrefix(file, ".."+string(filepath.Separator)) {
			continue
		}
		info, err := os.Stat(filepath.Join(root, file))
		if err != nil || info.IsDir() {
			continue
		}
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		out = append(out, file)
	}
	sort.Strings(out)
	return out
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
