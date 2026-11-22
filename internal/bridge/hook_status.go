package bridge

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// HookEnabled reads the project hook from disk using the same path as SetHook.
func (s *Service) HookEnabled(ctx context.Context, path string) (bool, error) {
	project, err := root(path)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(filepath.Join(project, ".git")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	hook, err := hookPath(ctx, s.engine.Runner, project)
	if err != nil {
		return false, err
	}
	file, err := readFile(hook)
	if err != nil {
		return false, err
	}
	start, _, err := hookBlock(string(file.data))
	return start >= 0 && (runtime.GOOS == "windows" || file.mode&0111 != 0), err
}
