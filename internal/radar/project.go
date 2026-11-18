package radar

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/engine"
)

// BaselineMetric 是单个逃生舱指标的基线水位与当前值。
type BaselineMetric struct {
	Name     string `json:"name"`
	Baseline int    `json:"baseline"`
	Current  int    `json:"current"`
	Delta    int    `json:"delta"`
}

// BaselineStatus 是项目棘轮基线的只读视图。
// Available=false 表示项目目录不存在或没有 baseline.json。
type BaselineStatus struct {
	Available       bool             `json:"available"`
	Path            string           `json:"path,omitempty"`
	UpdatedAt       time.Time        `json:"updated_at,omitempty"`
	Passed          bool             `json:"passed"`
	Metrics         []BaselineMetric `json:"metrics"`
	SuppressedFiles []string         `json:"suppressed_files,omitempty"`
}

// LoadBaselineStatus 读取项目 .slopguard/baseline.json，并按当前源文件重新计算
// 各逃生舱指标的实际值。项目目录或 baseline 文件缺失时返回空状态而不报错。
func LoadBaselineStatus(projectRoot string) (BaselineStatus, error) {
	root, ok, err := existingProjectRoot(projectRoot)
	if err != nil {
		return BaselineStatus{}, err
	}
	empty := BaselineStatus{Metrics: []BaselineMetric{}}
	if !ok {
		return empty, nil
	}
	baselinePath := filepath.Join(root, ".slopguard", "baseline.json")
	baseline, err := engine.LoadBaseline(baselinePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return empty, nil
		}
		return BaselineStatus{}, fmt.Errorf("radar: load baseline %s: %w", baselinePath, err)
	}

	current := map[string]int{}
	registry, err := adapter.LoadMergedRegistry(root)
	if err != nil {
		return BaselineStatus{}, fmt.Errorf("radar: load adapters for baseline: %w", err)
	}
	adapters := registry.Detect(root)
	if len(adapters) > 0 {
		files, err := engine.ListFiles(root, collectExtensions(adapters))
		if err != nil {
			return BaselineStatus{}, fmt.Errorf("radar: list files for baseline: %w", err)
		}
		files = filterSuppressedProjectFiles(files, baseline)
		if len(files) > 0 {
			current, err = engine.CountEscapes(root, files, collectEscapePatterns(adapters))
			if err != nil {
				return BaselineStatus{}, fmt.Errorf("radar: count escapes for baseline: %w", err)
			}
		}
	}

	names := make(map[string]struct{}, len(baseline.Metrics)+len(current))
	for name := range baseline.Metrics {
		names[name] = struct{}{}
	}
	for name := range current {
		names[name] = struct{}{}
	}
	sortedNames := make([]string, 0, len(names))
	for name := range names {
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)
	metrics := make([]BaselineMetric, 0, len(sortedNames))
	for _, name := range sortedNames {
		base := baseline.Metrics[name]
		value := current[name]
		metrics = append(metrics, BaselineMetric{
			Name:     name,
			Baseline: base,
			Current:  value,
			Delta:    value - base,
		})
	}
	comparison := engine.Compare(current, baseline)
	return BaselineStatus{
		Available:       true,
		Path:            baselinePath,
		UpdatedAt:       baseline.UpdatedAt,
		Passed:          comparison.Passed,
		Metrics:         metrics,
		SuppressedFiles: append([]string(nil), baseline.SuppressedFiles...),
	}, nil
}

func filterSuppressedProjectFiles(files []string, baseline *engine.Baseline) []string {
	if baseline == nil || len(baseline.SuppressedFiles) == 0 {
		return files
	}
	filtered := make([]string, 0, len(files))
	for _, file := range files {
		if baseline.IsSuppressed(file) {
			continue
		}
		filtered = append(filtered, file)
	}
	return filtered
}

func existingProjectRoot(projectRoot string) (string, bool, error) {
	root := strings.TrimSpace(projectRoot)
	if root == "" {
		return "", false, nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", false, fmt.Errorf("radar: resolve project root %s: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("radar: stat project root %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("radar: project root %s is not a directory", abs)
	}
	return abs, true, nil
}
