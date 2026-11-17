//go:build !windows

package engine

import "syscall"

// processAlive reports whether a PID is still present, using signal 0 on Unix.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
