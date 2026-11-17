package adapter

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// TypeScriptAdapterName 是 TS/JS 内置适配器的注册名。
const TypeScriptAdapterName = "typescript"

// TypeScriptAdapter 是一等公民的 TS/JS 适配器，封装 tsc、ESLint、knip、jscpd。
type TypeScriptAdapter struct{}

// NewTypeScriptAdapter 创建 TS/JS 内置适配器。
func NewTypeScriptAdapter() *TypeScriptAdapter {
	return &TypeScriptAdapter{}
}

// Name 实现 contract.Adapter。
func (a *TypeScriptAdapter) Name() string {
	return TypeScriptAdapterName
}

// Detect 在存在 package.json / tsconfig.json / jsconfig.json 时判定为 TS/JS 项目。
func (a *TypeScriptAdapter) Detect(projectRoot string) bool {
	for _, name := range []string{"package.json", "tsconfig.json", "jsconfig.json"} {
		if fileExists(filepath.Join(projectRoot, name)) {
			return true
		}
	}
	return false
}

// Extensions 返回 TS/JS 关心的文件扩展名。
func (a *TypeScriptAdapter) Extensions() []string {
	return []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}
}

// Checks 返回 TS/JS 项目的确定性检查列表。
// files 为空时执行项目级检查；非空时优先只检查变更文件。
func (a *TypeScriptAdapter) Checks(projectRoot string, files []string) []contract.Check {
	tsFiles := filterFilesByExtensions(files, a.Extensions())

	checks := make([]contract.Check, 0, 4)

	tsconfig := ""
	for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
		if p := filepath.Join(projectRoot, name); fileExists(p) {
			// 传相对路径：Runner 的工作目录就是 projectRoot，避免 Windows
			// .cmd 工具收到含空格的绝对路径参数时被 cmd.exe 拆分。
			tsconfig = name
			break
		}
	}
	if tsconfig != "" || hasAnyExtension(tsFiles, []string{".ts", ".tsx", ".mts", ".cts"}) {
		args := []string{"--noEmit", "--pretty", "false"}
		if tsconfig != "" {
			args = append(args, "--project", tsconfig)
		} else {
			args = append(args, tsFiles...)
		}
		checks = append(checks, contract.Check{
			ID:      "tsc",
			Cmd:     append([]string{resolveTool(projectRoot, "tsc")}, args...),
			Timeout: 120 * time.Second,
		})
	}

	eslintArgs := []string{"--format", "json", "--no-error-on-unmatched-pattern"}
	if len(tsFiles) > 0 {
		eslintArgs = append(eslintArgs, tsFiles...)
	} else {
		eslintArgs = append(eslintArgs, ".")
	}
	checks = append(checks, contract.Check{
		ID:      "eslint",
		Cmd:     append([]string{resolveTool(projectRoot, "eslint")}, eslintArgs...),
		Timeout: 120 * time.Second,
	})

	checks = append(checks,
		contract.Check{
			ID:       "knip",
			Cmd:      []string{resolveTool(projectRoot, "knip"), "--reporter", "json", "--no-progress"},
			Optional: true,
			Timeout:  180 * time.Second,
		},
		contract.Check{
			ID:       "jscpd",
			Cmd:      []string{resolveTool(projectRoot, "jscpd"), "--reporters", "console", "--silent", "--min-lines", "5", "--min-tokens", "50", "."},
			Optional: true,
			Timeout:  180 * time.Second,
		},
	)
	return checks
}

// EscapePatterns 返回 TS/JS 栈的逃生舱统计规则。
func (a *TypeScriptAdapter) EscapePatterns() []contract.EscapePattern {
	return []contract.EscapePattern{
		{Name: "ts_ignore", Regex: `(?m)//\s*@ts-ignore`},
		{Name: "ts_expect_error", Regex: `(?m)//\s*@ts-expect-error`},
		{Name: "eslint_disable", Regex: `(?m)//\s*eslint-disable(?:-next-line|-line)?`},
		{Name: "explicit_any", Regex: `(?m):\s*any\b`},
	}
}

// resolveTool 优先返回项目本地工具路径，否则返回裸命令名交给 Runner.LookPath 探测。
func resolveTool(projectRoot, tool string) string {
	if p, ok := findProjectTool(projectRoot, tool); ok {
		return p
	}
	return tool
}

// filterFilesByExtensions 只保留与适配器扩展名匹配的文件。
func filterFilesByExtensions(files []string, extensions []string) []string {
	set := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		set[strings.ToLower(ext)] = struct{}{}
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		if _, ok := set[strings.ToLower(filepath.Ext(f))]; ok {
			out = append(out, f)
		}
	}
	return out
}

// hasAnyExtension 判断文件列表中是否存在指定扩展名的文件。
func hasAnyExtension(files []string, extensions []string) bool {
	set := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		set[strings.ToLower(ext)] = struct{}{}
	}
	for _, f := range files {
		if _, ok := set[strings.ToLower(filepath.Ext(f))]; ok {
			return true
		}
	}
	return false
}
