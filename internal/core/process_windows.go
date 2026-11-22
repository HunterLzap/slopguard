package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func processCommand(ctx context.Context, bin string, args []string) (*exec.Cmd, error) {
	switch strings.ToLower(filepath.Ext(bin)) {
	case ".bat", ".cmd":
	default:
		return exec.CommandContext(ctx, bin, args...), nil
	}
	// Only batch files require cmd.exe. Reject tokens with shell expansion or
	// control characters before constructing its separately quoted command line.
	tokens := append([]string{bin}, args...)
	for i, token := range tokens {
		if strings.ContainsAny(token, "\x00\r\n\"%!^&|<>") {
			return nil, fmt.Errorf("unsupported character in batch command argument %d", i)
		}
		tokens[i] = "\"" + token + "\""
	}
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return nil, fmt.Errorf("SystemRoot is required to run a Windows batch file")
	}
	cmd := exec.CommandContext(ctx, filepath.Join(systemRoot, "System32", "cmd.exe"))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `cmd.exe /d /v:off /s /c "` + strings.Join(tokens, " ") + `"`,
	}
	return cmd, nil
}
