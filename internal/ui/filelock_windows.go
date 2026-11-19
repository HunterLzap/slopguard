//go:build windows

package ui

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Keep the metadata bytes (the beginning of ui.lock) outside the locked byte
// range so other local processes can read instance_id while the owner holds
// the exclusive lock. Windows shares the file handle, but reading a byte that
// is inside the locked range fails with ERROR_LOCK_VIOLATION.
const instanceLockByteOffset = 1 << 20

func instanceLockOverlapped() *windows.Overlapped {
	overlapped := new(windows.Overlapped)
	overlapped.Offset = instanceLockByteOffset
	return overlapped
}

func lockFileExclusive(f *os.File) error {
	if f == nil {
		return errors.New("ui: missing lock file")
	}
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, instanceLockOverlapped(),
	)
}

func unlockFile(f *os.File) error {
	if f == nil {
		return nil
	}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, instanceLockOverlapped())
}

func isFileLockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
