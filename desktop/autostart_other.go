//go:build !windows && !darwin

package main

import (
	"context"
	"errors"

	"github.com/HunterLzap/slopguard/internal/core"
)

type unsupportedAutostart struct{}

func newAutostart(_ string, _ core.ProcessRunner) AutostartManager { return unsupportedAutostart{} }

func (unsupportedAutostart) Enabled(context.Context) (bool, error) { return false, nil }

func (unsupportedAutostart) SetEnabled(_ context.Context, enabled bool) error {
	if enabled {
		return errors.New("当前平台不支持用户级开机自启")
	}
	return nil
}
