package main

import (
	"context"
	"errors"

	"github.com/HunterLzap/slopguard/internal/core"
	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runName = "SlopGuard"

type windowsAutostart struct{ command string }

func newAutostart(executable string, _ core.ProcessRunner) AutostartManager {
	return windowsAutostart{command: `"` + executable + `"`}
}

func (a windowsAutostart) Enabled(ctx context.Context) (enabled bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, key.Close()) }()
	command, _, err := key.GetStringValue(runName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	return command == a.command, err
}

func (a windowsAutostart) SetEnabled(ctx context.Context, enabled bool) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if enabled {
		key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
		if err != nil {
			return err
		}
		return errors.Join(key.SetStringValue(runName, a.command), key.Close())
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, key.Close()) }()
	err = key.DeleteValue(runName)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}
