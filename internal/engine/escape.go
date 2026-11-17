package engine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type compiledEscapePattern struct {
	name string
	re   *regexp.Regexp
}

// CountEscapes 统计 files（相对 projectRoot）中每个逃生舱正则的出现总次数。
// 返回 map 一定包含所有 pattern.Name（无命中为 0），便于直接与 baseline 比对。
// 文件按行流式读取，不一次性载入内存。
func CountEscapes(projectRoot string, files []string, patterns []contract.EscapePattern) (map[string]int, error) {
	counts := make(map[string]int, len(patterns))
	compiled := make([]compiledEscapePattern, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern.Name == "" {
			continue
		}
		re, err := regexp.Compile(pattern.Regex)
		if err != nil {
			return nil, fmt.Errorf("engine: invalid escape pattern %q: %w", pattern.Name, err)
		}
		if _, exists := counts[pattern.Name]; !exists {
			counts[pattern.Name] = 0
		}
		compiled = append(compiled, compiledEscapePattern{name: pattern.Name, re: re})
	}
	if len(compiled) == 0 || len(files) == 0 {
		return counts, nil
	}

	for _, file := range files {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(projectRoot, file)
		}
		handle, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("engine: open %s: %w", path, err)
		}
		scanner := bufio.NewScanner(handle)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			for i := range compiled {
				counts[compiled[i].name] += len(compiled[i].re.FindAllIndex(line, -1))
			}
		}
		scanErr := scanner.Err()
		closeErr := handle.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("engine: scan %s: %w", path, scanErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("engine: close %s: %w", path, closeErr)
		}
	}
	return counts, nil
}
