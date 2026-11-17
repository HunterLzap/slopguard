package radar

import (
	"context"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
)

// loadRework 返回近 30 天 fix 类 commit 的返工文件计数。
// git 不可用或仓库不是 git 仓库时按 0 计并标注 skipped。
func loadRework(ctx context.Context, runner contract.Runner, root string) (map[string]int, []Skipped) {
	if runner == nil {
		return nil, []Skipped{{Dimension: "rework", Tool: "git", Reason: "Runner 不可用，返工维度按 0 计"}}
	}
	if _, ok := runner.LookPath("git"); !ok {
		return nil, []Skipped{{Dimension: "rework", Tool: "git", Reason: "未找到 git，返工维度按 0 计"}}
	}
	git := engine.NewGit(runner, root)
	raw, err := git.HotfixFiles(ctx, 30)
	if err != nil {
		return nil, []Skipped{{Dimension: "rework", Tool: "git", Reason: "Git 返工统计不可用：" + err.Error()}}
	}
	rework := make(map[string]int, len(raw))
	for file, count := range raw {
		if key := normalizeFileRef(file); key != "" {
			rework[key] += count
		}
	}
	return rework, nil
}
