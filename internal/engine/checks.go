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

// RunChecks 按输入顺序依次执行 checks，结果顺序与输入一致。
// 这样检查的启动与输出顺序稳定可复现；需要并行执行多项检查时使用
// RunChecksConcurrent（或直接用 NewScheduler 包装 Task）。
func RunChecks(ctx context.Context, runner contract.Runner, dir string, checks []contract.Check, limit int) []CheckResult {
	results := make([]CheckResult, len(checks))
	if len(checks) == 0 {
		return results
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for i := range checks {
		check := checks[i]
		results[i].Check = check
		if len(check.Cmd) == 0 {
			results[i].Result = contract.RunResult{
				ExitCode: -1,
				Err:      errors.New("engine: check " + check.ID + " has empty command"),
			}
			continue
		}
		if runner == nil {
			results[i].Result = contract.RunResult{
				ExitCode: -1,
				Err:      errors.New("engine: nil runner"),
			}
			continue
		}
		results[i].Result = runner.Run(ctx, contract.RunSpec{
			Name:    check.Cmd[0],
			Args:    check.Cmd[1:],
			Dir:     dir,
			Timeout: check.Timeout,
		})
	}
	return results
}

// RunChecksConcurrent 通过有界调度器并发执行 checks，结果顺序与输入一致。
// 生产 CLI 应优先使用本函数，以符合外部工具默认最多 2 并发的资源约束。
func RunChecksConcurrent(ctx context.Context, runner contract.Runner, dir string, checks []contract.Check, limit int) []CheckResult {
	results := make([]CheckResult, len(checks))
	if len(checks) == 0 {
		return results
	}
	if ctx == nil {
		ctx = context.Background()
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
