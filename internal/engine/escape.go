package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type escapeTarget int

const (
	targetRaw escapeTarget = iota
	targetCode
	targetComment
)

type compiledEscapePattern struct {
	name   string
	re     *regexp.Regexp
	target escapeTarget
}

func compileEscapePatterns(patterns []contract.EscapePattern) ([]compiledEscapePattern, map[string]int, error) {
	counts := make(map[string]int, len(patterns))
	compiled := make([]compiledEscapePattern, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern.Name == "" {
			continue
		}
		re, err := regexp.Compile(pattern.Regex)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: invalid escape pattern %q: %w", pattern.Name, err)
		}
		if _, exists := counts[pattern.Name]; !exists {
			counts[pattern.Name] = 0
		}
		compiled = append(compiled, compiledEscapePattern{
			name:   pattern.Name,
			re:     re,
			target: inferEscapeTarget(pattern),
		})
	}
	return compiled, counts, nil
}

func inferEscapeTarget(p contract.EscapePattern) escapeTarget {
	switch strings.ToLower(strings.TrimSpace(p.Target)) {
	case "code":
		return targetCode
	case "comment":
		return targetComment
	case "raw":
		return targetRaw
	}
	switch strings.ToLower(strings.TrimSpace(p.Name)) {
	case "explicit_any":
		return targetCode
	case "ts_ignore", "ts_expect_error", "eslint_disable", "type_ignore_py", "noqa", "pyright_ignore", "nolint":
		return targetComment
	}
	if strings.Contains(p.Regex, "//") || strings.Contains(p.Regex, "#") {
		return targetComment
	}
	return targetRaw
}

func countEscapesOnLexedLines(lines []LexedLine, compiled []compiledEscapePattern, counts map[string]int) {
	for _, line := range lines {
		for i := range compiled {
			var haystack string
			switch compiled[i].target {
			case targetCode:
				haystack = line.Code
			case targetComment:
				haystack = line.Comments
			default:
				haystack = line.Raw
			}
			if haystack == "" {
				continue
			}
			counts[compiled[i].name] += len(compiled[i].re.FindAllStringIndex(haystack, -1))
		}
	}
}

// CountEscapes 统计 files（相对 projectRoot）中每个逃生舱正则的出现总次数。
// 返回 map 一定包含所有 pattern.Name（无命中为 0），便于直接与 baseline 比对。
// 统计前先经词法剥离，确保代码级逃生舱（如 explicit_any）不误报注释和字符串，
// 注释级逃生舱不误报字符串字面量。
func CountEscapes(projectRoot string, files []string, patterns []contract.EscapePattern) (map[string]int, error) {
	compiled, counts, err := compileEscapePatterns(patterns)
	if err != nil {
		return nil, err
	}
	if len(compiled) == 0 || len(files) == 0 {
		return counts, nil
	}

	for _, file := range files {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(projectRoot, file)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("engine: read %s: %w", path, err)
		}
		lines := LexLines(file, string(data))
		countEscapesOnLexedLines(lines, compiled, counts)
	}
	return counts, nil
}
