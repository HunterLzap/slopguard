package radar

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/contract"
)

// detectDeadCode 运行 knip（TS/JS）与 vulture（Python），工具缺失时返回跳过信息。
func detectDeadCode(ctx context.Context, runner contract.Runner, root string, adapters []contract.Adapter, metrics map[string]*fileMetric) (map[string]bool, []Skipped) {
	dead := make(map[string]bool)
	var skipped []Skipped
	if hasAdapter(adapters, adapter.TypeScriptAdapterName) {
		check := contract.Check{
			ID:       "knip",
			Cmd:      []string{"knip", "--reporter", "json", "--no-progress"},
			Optional: true,
			Timeout:  120 * time.Second,
		}
		if result, skip := runOptionalTool(ctx, runner, root, check, "dead_code"); skip != nil {
			skipped = append(skipped, *skip)
		} else {
			markKnipOutput(result.Stdout+"\n"+result.Stderr, metrics, dead)
		}
	}
	if hasAdapter(adapters, adapter.PythonAdapterName) {
		check := contract.Check{
			ID:       "vulture",
			Cmd:      []string{"vulture", "--min-confidence", "80", "."},
			Optional: true,
			Timeout:  120 * time.Second,
		}
		if result, skip := runOptionalTool(ctx, runner, root, check, "dead_code"); skip != nil {
			skipped = append(skipped, *skip)
		} else {
			markVultureOutput(result.Stdout+"\n"+result.Stderr, metrics, dead)
		}
	}
	return dead, skipped
}

// detectDuplicates 运行 jscpd，工具缺失时返回跳过信息。
func detectDuplicates(ctx context.Context, runner contract.Runner, root string, metrics map[string]*fileMetric) (map[string]bool, []Skipped) {
	duplicates := make(map[string]bool)
	if len(metrics) == 0 {
		return duplicates, nil
	}
	check := contract.Check{
		ID:       "jscpd",
		Cmd:      []string{"jscpd", "--reporters", "console", "--silent", "--min-lines", "5", "--min-tokens", "50", "."},
		Optional: true,
		Timeout:  180 * time.Second,
	}
	result, skip := runOptionalTool(ctx, runner, root, check, "duplicate")
	if skip != nil {
		return duplicates, []Skipped{*skip}
	}
	markJSCPDOutput(result.Stdout+"\n"+result.Stderr, metrics, duplicates)
	return duplicates, nil
}

// runOptionalTool 用 adapter.PlanChecks 解析项目本地工具路径并执行；任何缺失/失败都降级。
func runOptionalTool(ctx context.Context, runner contract.Runner, root string, check contract.Check, dimension string) (contract.RunResult, *Skipped) {
	tool := ""
	if len(check.Cmd) > 0 {
		tool = check.Cmd[0]
	}
	if runner == nil {
		return contract.RunResult{}, &Skipped{Dimension: dimension, Tool: tool, Reason: "Runner 不可用，已跳过该维度"}
	}
	plan, err := adapter.PlanChecks(runner, root, []contract.Check{check})
	if err != nil {
		return contract.RunResult{}, &Skipped{Dimension: dimension, Tool: tool, Reason: err.Error()}
	}
	if len(plan.Skipped) > 0 {
		skip := plan.Skipped[0]
		return contract.RunResult{}, &Skipped{Dimension: dimension, Tool: skip.Tool, Reason: skip.Reason}
	}
	if len(plan.Runnable) == 0 {
		return contract.RunResult{}, &Skipped{Dimension: dimension, Tool: tool, Reason: "没有可执行的检查命令，已跳过"}
	}
	runnable := plan.Runnable[0]
	result := runner.Run(ctx, contract.RunSpec{
		Name:    runnable.Cmd[0],
		Args:    append([]string(nil), runnable.Cmd[1:]...),
		Dir:     root,
		Timeout: runnable.Timeout,
	})
	if result.Skipped {
		return result, &Skipped{Dimension: dimension, Tool: tool, Reason: "工具缺失，已跳过"}
	}
	if result.Err != nil {
		return result, &Skipped{Dimension: dimension, Tool: tool, Reason: fmt.Sprintf("工具执行失败：%v", result.Err)}
	}
	if result.ExitCode != 0 && strings.TrimSpace(result.Stdout) == "" && strings.TrimSpace(result.Stderr) == "" {
		return result, &Skipped{Dimension: dimension, Tool: tool, Reason: fmt.Sprintf("工具执行失败（退出码 %d），已跳过", result.ExitCode)}
	}
	return result, nil
}

func hasAdapter(adapters []contract.Adapter, name string) bool {
	for _, a := range adapters {
		if a.Name() == name {
			return true
		}
	}
	return false
}

func markKnipOutput(output string, metrics map[string]*fileMetric, dead map[string]bool) {
	findings := adapter.NewTypeScriptAdapter().ParseOutput("knip", output)
	for _, finding := range findings {
		markFile(finding.File, metrics, dead)
	}
	if len(findings) == 0 {
		collectJSONPaths(output, metrics, dead)
	}
}

var vultureLineRE = regexp.MustCompile(`(?m)^(.+?\.pyi?):\d+:`)

func markVultureOutput(output string, metrics map[string]*fileMetric, dead map[string]bool) {
	for _, match := range vultureLineRE.FindAllStringSubmatch(output, -1) {
		if len(match) > 1 {
			markFile(match[1], metrics, dead)
		}
	}
}

var jscpdCloneLineRE = regexp.MustCompile(`^\s*-\s+(.+?)\s+\[(\d+):(\d+)\s*-\s*\d+:\d+\]`)

func markJSCPDOutput(output string, metrics map[string]*fileMetric, duplicates map[string]bool) {
	findings := adapter.NewTypeScriptAdapter().ParseOutput("jscpd", output)
	for _, finding := range findings {
		markFile(finding.File, metrics, duplicates)
	}
	if len(findings) == 0 {
		for _, line := range strings.Split(output, "\n") {
			match := jscpdCloneLineRE.FindStringSubmatch(line)
			if len(match) > 1 {
				markFile(match[1], metrics, duplicates)
			}
		}
	}
}

// collectJSONPaths 从任意 JSON 输出中提取匹配已知源文件的字符串与对象键，
// 以兼容不同版本 knip 的 JSON reporter 结构差异。
func collectJSONPaths(output string, metrics map[string]*fileMetric, marks map[string]bool) {
	trimmed := strings.TrimSpace(output)
	start := strings.IndexAny(trimmed, "[{")
	end := strings.LastIndexAny(trimmed, "]}")
	if start >= 0 && end > start {
		trimmed = trimmed[start : end+1]
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return
	}
	var walk func(any)
	walk = func(v any) {
		switch item := v.(type) {
		case string:
			markFile(item, metrics, marks)
		case []any:
			for _, child := range item {
				walk(child)
			}
		case map[string]any:
			for key, child := range item {
				markFile(key, metrics, marks)
				walk(child)
			}
		}
	}
	walk(value)
}

func markFile(raw string, metrics map[string]*fileMetric, marks map[string]bool) {
	key := normalizeFileRef(raw)
	if key == "" {
		return
	}
	if _, ok := metrics[key]; ok {
		marks[key] = true
		return
	}
	for candidate := range metrics {
		if strings.HasSuffix(key, "/"+candidate) {
			marks[candidate] = true
		}
	}
}

func normalizeFileRef(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "./")
	if raw == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(raw)))
}
