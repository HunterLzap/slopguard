package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// PythonAdapterName 是 Python 内置适配器的注册名。
const PythonAdapterName = "python"

// PythonAdapter 是一等公民的 Python 适配器，封装 Ruff、Pyright、Semgrep。
type PythonAdapter struct{}

// NewPythonAdapter 创建 Python 内置适配器。
func NewPythonAdapter() *PythonAdapter {
	return &PythonAdapter{}
}

// Name 实现 contract.Adapter。
func (a *PythonAdapter) Name() string {
	return PythonAdapterName
}

// Detect 根据常见 Python 项目文件或根目录下的 .py 文件判定。
func (a *PythonAdapter) Detect(projectRoot string) bool {
	for _, name := range []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile", "poetry.lock", "uv.lock"} {
		if fileExists(filepath.Join(projectRoot, name)) {
			return true
		}
	}
	entries, err := os.ReadDir(projectRoot)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".py") {
			return true
		}
	}
	return false
}

// Extensions 返回 Python 关心的文件扩展名。
func (a *PythonAdapter) Extensions() []string {
	return []string{".py", ".pyi"}
}

// Checks 返回 Python 项目的确定性检查列表。
func (a *PythonAdapter) Checks(projectRoot string, files []string) []contract.Check {
	pyFiles := filterFilesByExtensions(files, a.Extensions())
	targets := pyFiles
	if len(targets) == 0 {
		targets = []string{"."}
	}

	return []contract.Check{
		{
			ID:      "ruff",
			Cmd:     append([]string{resolveTool(projectRoot, "ruff"), "check", "--output-format", "json"}, targets...),
			Timeout: 60 * time.Second,
		},
		{
			ID:      "pyright",
			Cmd:     append([]string{resolveTool(projectRoot, "pyright"), "--outputjson"}, targets...),
			Timeout: 120 * time.Second,
		},
		{
			ID:       "semgrep",
			Cmd:      append([]string{resolveTool(projectRoot, "semgrep"), "--config", "auto", "--json", "--quiet", "--metrics", "off"}, targets...),
			Optional: true,
			Timeout:  300 * time.Second,
		},
	}
}

// EscapePatterns 返回 Python 栈的逃生舱统计规则。
func (a *PythonAdapter) EscapePatterns() []contract.EscapePattern {
	return []contract.EscapePattern{
		{Name: "type_ignore_py", Regex: `(?m)#\s*type:\s*ignore(?:\[[^\]]*\])?`},
		{Name: "noqa", Regex: `(?m)#\s*noqa\b`},
		{Name: "pyright_ignore", Regex: `(?m)#\s*pyright:\s*ignore`},
	}
}
