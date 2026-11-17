//go:build windows

package engine

import (
	"os/exec"
	"strconv"
	"strings"
)

// processAlive reports whether a PID is still present, using tasklist on Windows.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), `","`+strconv.Itoa(pid)+`",`)
}
