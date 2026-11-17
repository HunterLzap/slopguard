package radar

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
)

// fileMetric 是单个源文件的确定性静态指标。
type fileMetric struct {
	complexity int
	escapes    int
	imports    []string
}

var controlFlowRE = regexp.MustCompile(`\b(if|elif|for|while|case|catch|switch|select|except)\b|&&|\|\|`)

// analyzeFile 读取文件并计算复杂度、逃生舱数与 import 声明。
func analyzeFile(root, file string, patterns []contract.EscapePattern) (*fileMetric, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return nil, err
	}
	content := string(data)
	escapes := 0
	if len(patterns) > 0 {
		counts, err := engine.CountEscapes(root, []string{file}, patterns)
		if err != nil {
			return nil, err
		}
		for _, count := range counts {
			escapes += count
		}
	}
	return &fileMetric{
		complexity: estimateComplexity(content),
		escapes:    escapes,
		imports:    extractImports(file, content),
	}, nil
}

func countLines(content string) int {
	if content == "" {
		return 0
	}
	lines := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		lines++
	}
	return lines
}

// estimateComplexity 用控制流关键字计数做轻量圈复杂度估算；
// 超过 500 行的文件额外增加 lines/100 的复杂度权重。
func estimateComplexity(content string) int {
	complexity := 1
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" ||
			strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "/*") ||
			strings.HasPrefix(trimmed, "*") {
			continue
		}
		complexity += len(controlFlowRE.FindAllStringIndex(trimmed, -1))
	}
	if lines := countLines(content); lines > 500 {
		complexity += lines / 100
	}
	return complexity
}
