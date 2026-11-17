//go:build !windows

package engine

import (
	"os/exec"
	"syscall"
)

// newProcessCommand 在 Unix 上直接执行可执行文件。
func newProcessCommand(path string, args []string) *exec.Cmd {
	return exec.Command(path, args...)
}

// configureProcessGroup 让子进程成为独立进程组组长，便于整组终止。
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// processHandle 在 Unix 上通过进程组负 PID 终止整棵进程树。
type processHandle struct {
	pid int
	cmd *exec.Cmd
}

func attachProcess(cmd *exec.Cmd) *processHandle {
	if cmd == nil || cmd.Process == nil {
		return &processHandle{}
	}
	return &processHandle{pid: cmd.Process.Pid, cmd: cmd}
}

func (p *processHandle) kill() {
	if p == nil || p.pid <= 0 {
		return
	}
	// 负 PID 向整个进程组发送 SIGKILL，子孙进程一并终止。
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func (p *processHandle) release() {}
