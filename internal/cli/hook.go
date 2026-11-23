package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	preCommitHookStartMarker = "# >>> slopguard pre-commit >>>"
	preCommitHookEndMarker   = "# <<< slopguard pre-commit <<<"
)

type hookEnv struct {
	executable func() (string, error)
	lookPath   func(string) (string, error)
}

var currentHookEnv = hookEnv{
	executable: os.Executable,
	lookPath:   exec.LookPath,
}

func runHook(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHookUsage(stderr)
		return ExitError
	}
	sub := args[0]
	if sub == "-h" || sub == "--help" || sub == "help" {
		printHookUsage(stdout)
		return ExitOK
	}

	fs := flag.NewFlagSet("hook "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	preCommit := fs.Bool("pre-commit", true, "manage the blocking Git pre-commit hook")
	rootFlag := fs.String("root", ".", "project root directory")
	help, err := parseFlags(fs, args[1:])
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	if help {
		return ExitOK
	}
	if !*preCommit {
		return commandError(stderr, "only --pre-commit is supported")
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

	switch sub {
	case "install":
		return runHookInstall(currentHookEnv, root, stdout, stderr)
	case "uninstall":
		return runHookUninstall(root, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "slopguard: unknown hook subcommand %q\n", sub)
		printHookUsage(stderr)
		return ExitError
	}
}

func printHookUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  slopguard hook install   [--pre-commit] [--root DIR]
  slopguard hook uninstall [--pre-commit] [--root DIR]
`)
}

func runHookInstall(env hookEnv, root string, stdout, stderr io.Writer) int {
	hookPath, err := resolvePreCommitHookPath(root)
	if err != nil {
		return commandError(stderr, "%v", err)
	}

	bin := resolveHookBinary(env)
	block := buildPreCommitHookBlock(bin)

	var existing string
	mode := os.FileMode(0o755)
	if data, readErr := os.ReadFile(hookPath); readErr == nil {
		existing = string(data)
		if info, statErr := os.Stat(hookPath); statErr == nil && info.Mode().Perm() != 0 {
			mode = info.Mode().Perm() | 0o111
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return commandError(stderr, "read %s: %v", hookPath, readErr)
	}

	updated := upsertPreCommitHookBlock(existing, block)
	if err := writeFileAtomic(hookPath, []byte(updated), mode); err != nil {
		return commandError(stderr, "write %s: %v", hookPath, err)
	}
	fmt.Fprintf(stdout, "Installed blocking SlopGuard pre-commit hook: %s\n", hookPath)
	return ExitOK
}

func runHookUninstall(root string, stdout, stderr io.Writer) int {
	hookPath, err := resolvePreCommitHookPath(root)
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stdout, "SlopGuard pre-commit hook is not installed (%s)\n", hookPath)
			return ExitOK
		}
		return commandError(stderr, "read %s: %v", hookPath, err)
	}

	text := string(data)
	if !strings.Contains(text, preCommitHookStartMarker) || !strings.Contains(text, preCommitHookEndMarker) {
		fmt.Fprintf(stdout, "SlopGuard pre-commit hook block not found in %s (kept existing file untouched)\n", hookPath)
		return ExitOK
	}

	stripped := stripPreCommitHookBlock(text)
	if isEffectivelyEmptyHook(stripped) {
		if err := os.Remove(hookPath); err != nil && !os.IsNotExist(err) {
			return commandError(stderr, "remove %s: %v", hookPath, err)
		}
		fmt.Fprintf(stdout, "Removed SlopGuard pre-commit hook: %s\n", hookPath)
		return ExitOK
	}

	mode := os.FileMode(0o755)
	if info, statErr := os.Stat(hookPath); statErr == nil && info.Mode().Perm() != 0 {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(hookPath, []byte(stripped), mode); err != nil {
		return commandError(stderr, "update %s: %v", hookPath, err)
	}
	fmt.Fprintf(stdout, "Removed SlopGuard pre-commit block (preserved existing custom hook): %s\n", hookPath)
	return ExitOK
}

func resolvePreCommitHookPath(root string) (string, error) {
	checkCmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	checkCmd.Dir = root
	out, err := checkCmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return "", fmt.Errorf("not a git repository: %s", root)
	}

	pathCmd := exec.Command("git", "rev-parse", "--git-path", "hooks/pre-commit")
	pathCmd.Dir = root
	pathOut, err := pathCmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve pre-commit hook path: %w", err)
	}
	hookPath := strings.TrimSpace(string(pathOut))
	if hookPath == "" {
		return "", errors.New("empty pre-commit hook path")
	}
	if !filepath.IsAbs(hookPath) {
		hookPath = filepath.Join(root, hookPath)
	}
	return filepath.Clean(hookPath), nil
}

func resolveHookBinary(env hookEnv) string {
	if env.executable != nil {
		if exe, err := env.executable(); err == nil && strings.TrimSpace(exe) != "" {
			base := strings.ToLower(filepath.Base(exe))
			if !strings.HasSuffix(base, ".test") && !strings.HasSuffix(base, ".test.exe") {
				return exe
			}
		}
	}
	if env.lookPath != nil {
		if p, err := env.lookPath("slopguard"); err == nil && strings.TrimSpace(p) != "" {
			return p
		}
	}
	return "slopguard"
}

func buildPreCommitHookBlock(binaryPath string) string {
	quotedExe := "'" + strings.ReplaceAll(binaryPath, "'", "'\"'\"'") + "'"
	return strings.Join([]string{
		preCommitHookStartMarker,
		`export PATH="$PATH:/opt/homebrew/bin:/usr/local/bin:$HOME/.local/bin:$HOME/go/bin"`,
		fmt.Sprintf("_sg_bin=%s", quotedExe),
		`if [ ! -x "$_sg_bin" ]; then`,
		`  _sg_bin="$(command -v slopguard 2>/dev/null || true)"`,
		`fi`,
		`if [ -z "$_sg_bin" ]; then`,
		`  echo "slopguard: binary not found; aborting commit" >&2`,
		`  exit 1`,
		`fi`,
		`_sg_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"`,
		`"$_sg_bin" verify --staged --root "$_sg_root" || exit $?`,
		preCommitHookEndMarker,
	}, "\n")
}

func upsertPreCommitHookBlock(existing, block string) string {
	normalized := strings.ReplaceAll(existing, "\r\n", "\n")
	start := strings.Index(normalized, preCommitHookStartMarker)
	end := strings.Index(normalized, preCommitHookEndMarker)
	if start >= 0 && end > start {
		afterEnd := end + len(preCommitHookEndMarker)
		return normalized[:start] + block + normalized[afterEnd:]
	}
	if strings.TrimSpace(normalized) == "" {
		return "#!/bin/sh\n\n" + block + "\n"
	}
	if !strings.HasSuffix(normalized, "\n") {
		normalized += "\n"
	}
	return normalized + "\n" + block + "\n"
}

func stripPreCommitHookBlock(existing string) string {
	normalized := strings.ReplaceAll(existing, "\r\n", "\n")
	start := strings.Index(normalized, preCommitHookStartMarker)
	end := strings.Index(normalized, preCommitHookEndMarker)
	if start < 0 || end < start {
		return normalized
	}
	afterEnd := end + len(preCommitHookEndMarker)
	if afterEnd < len(normalized) && normalized[afterEnd] == '\n' {
		afterEnd++
	}
	prefix := normalized[:start]
	suffix := normalized[afterEnd:]
	if strings.HasSuffix(prefix, "\n\n") && (suffix == "" || strings.HasPrefix(suffix, "\n")) {
		prefix = strings.TrimSuffix(prefix, "\n")
	}
	return prefix + suffix
}

func isEffectivelyEmptyHook(content string) bool {
	trimmed := strings.TrimSpace(content)
	switch trimmed {
	case "", "#!/bin/sh", "#!/usr/bin/env sh", "#!/bin/bash", "#!/usr/bin/env bash":
		return true
	default:
		return false
	}
}
