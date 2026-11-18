// Package radar 实现零 LLM 成本的坏样本扫描与传染风险排序。
package radar

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
)

// DefaultTop 是 radar 默认输出的热点文件数。
const DefaultTop = 3

// Options 是 radar 扫描参数。
type Options struct {
	// Root 是项目根目录。
	Root string
	// Top 是输出热点数；<= 0 时使用 DefaultTop。
	Top int
	// Runner 用于执行 knip / vulture / jscpd / git 等外部工具。
	Runner contract.Runner
	// Files 是可选扫描范围（相对 Root）；nil 表示扫描全部源文件。
	Files []string
}

// Hotspot 是一个坏样本热点文件及其指标。
type Hotspot struct {
	File       string `json:"file"`
	RiskScore  int    `json:"risk_score"`
	References int    `json:"references"`
	Complexity int    `json:"complexity"`
	Escapes    int    `json:"escapes"`
	Rework     int    `json:"rework"`
	DeadCode   bool   `json:"dead_code"`
	Duplicate  bool   `json:"duplicate"`
}

// Skipped 描述一个因工具缺失或执行失败而降级的扫描维度。
type Skipped struct {
	Dimension string `json:"dimension"`
	Tool      string `json:"tool,omitempty"`
	Reason    string `json:"reason"`
}

// Result 是一次 radar 扫描的完整结果。
type Result struct {
	Hotspots []Hotspot `json:"hotspots"`
	Skipped  []Skipped `json:"skipped,omitempty"`
}

// Scan 返回按传染风险分降序排列的 Top N 热点文件。
func Scan(ctx context.Context, opts Options) ([]Hotspot, error) {
	result, err := ScanReport(ctx, opts)
	if err != nil {
		return nil, err
	}
	return result.Hotspots, nil
}

// ScanReport 返回热点与降级维度的完整扫描结果。
// 传染风险分 = 被引用次数 × (复杂度 + 逃生舱数 + 返工次数)。
func ScanReport(ctx context.Context, opts Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	root, err := resolveRoot(opts.Root)
	if err != nil {
		return Result{}, err
	}

	registry, err := adapter.LoadMergedRegistry(root)
	if err != nil {
		return Result{}, fmt.Errorf("radar: load adapter registry: %w", err)
	}

	adapters := registry.Detect(root)
	if len(adapters) == 0 {
		return Result{Hotspots: []Hotspot{}}, nil
	}
	extensions := collectExtensions(adapters)
	patterns := collectEscapePatterns(adapters)
	files, err := collectFiles(root, opts.Files, extensions)
	if err != nil {
		return Result{}, err
	}
	if len(files) == 0 {
		return Result{Hotspots: []Hotspot{}}, nil
	}

	metrics := make(map[string]*fileMetric, len(files))
	for _, file := range files {
		metric, err := analyzeFile(root, file, patterns)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Result{}, fmt.Errorf("radar: analyze %s: %w", file, err)
		}
		metrics[file] = metric
	}
	references := buildReferences(metrics, extensions)

	deadCode, skippedDead := detectDeadCode(ctx, opts.Runner, root, adapters, metrics)
	duplicates, skippedDuplicate := detectDuplicates(ctx, opts.Runner, root, metrics)
	rework, skippedRework := loadRework(ctx, opts.Runner, root)

	var skipped []Skipped
	skipped = append(skipped, skippedDead...)
	skipped = append(skipped, skippedDuplicate...)
	skipped = append(skipped, skippedRework...)

	hotspots := make([]Hotspot, 0, len(metrics))
	for file, metric := range metrics {
		refs := references[file]
		reworkCount := rework[file]
		score := refs * (metric.complexity + metric.escapes + reworkCount)
		// 保留零引用但确有坏样本（逃生舱/死代码/重复代码）的文件，
		// 避免小项目或入口文件因为无人 import 而完全从雷达中消失。
		if score <= 0 && metric.escapes == 0 && !deadCode[file] && !duplicates[file] {
			continue
		}
		hotspots = append(hotspots, Hotspot{
			File:       file,
			RiskScore:  score,
			References: refs,
			Complexity: metric.complexity,
			Escapes:    metric.escapes,
			Rework:     reworkCount,
			DeadCode:   deadCode[file],
			Duplicate:  duplicates[file],
		})
	}

	sort.SliceStable(hotspots, func(i, j int) bool {
		if hotspots[i].RiskScore != hotspots[j].RiskScore {
			return hotspots[i].RiskScore > hotspots[j].RiskScore
		}
		return hotspots[i].File < hotspots[j].File
	})
	top := opts.Top
	if top <= 0 {
		top = DefaultTop
	}
	if top > len(hotspots) {
		top = len(hotspots)
	}
	return Result{Hotspots: hotspots[:top], Skipped: dedupeSkipped(skipped)}, nil
}

func resolveRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("radar: Root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("radar: resolve root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("radar: stat root %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("radar: %s is not a directory", abs)
	}
	return abs, nil
}

func collectExtensions(adapters []contract.Adapter) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, a := range adapters {
		for _, ext := range a.Extensions() {
			ext = strings.ToLower(strings.TrimSpace(ext))
			if ext == "" {
				continue
			}
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			if _, ok := seen[ext]; ok {
				continue
			}
			seen[ext] = struct{}{}
			out = append(out, ext)
		}
	}
	sort.Strings(out)
	return out
}

func collectEscapePatterns(adapters []contract.Adapter) []contract.EscapePattern {
	seen := make(map[string]struct{})
	var out []contract.EscapePattern
	for _, a := range adapters {
		for _, pattern := range a.EscapePatterns() {
			if pattern.Name == "" || pattern.Regex == "" {
				continue
			}
			if _, ok := seen[pattern.Name]; ok {
				continue
			}
			seen[pattern.Name] = struct{}{}
			out = append(out, pattern)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func collectFiles(root string, requested []string, extensions []string) ([]string, error) {
	var candidates []string
	if requested == nil {
		listed, err := engine.ListFiles(root, extensions)
		if err != nil {
			return nil, fmt.Errorf("radar: list files: %w", err)
		}
		candidates = listed
	} else {
		candidates = requested
	}

	byExtension := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		byExtension[strings.ToLower(ext)] = struct{}{}
	}
	set := make(map[string]struct{}, len(candidates))
	for _, raw := range candidates {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		file := raw
		if filepath.IsAbs(file) {
			rel, err := filepath.Rel(root, file)
			if err != nil {
				continue
			}
			file = rel
		}
		file = filepath.ToSlash(filepath.Clean(filepath.FromSlash(file)))
		if file == "." || strings.HasPrefix(file, "../") {
			continue
		}
		if _, ok := byExtension[strings.ToLower(filepath.Ext(file))]; !ok {
			continue
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil || info.IsDir() {
			continue
		}
		set[file] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for file := range set {
		out = append(out, file)
	}
	sort.Strings(out)
	return out, nil
}

func dedupeSkipped(items []Skipped) []Skipped {
	seen := make(map[string]struct{}, len(items))
	out := make([]Skipped, 0, len(items))
	for _, item := range items {
		key := item.Dimension + "\x00" + item.Tool
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}
