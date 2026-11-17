package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// baselineDocument mirrors .slopguard/baseline.json for previews and writes.
type baselineDocument struct {
	Version         int            `json:"version"`
	UpdatedAt       time.Time      `json:"updated_at"`
	Metrics         map[string]int `json:"metrics"`
	SuppressedFiles []string       `json:"suppressed_files"`
}

// baselineAdmin is an optional engine capability used by the baseline command.
// It is optional so existing test doubles for init/verify stay small.
type baselineAdmin interface {
	LoadBaselineDocument(path string) (baselineDocument, error)
	TightenBaseline(path string, current map[string]int) (baselineDocument, error)
	ResetBaseline(path string, current map[string]int) (baselineDocument, error)
	SaveBaselineDocument(path string, doc baselineDocument) error
}

func runBaseline(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("baseline", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tighten := fs.Bool("tighten", false, "tighten existing baseline metrics to current values (only decreases)")
	reset := fs.Bool("reset", false, "reset baseline metrics to current values")
	yes := fs.Bool("yes", false, "apply changes without prompting")
	rootFlag := fs.String("root", ".", "project root directory")
	help, err := parseFlags(fs, args)
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	if help {
		return ExitOK
	}
	if *tighten && *reset {
		return commandError(stderr, "--tighten and --reset are mutually exclusive")
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

	path := baselinePath(root)
	if !*tighten && !*reset {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return commandError(stderr, "%s not found; run slopguard init first", baselineRelPath)
			}
			return commandError(stderr, "read %s: %v", baselineRelPath, err)
		}
		fmt.Fprint(stdout, string(data))
		if len(data) > 0 && data[len(data)-1] != '\n' {
			fmt.Fprintln(stdout)
		}
		return ExitOK
	}

	deps, err := newDependencies()
	if err != nil {
		return commandError(stderr, "prepare engine: %v", err)
	}
	admin, ok := deps.engine.(baselineAdmin)
	if !ok {
		return commandError(stderr, "engine does not support baseline administration")
	}

	cfg, cfgErr := loadConfig(root)
	if cfgErr != nil && !errors.Is(cfgErr, os.ErrNotExist) {
		return commandError(stderr, "%v", cfgErr)
	}
	detected, err := deps.adapters.Detect(root)
	if err != nil {
		return commandError(stderr, "detect adapters: %v", err)
	}
	adapters := filterAdaptersByName(detected, cfg.Adapters)
	baseline, baselineErr := loadBaselineForVerify(deps.engine, path)
	if baselineErr != nil && !errors.Is(baselineErr, os.ErrNotExist) {
		return commandError(stderr, "load baseline: %v", baselineErr)
	}
	if errors.Is(baselineErr, os.ErrNotExist) {
		baseline = baselineState{Metrics: map[string]int{}}
	}
	current := map[string]int{}
	patterns := collectEscapePatterns(adapters)
	if len(patterns) > 0 {
		files, err := deps.engine.ListFiles(root, collectExtensions(adapters))
		if err != nil {
			return commandError(stderr, "list project files: %v", err)
		}
		files, _ = filterSuppressedFiles(files, baseline)
		current, err = deps.engine.ComputeEscapes(root, files, patterns)
		if err != nil {
			return commandError(stderr, "count escape hatches: %v", err)
		}
	}

	var proposed baselineDocument
	if *tighten {
		proposed, err = admin.TightenBaseline(path, current)
	} else {
		proposed, err = admin.ResetBaseline(path, current)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return commandError(stderr, "%s not found; run slopguard init first", baselineRelPath)
		}
		return commandError(stderr, "prepare baseline update: %v", err)
	}
	proposedJSON, err := json.MarshalIndent(proposed, "", "  ")
	if err != nil {
		return commandError(stderr, "render baseline: %v", err)
	}
	proposedJSON = append(proposedJSON, '\n')

	oldJSON, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return commandError(stderr, "read %s: %v", baselineRelPath, readErr)
	}
	if len(oldJSON) > 0 && bytes.Equal(normalizeText(oldJSON), normalizeText(proposedJSON)) {
		fmt.Fprintln(stdout, "baseline is already up to date.")
		return ExitOK
	}
	fmt.Fprint(stdout, formatDiff(baselineRelPath, oldJSON, proposedJSON))

	apply := *yes
	if !apply {
		action := "tighten"
		if *reset {
			action = "reset"
		}
		fmt.Fprintf(stdout, "Apply this baseline %s? [y/N] ", action)
		apply = confirm(stdin)
	}
	if !apply {
		fmt.Fprintln(stdout, "No changes made.")
		return ExitOK
	}
	if err := admin.SaveBaselineDocument(path, proposed); err != nil {
		return commandError(stderr, "write %s: %v", baselineRelPath, err)
	}
	fmt.Fprintf(stdout, "Updated %s (%s)\n", baselineRelPath, formatBaselineMetrics(proposed.Metrics))
	return ExitOK
}

func formatBaselineMetrics(metrics map[string]int) string {
	if len(metrics) == 0 {
		return "no metrics"
	}
	keys := make([]string, 0, len(metrics))
	for key := range metrics {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, metrics[key]))
	}
	return strings.Join(parts, ", ")
}
