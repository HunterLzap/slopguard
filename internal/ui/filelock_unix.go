//go:build !windows

package ui

import (
	"errors"
	"os"
	"syscall"
)

func lockFileExclusive(f *os.File) error {
	if f == nil {
		return errors.New("ui: missing lock file")
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(f *os.File) error {
	if f == nil {
		return nil
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func isFileLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
