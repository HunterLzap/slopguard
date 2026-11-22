package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core/adapters"
)

type Engine struct {
	Runner ProcessRunner
}

func ProjectPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	stat, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !stat.IsDir() {
		return "", fmt.Errorf("project path %q is not a directory", real)
	}
	return real, nil
}

func DetectLanguages(project string) ([]string, error) {
	all, err := treeFiles(project)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, file := range all {
		if language, ok := adapters.Match(file); ok && !slices.Contains(ids, language.ID) {
			ids = append(ids, language.ID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (e Engine) Verify(ctx context.Context, path string, scope contract.VerifyScope) (result contract.VerifyResult, err error) {
	start := time.Now()
	defer func() { result.DurationMs = time.Since(start).Milliseconds() }()
	result = contract.VerifyResult{
		Scope: scope, ExitCode: 2, DetectedLanguages: []string{},
		ExecutedCheckers: []string{}, MissingTools: []contract.ToolRequirement{},
		Diagnostics: []contract.Diagnostic{}, Warnings: []string{},
	}
	project, err := ProjectPath(path)
	if err != nil {
		return result, err
	}
	result.ProjectPath = project
	var selected []string
	switch scope {
	case contract.ScopeChanged:
		selected, err = ChangedFiles(ctx, e.Runner, project)
	case contract.ScopeAll:
		selected, err = treeFiles(project)
	default:
		return result, fmt.Errorf("invalid scope %q; use changed or all", scope)
	}
	if err != nil {
		return result, err
	}
	if scope == contract.ScopeChanged && len(selected) == 0 {
		result.Status, result.ExitCode = contract.NoChanges, 0
		return result, nil
	}
	targets, err := groupTargets(project, selected)
	if err != nil {
		return result, err
	}
	checked := e.runTargets(ctx, project, targets)
	scanned := map[string]bool{}
	var checkErrors error
	for i, t := range targets {
		if !slices.Contains(result.DetectedLanguages, t.language.ID) {
			result.DetectedLanguages = append(result.DetectedLanguages, t.language.ID)
		}
		c := checked[i]
		for _, name := range c.executed {
			addUnique(&result.ExecutedCheckers, name)
		}
		for _, tool := range c.missing {
			addUnique(&result.MissingTools, tool)
		}
		for _, file := range c.files {
			scanned[file] = true
		}
		result.Diagnostics = append(result.Diagnostics, c.diagnostics...)
		result.Warnings = append(result.Warnings, c.warnings...)
		checkErrors = errors.Join(checkErrors, c.err)
	}
	slices.Sort(result.DetectedLanguages)
	custom, customErr := (Semgrep{Runner: e.Runner}).RunCustomRules(ctx, project, scanFiles(project, selected))
	checkErrors = errors.Join(checkErrors, customErr)
	if custom.Missing {
		addUnique(&result.MissingTools, SemgrepTool)
	}
	for _, file := range custom.ScannedFiles {
		scanned[file] = true
	}
	if len(custom.ScannedFiles) > 0 {
		addUnique(&result.ExecutedCheckers, SemgrepTool.Name)
	}
	result.Diagnostics = append(result.Diagnostics, custom.Diagnostics...)
	result.ScannedFilesCount = len(scanned)
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	failed := slices.ContainsFunc(result.Diagnostics, func(d contract.Diagnostic) bool { return d.Severity == "error" })
	switch {
	case failed:
		result.Status, result.ExitCode = contract.Fail, 1
		if checkErrors != nil {
			result.Warnings = append(result.Warnings, checkErrors.Error())
		}
	case custom.Missing:
		result.Status = contract.MissingTool
		if checkErrors != nil {
			result.Warnings = append(result.Warnings, checkErrors.Error())
		}
	case checkErrors != nil:
		return result, checkErrors
	case result.ScannedFilesCount > 0:
		result.Status, result.ExitCode = contract.Pass, 0
	case len(result.MissingTools) > 0:
		result.Status = contract.MissingTool
	case len(targets) == 0 && !custom.Configured:
		result.Status = contract.Unsupported
	default:
		return result, fmt.Errorf("no checker scanned project files")
	}
	return result, nil
}

func addUnique[T comparable](items *[]T, item T) {
	if !slices.Contains(*items, item) {
		*items = append(*items, item)
	}
}

type target struct {
	language adapters.Language
	root     string
	files    []string
}

func groupTargets(project string, selected []string) ([]target, error) {
	type key struct{ language, root string }
	indices := make(map[key]int)
	var targets []target
	var all []string
	allLoaded := false
	for _, file := range selected {
		language, ok := adapters.Match(file)
		if !ok {
			continue
		}
		root := language.Root(project, file)
		k := key{language.ID, root}
		i, exists := indices[k]
		if !exists {
			i = len(targets)
			indices[k] = i
			targets = append(targets, target{language: language, root: root})
		}
		if language.Source(file) && adapters.Regular(filepath.Join(project, file)) {
			targets[i].files = append(targets[i].files, file)
		}
	}
	for i, t := range targets {
		if len(t.files) > 0 {
			continue
		}
		// A manifest change or deletion may require a package scan even if no
		// source file remains in the changed paths.
		if !allLoaded {
			var err error
			all, err = treeFiles(project)
			if err != nil {
				return nil, err
			}
			allLoaded = true
		}
		for _, file := range all {
			if t.language.Source(file) && t.language.Root(project, file) == t.root {
				targets[i].files = append(targets[i].files, file)
			}
		}
	}
	return targets, nil
}

type checkReport struct {
	executed, files, warnings []string
	missing                   []contract.ToolRequirement
	diagnostics               []contract.Diagnostic
	err                       error
}

func (e Engine) runTargets(ctx context.Context, project string, targets []target) []checkReport {
	results := make([]checkReport, len(targets))
	limit := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
				results[i] = e.checkTarget(ctx, project, t)
			case <-ctx.Done():
				results[i].err = ctx.Err()
			}
		}()
	}
	wg.Wait()
	return results
}

func (e Engine) checkTarget(ctx context.Context, project string, t target) (report checkReport) {
	for _, alternatives := range t.language.Groups {
		var missing []contract.ToolRequirement
		for _, tool := range alternatives {
			files, perFile := toolFiles(t, tool)
			if tool.Input == adapters.Kotlin && len(files) == 0 {
				continue
			}
			bin, err := e.findTool(t.root, tool)
			if err != nil {
				if tool.Optional {
					report.warnings = append(report.warnings, fmt.Sprintf("optional %s unavailable; install via %s", tool.Name, tool.Install))
				} else {
					missing = append(missing, contract.ToolRequirement{Name: tool.Name, InstallCommand: tool.Install})
				}
				continue
			}
			missing = nil
			if len(files) == 0 {
				break
			}
			batches := [][]string{nil}
			if perFile {
				batches = fileBatches(project, files)
			}
			for _, batch := range batches {
				args := slices.Clone(tool.Args)
				if tool.Input == adapters.TypeScript && perFile {
					args = append(args, "--allowJs", "--checkJs")
				}
				r, err := e.Runner.Run(ctx, t.root, bin, append(args, batch...)...)
				if err == nil {
					err = collectOutput(&report, project, t, tool, r, files, batch)
				}
				if err != nil {
					report.err = errors.Join(report.err, fmt.Errorf("%s in %s: %w", tool.Name, t.root, err))
				}
			}
			break
		}
		report.missing = append(report.missing, missing...)
	}
	return report
}

func toolFiles(t target, tool adapters.Tool) ([]string, bool) {
	switch tool.Input {
	case adapters.Files:
		return t.files, true
	case adapters.TypeScript:
		return t.files, !adapters.Regular(filepath.Join(t.root, "tsconfig.json"))
	case adapters.Kotlin:
		var kotlin []string
		for _, file := range t.files {
			if ext := strings.ToLower(filepath.Ext(file)); ext == ".kt" || ext == ".kts" {
				kotlin = append(kotlin, file)
			}
		}
		if len(kotlin) < len(t.files) {
			return nil, true
		}
		return kotlin, true
	default:
		return t.files, false
	}
}

func (e Engine) findTool(root string, tool adapters.Tool) (string, error) {
	if tool.Name == "gradlew" {
		name := tool.Name
		if runtime.GOOS == "windows" {
			name += ".bat"
		}
		return e.Runner.LookPath(filepath.Join(root, name))
	}
	if tool.LocalBin {
		local := filepath.Join(root, "node_modules", ".bin", tool.Name)
		if bin, err := e.Runner.LookPath(local); err == nil {
			return bin, nil
		}
	}
	return e.Runner.LookPath(tool.Name)
}

func collectOutput(report *checkReport, project string, t target, tool adapters.Tool, output ProcessResult, files, batch []string) error {
	diagnostics, err := tool.Parse(output.Stdout, output.Stderr)
	if err != nil {
		return fmt.Errorf("%w (%s)", err, processMessage(output))
	}
	failed := false
	ignored := make(map[string]bool)
	for _, d := range diagnostics {
		d.Language, d.Checker = t.language.ID, tool.Name
		d.Severity = strings.ToLower(d.Severity)
		if d.Severity == "fatal" || (output.ExitCode == 1 && d.Severity == "warning" && tool.Name == "cppcheck") {
			d.Severity = "error"
		}
		if d.File != "" {
			absolute := d.File
			if !filepath.IsAbs(absolute) {
				absolute = filepath.Join(t.root, filepath.FromSlash(absolute))
			}
			if rel, ok := relativeFile(project, absolute); ok {
				d.File, d.CodeSnippet = rel, snippet(project, rel, d.Line)
				if strings.HasPrefix(d.Message, "File ignored") {
					ignored[rel] = true
				}
			} else {
				d.File = ""
			}
		}
		failed = failed || d.Severity == "error"
		report.diagnostics = append(report.diagnostics, d)
	}
	if output.ExitCode != 0 && !failed {
		return fmt.Errorf("checker exited %d: %s", output.ExitCode, processMessage(output))
	}
	addUnique(&report.executed, tool.Name)
	text := strings.ToLower(output.Stderr + "\n" + output.Stdout)
	if strings.Contains(text, "matched no packages") || strings.Contains(text, "no files were processed") || strings.Contains(text, "no python files are present") {
		report.warnings = append(report.warnings, fmt.Sprintf("%s did not scan any files", tool.Name))
		return nil
	}
	if batch != nil {
		files = nil
		for _, path := range batch {
			if rel, ok := relativeFile(project, path); ok {
				files = append(files, rel)
			}
		}
	}
	for _, file := range files {
		if !ignored[file] {
			report.files = append(report.files, file)
		}
	}
	return nil
}

func ignoredDirectory(name string) bool {
	switch name {
	case ".git", ProjectDirectory, "node_modules", ".venv", ".dart_tool", ".gradle", "__pycache__":
		return true
	}
	return false
}

func treeFiles(project string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(project, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != project && ignoredDirectory(d.Name()) {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			if rel, ok := relativeFile(project, path); ok {
				files = append(files, rel)
			}
		}
		return nil
	})
	return files, err
}

func snippet(project, file string, line int) string {
	if line < 1 {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(project, file))
	if err != nil {
		return ""
	}
	if _, ok := relativeFile(project, resolved); !ok {
		return ""
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return ""
	}
	lines := bytes.Split(data, []byte{'\n'})
	if line > len(lines) {
		return ""
	}
	return strings.TrimSuffix(normalizeOutput(string(bytes.Join(lines[max(line-11, 0):min(line+10, len(lines))], []byte{'\n'}))), "\r")
}
