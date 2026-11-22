package core

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

func ChangedFiles(ctx context.Context, runner ProcessRunner, project string) ([]string, error) {
	git, err := runner.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git is required for --scope changed; use --scope all: %w", err)
	}
	root, err := runner.Run(ctx, project, git, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if root.ExitCode != 0 {
		return nil, fmt.Errorf("%s is not a Git worktree; use --scope all (%s)", project, processMessage(root))
	}
	repo := filepath.FromSlash(strings.TrimSuffix(root.Stdout, "\n"))
	result, err := runner.Run(ctx, project, git, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".")
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("git status: %s", processMessage(result))
	}
	var files []string
	seen := map[string]bool{}
	records := strings.Split(strings.TrimSuffix(result.Stdout, "\x00"), "\x00")
	for i := 0; i < len(records); i++ {
		entry := records[i]
		if entry == "" {
			continue
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return nil, fmt.Errorf("invalid Git status entry %q", entry)
		}
		if strings.ContainsAny(entry[:2], "RC") {
			i++
			if i >= len(records) || records[i] == "" {
				return nil, fmt.Errorf("missing Git rename source for %q", entry)
			}
		}
		rel, ok := relativeFile(project, filepath.Join(repo, filepath.FromSlash(entry[3:])))
		if ok && !seen[rel] {
			files = append(files, rel)
			seen[rel] = true
		}
	}
	slices.Sort(files)
	return files, nil
}

func processMessage(r ProcessResult) string {
	msg := strings.TrimSpace(r.Stderr + "\n" + r.Stdout)
	if msg == "" {
		return fmt.Sprintf("exit code %d", r.ExitCode)
	}
	return msg
}

func relativeFile(root, path string) (string, bool) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	rel, err := filepath.Rel(root, path)
	return rel, err == nil && filepath.IsLocal(rel) && rel != "."
}
