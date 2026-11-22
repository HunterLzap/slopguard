package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/HunterLzap/slopguard/internal/core"
)

type darwinAutostart struct {
	target string
	runner core.ProcessRunner
}

const loginItemsScript = `on run argv
	set targetPath to item 1 of argv
	set operation to item 2 of argv
	tell application "System Events"
		set matches to every login item whose path is targetPath
		if operation is "status" then
			return (count of matches is greater than 0)
		else if operation is "enable" then
			if (count of matches) is 0 then
				make new login item at end with properties {path:targetPath, hidden:true}
			end if
		else
			repeat with entry in matches
				delete entry
			end repeat
		end if
	end tell
end run`

func newAutostart(executable string, runner core.ProcessRunner) AutostartManager {
	bundle := filepath.Dir(filepath.Dir(filepath.Dir(executable)))
	if strings.EqualFold(filepath.Ext(bundle), ".app") {
		executable = bundle
	}
	return darwinAutostart{target: executable, runner: runner}
}

func (a darwinAutostart) Enabled(ctx context.Context) (bool, error) {
	answer, err := a.run(ctx, "status")
	if err != nil {
		return false, err
	}
	switch answer {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected login items status %q", answer)
	}
}

func (a darwinAutostart) SetEnabled(ctx context.Context, enabled bool) error {
	op := "disable"
	if enabled {
		op = "enable"
	}
	_, err := a.run(ctx, op)
	return err
}

func (a darwinAutostart) run(ctx context.Context, operation string) (string, error) {
	bin, err := a.runner.LookPath("osascript")
	if err != nil {
		return "", err
	}
	result, err := a.runner.Run(ctx, "", bin, "-e", loginItemsScript, a.target, operation)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("用户登录项：%s（退出码 %d）", strings.TrimSpace(result.Stderr), result.ExitCode)
	}
	return strings.TrimSpace(result.Stdout), nil
}
