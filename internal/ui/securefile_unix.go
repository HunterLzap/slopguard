//go:build !windows

package ui

import "os"

func hardenSecretDir(path string) error {
	return os.Chmod(path, 0o700)
}

func hardenSecretFile(path string) error {
	return os.Chmod(path, 0o600)
}
