//go:build windows

package ui

import (
	"errors"

	"golang.org/x/sys/windows"
)

const stillActiveExitCode = 259

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access-denied normally means the process exists but belongs to a
		// different integrity level; safer to treat the lock as still held.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return true
	}
	return exitCode == stillActiveExitCode
}
