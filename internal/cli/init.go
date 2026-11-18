package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// featureFiles are the repo markers init reports while detecting a stack.
var featureFiles = []string{
	"package.json",
	"tsconfig.json",
	"jsconfig.json",
	"pyproject.toml",
	"requirements.txt",
	"setup.py",
	"setup.cfg",
	"Pipfile",
	"poetry.lock",
	"uv.lock",
	"go.mod",
	"Cargo.toml",
	"pom.xml",
	"build.gradle",
	".eslintrc",
	".eslintrc.json",
	".eslintrc.js",
	"eslint.config.js",
	"eslint.config.mjs",
	"eslint.config.cjs",
	"ruff.toml",
	".ruff.toml",
	"pyrightconfig.json",
}

func runInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "apply changes to an existing config file without prompting")
	presets := fs.Bool("presets", false, "merge global rule presets into project config")
	ui := fs.Bool("ui", false, "scaffold Playwright UI smoke configuration")
	rootFlag := fs.String("root", ".", "project root directory")
	help, err := parseFlags(fs, args)
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	if help {
		return ExitOK
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

	deps, err := newDependencies()
	if err != nil {
		return commandError(stderr, "prepare engine: %v", err)
	}

	adapters, err := deps.adapters.Detect(root)
	if err != nil {
		return commandError(stderr, "detect adapters: %v", err)
	}
	names := adapterNames(adapters)

	fmt.Fprintf(stdout, "Scanning %s\n", root)
	if features := scanFeatures(root); len(features) > 0 {
		fmt.Fprintf(stdout, "Feature files: %s\n", strings.Join(features, ", "))
	}
	if len(names) > 0 {
		fmt.Fprintf(stdout, "Applicable adapters: %s\n", strings.Join(names, ", "))
	} else {
		fmt.Fprintln(stdout, "Applicable adapters: none (no TypeScript/Python project detected)")
	}

	// --- .slopguard/config.yaml -------------------------------------------------
	rendered, err := renderConfig(names)
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	cfgPath := configPath(root)
	oldConfig, readErr := os.ReadFile(cfgPath)
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		if err := writeFileAtomic(cfgPath, rendered, 0o644); err != nil {
			return commandError(stderr, "write %s: %v", configRelPath, err)
		}
		fmt.Fprintf(stdout, "Created %s\n", configRelPath)
	case readErr != nil:
		return commandError(stderr, "read %s: %v", configRelPath, readErr)
	case bytes.Equal(normalizeText(oldConfig), normalizeText(rendered)):
		fmt.Fprintf(stdout, "%s is already up to date.\n", configRelPath)
	default:
		fmt.Fprint(stdout, formatDiff(configRelPath, oldConfig, rendered))
		apply := *yes
		if !apply {
			fmt.Fprintf(stdout, "%s differs from the generated default. Apply this change? [y/N] ", configRelPath)
			apply = confirm(stdin)
		}
		if !apply {
			fmt.Fprintf(stdout, "No changes made to %s. Re-run with --yes to apply.\n", configRelPath)
		} else if err := writeFileAtomic(cfgPath, rendered, 0o644); err != nil {
			return commandError(stderr, "write %s: %v", configRelPath, err)
		} else {
			fmt.Fprintf(stdout, "Updated %s\n", configRelPath)
		}
	}

	// --- .slopguard/baseline.json -----------------------------------------------
	basePath := baselinePath(root)
	if _, err := os.Stat(basePath); err == nil {
		if _, err := deps.engine.LoadBaseline(basePath); err != nil {
			return commandError(stderr, "existing %s is invalid: %v", baselineRelPath, err)
		}
		fmt.Fprintf(stdout, "Kept existing %s\n", baselineRelPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return commandError(stderr, "stat %s: %v", baselineRelPath, err)
	} else {
		metrics := map[string]int{}
		patterns := collectEscapePatterns(adapters)
		if len(patterns) > 0 {
			files, err := deps.engine.ListFiles(root, collectExtensions(adapters))
			if err != nil {
				return commandError(stderr, "list project files: %v", err)
			}
			metrics, err = deps.engine.ComputeEscapes(root, files, patterns)
			if err != nil {
				return commandError(stderr, "compute initial baseline: %v", err)
			}
		}
		if err := deps.engine.SaveBaseline(basePath, metrics); err != nil {
			return commandError(stderr, "write %s: %v", baselineRelPath, err)
		}
		fmt.Fprintf(stdout, "Created %s (%s)\n", baselineRelPath, formatMetrics(metrics))
	}

	if *presets {
		if code := runPresetSync(deps, root, *yes, stdin, stdout, stderr); code != ExitOK {
			return code
		}
	}
	if *ui {
		if code := runUIScaffold(root, stdout, stderr); code != ExitOK {
			return code
		}
	}

	fmt.Fprintln(stdout, "init complete.")
	return ExitOK
}

func scanFeatures(root string) []string {
	var found []string
	for _, name := range featureFiles {
		if info, err := os.Stat(filepath.Join(root, name)); err == nil && !info.IsDir() {
			found = append(found, name)
		}
	}
	return found
}

func adapterNames(adapters []contract.Adapter) []string {
	names := make([]string, 0, len(adapters))
	for _, a := range adapters {
		names = append(names, a.Name())
	}
	sort.Strings(names)
	return names
}

func formatMetrics(metrics map[string]int) string {
	if len(metrics) == 0 {
		return "no escape hatches found"
	}
	keys := make([]string, 0, len(metrics))
	for k := range metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, metrics[k]))
	}
	return strings.Join(parts, ", ")
}
