package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/HunterLzap/slopguard/internal/core"
)

const (
	hookStart    = "# >>> SLOPGUARD HOOK START >>>"
	hookEnd      = "# <<< SLOPGUARD HOOK END <<<"
	hookMetadata = "# slopguard original: "
	guardCommand = "slopguard verify --scope changed || exit $?"
)

func (s *Service) SetHook(ctx context.Context, path string, enabled bool) error {
	project, err := root(path)
	if err != nil {
		return err
	}
	hook, err := hookPath(ctx, s.engine.Runner, project)
	if err != nil {
		return err
	}
	undo, err := editHook(hook, enabled)
	if err != nil {
		return err
	}
	if err := s.reviewer.Store.SetHookEnabled(ctx, project, enabled); err != nil {
		if undo != nil {
			err = errors.Join(err, undo())
		}
		return err
	}
	return nil
}

func hookPath(ctx context.Context, runner core.ProcessRunner, project string) (string, error) {
	git, err := runner.LookPath("git")
	if err != nil {
		return "", err
	}
	gitPath := func(args ...string) (string, error) {
		r, err := runner.Run(ctx, project, git, append([]string{"rev-parse"}, args...)...)
		if err != nil {
			return "", err
		}
		if r.ExitCode != 0 {
			return "", fmt.Errorf("git rev-parse failed: %s", strings.TrimSpace(r.Stderr))
		}
		return filepath.FromSlash(strings.TrimSuffix(r.Stdout, "\n")), nil
	}
	worktree, err := gitPath("--show-toplevel")
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(project, worktree); err != nil || rel != "." {
		return "", fmt.Errorf("install or remove the hook from the Git worktree root")
	}
	path, err := gitPath("--git-path", "hooks/pre-commit")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(project, path)
	}
	if _, err := containedPath(project, path); err != nil {
		return "", err
	}
	return path, nil
}

func containedPath(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return "", fmt.Errorf("path %s is outside %s", path, root)
	}
	// Walk ancestors so a symlink cannot redirect a write to a shared hook or config.
	for part := filepath.Dir(rel); part != "."; part = filepath.Dir(part) {
		dir := filepath.Join(root, part)
		info, err := os.Lstat(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err == nil && !info.IsDir() {
			return "", fmt.Errorf("%s must be a directory", dir)
		}
	}
	return rel, nil
}

type fileState struct {
	data   []byte
	mode   fs.FileMode
	exists bool
}

func readFile(path string) (fileState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{mode: 0600}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	if !info.Mode().IsRegular() {
		return fileState{}, fmt.Errorf("%s must be a regular file", path)
	}
	data, err := os.ReadFile(path)
	return fileState{data: data, mode: info.Mode().Perm(), exists: true}, err
}

func (f fileState) restore(path string) error {
	if f.exists {
		return core.WriteFileAtomic(path, f.data, f.mode, false, nil)
	}
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func editHook(path string, enable bool) (func() error, error) {
	original, err := readFile(path)
	if err != nil {
		return nil, err
	}
	start, end, err := hookBlock(string(original.data))
	if err != nil {
		return nil, err
	}
	if (start >= 0) == enable {
		return nil, nil
	}
	data := string(original.data)
	mode := original.mode
	undo := func() error { return original.restore(path) }
	if enable {
		if first, _, _ := strings.Cut(data, "\n"); strings.HasPrefix(first, "#!") && !shellShebang(first) {
			return nil, fmt.Errorf("existing pre-commit uses a non-shell interpreter")
		}
		created := len(data) == 0
		if created {
			data = "#!/bin/sh\n"
		}
		newline := "\n"
		separator := !strings.HasSuffix(data, "\n")
		if separator {
			data += newline
		}
		data += strings.Join([]string{hookStart, fmt.Sprintf("%s%t %t %o", hookMetadata, created, separator, mode), guardCommand, hookEnd, ""}, newline)
		mode |= 0100
	} else {
		block := strings.Split(data[start:end], "\n")
		created, separator := false, false
		if len(block) > 1 && strings.HasPrefix(block[1], hookMetadata) {
			fields := strings.Fields(strings.TrimPrefix(block[1], hookMetadata))
			if len(fields) != 3 {
				return nil, fmt.Errorf("invalid SlopGuard hook metadata")
			}
			created, err = strconv.ParseBool(fields[0])
			if err != nil {
				return nil, err
			}
			separator, err = strconv.ParseBool(fields[1])
			if err != nil {
				return nil, err
			}
			bits, err := strconv.ParseUint(fields[2], 8, 9)
			if err != nil {
				return nil, err
			}
			mode = fs.FileMode(bits)
		}
		prefix := data[:start]
		if separator {
			prefix = strings.TrimSuffix(prefix, "\n")
		}
		data = prefix + data[end:]
		if data == "" || (created && data == "#!/bin/sh\n") {
			return undo, os.Remove(path)
		}
	}
	if err := core.WriteFileAtomic(path, []byte(data), mode, !original.exists, nil); err != nil {
		return nil, err
	}
	return undo, nil
}

func hookBlock(data string) (int, int, error) {
	start, end := -1, -1
	offset := 0
	for _, line := range strings.SplitAfter(data, "\n") {
		switch strings.TrimRight(line, "\r\n") {
		case hookStart:
			if start >= 0 || end >= 0 {
				return -1, -1, fmt.Errorf("duplicate SlopGuard hook marker")
			}
			start = offset
		case hookEnd:
			if start < 0 || end >= 0 {
				return -1, -1, fmt.Errorf("unmatched SlopGuard hook end marker")
			}
			end = offset + len(line)
		}
		offset += len(line)
	}
	if start >= 0 && end < 0 {
		return -1, -1, fmt.Errorf("unmatched SlopGuard hook start marker")
	}
	return start, end, nil
}

func shellShebang(line string) bool {
	fields := bytes.Fields([]byte(strings.TrimPrefix(line, "#!")))
	for _, field := range fields {
		switch filepath.Base(string(field)) {
		case "sh", "bash", "dash", "zsh", "ksh", "ash":
			return true
		}
	}
	return false
}
