package engine

import (
	"context"
	"errors"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// CheckResult 是一项 contract.Check 及其执行结果。
type CheckResult struct {
	Check  contract.Check
	Result contract.RunResult
}

// Failed 表示该检查以非零退出码结束（工具缺失的 Skipped 不算失败）。
func (r CheckResult) Failed() bool {
	return !r.Result.Skipped && r.Result.Err == nil && r.Result.ExitCode != 0
}

// RunChecks 通过有界调度器并发执行 checks，结果顺序与输入一致。
// 单条检查无命令时返回系统错误结果，不 panic。
func RunChecks(ctx context.Context, runner contract.Runner, dir string, checks []contract.Check, limit int) []CheckResult {
	results := make([]CheckResult, len(checks))
	if len(checks) == 0 {
		return results
	}
	tasks := make([]Task, len(checks))
	for i := range checks {
		i := i
		check := checks[i]
		results[i].Check = check
		tasks[i] = func(ctx context.Context) error {
			if len(check.Cmd) == 0 {
				results[i].Result = contract.RunResult{
					ExitCode: -1,
					Err:      errors.New("engine: check " + check.ID + " has empty command"),
				}
				return nil
			}
			if runner == nil {
				results[i].Result = contract.RunResult{
					ExitCode: -1,
					Err:      errors.New("engine: nil runner"),
				}
				return nil
			}
			results[i].Result = runner.Run(ctx, contract.RunSpec{
				Name:    check.Cmd[0],
				Args:    check.Cmd[1:],
				Dir:     dir,
				Timeout: check.Timeout,
			})
			return nil
		}
	}
	_ = NewScheduler(limit).Run(ctx, tasks)
	return results
}
