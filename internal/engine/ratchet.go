package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// BaselineVersion 是当前 baseline.json 的 schema 版本。
const BaselineVersion = 1

// Baseline 对应项目内可读的 .slopguard/baseline.json。
type Baseline struct {
	Version         int            `json:"version"`
	UpdatedAt       time.Time      `json:"updated_at"`
	Metrics         map[string]int `json:"metrics"`
	SuppressedFiles []string       `json:"suppressed_files"`
}

// NewBaseline 返回一个空的基线。
func NewBaseline(now time.Time) *Baseline {
	return &Baseline{
		Version:         BaselineVersion,
		UpdatedAt:       now.UTC(),
		Metrics:         map[string]int{},
		SuppressedFiles: []string{},
	}
}

// NewBaselineFromMetrics 用当前指标创建全新基线（baseline --reset 语义）。
func NewBaselineFromMetrics(metrics map[string]int, now time.Time) *Baseline {
	b := NewBaseline(now)
	b.Metrics = cloneMetrics(metrics)
	return b
}

// LoadBaseline 从 path 读取并校验 baseline.json。
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// 容忍 Windows 编辑器写入的 UTF-8 BOM。
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var baseline Baseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return nil, fmt.Errorf("engine: parse baseline %s: %w", path, err)
	}
	if baseline.Version == 0 {
		baseline.Version = BaselineVersion
	}
	if baseline.Version != BaselineVersion {
		return nil, fmt.Errorf("engine: unsupported baseline version %d in %s (want %d)",
			baseline.Version, path, BaselineVersion)
	}
	if baseline.Metrics == nil {
		baseline.Metrics = map[string]int{}
	}
	for name, value := range baseline.Metrics {
		if value < 0 {
			return nil, fmt.Errorf("engine: baseline %s has negative metric %q = %d", path, name, value)
		}
	}
	if baseline.SuppressedFiles == nil {
		baseline.SuppressedFiles = []string{}
	}
	return &baseline, nil
}

// LoadBaselineOrEmpty 读取基线；文件不存在时返回空基线（视为全部指标上限为 0）。
func LoadBaselineOrEmpty(path string) (*Baseline, error) {
	baseline, err := LoadBaseline(path)
	if err == nil {
		return baseline, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return NewBaseline(time.Now().UTC()), nil
	}
	return nil, err
}

// SaveBaseline 以稳定的两空格缩进 JSON 写入 baseline（map key 自动按字母序输出）。
func SaveBaseline(path string, baseline *Baseline) error {
	if baseline == nil {
		return errors.New("engine: nil baseline")
	}
	if baseline.Version == 0 {
		baseline.Version = BaselineVersion
	}
	if baseline.Version != BaselineVersion {
		return fmt.Errorf("engine: unsupported baseline version %d", baseline.Version)
	}
	if baseline.Metrics == nil {
		baseline.Metrics = map[string]int{}
	}
	if baseline.UpdatedAt.IsZero() {
		baseline.UpdatedAt = time.Now().UTC()
	}
	baseline.SuppressedFiles = normalizeSuppressed(baseline.SuppressedFiles)

	data, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return fmt.Errorf("engine: marshal baseline: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("engine: create baseline dir: %w", err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("engine: write baseline %s: %w", path, err)
	}
	return nil
}

// MetricDelta 描述单个逃生舱指标的基线/当前值差异。
type MetricDelta = contract.MetricDelta

// RatchetResult 是棘轮比对结果：存量放行，增量拦截。
type RatchetResult struct {
	Passed       bool          `json:"passed"`
	Regressions  []MetricDelta `json:"regressions,omitempty"`
	Improvements []MetricDelta `json:"improvements,omitempty"`
}

// Compare 将当前逃生舱计数与基线比较。
// 当前值大于基线（基线缺失按 0 计）即增量拦截；小于基线则提示可收紧。
// 结果中的条目按指标名排序，保证输出稳定。
func Compare(current map[string]int, baseline *Baseline) RatchetResult {
	if baseline == nil {
		baseline = NewBaseline(time.Time{})
	}
	names := make(map[string]struct{}, len(current)+len(baseline.Metrics))
	for name := range current {
		names[name] = struct{}{}
	}
	for name := range baseline.Metrics {
		names[name] = struct{}{}
	}
	result := RatchetResult{Passed: true}
	for name := range names {
		baseValue := baseline.Metrics[name]
		currentValue := current[name]
		if currentValue < 0 {
			currentValue = 0
		}
		switch {
		case currentValue > baseValue:
			result.Regressions = append(result.Regressions, MetricDelta{
				Name: name, Baseline: baseValue, Current: currentValue, Delta: currentValue - baseValue,
			})
		case currentValue < baseValue:
			result.Improvements = append(result.Improvements, MetricDelta{
				Name: name, Baseline: baseValue, Current: currentValue, Delta: currentValue - baseValue,
			})
		}
	}
	sort.Slice(result.Regressions, func(i, j int) bool { return result.Regressions[i].Name < result.Regressions[j].Name })
	sort.Slice(result.Improvements, func(i, j int) bool { return result.Improvements[i].Name < result.Improvements[j].Name })
	result.Passed = len(result.Regressions) == 0
	return result
}

// Tighten 只收紧已有指标的基线（只减不增），并返回实际变化的指标。
// current 中不存在或大于等于基线值的指标保持不变；新指标不会被加入。
func (b *Baseline) Tighten(current map[string]int, now time.Time) []MetricDelta {
	if b == nil {
		return nil
	}
	if b.Metrics == nil {
		b.Metrics = map[string]int{}
	}
	var changed []MetricDelta
	for name, oldValue := range b.Metrics {
		currentValue, ok := current[name]
		if !ok || currentValue < 0 || currentValue >= oldValue {
			continue
		}
		b.Metrics[name] = currentValue
		changed = append(changed, MetricDelta{
			Name: name, Baseline: oldValue, Current: currentValue, Delta: currentValue - oldValue,
		})
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].Name < changed[j].Name })
	if len(changed) > 0 {
		b.UpdatedAt = now.UTC()
	}
	return changed
}

// Reset 用当前指标重建基线（baseline --reset 语义），保留 suppressed_files。
func (b *Baseline) Reset(current map[string]int, now time.Time) {
	if b == nil {
		return
	}
	b.Version = BaselineVersion
	b.UpdatedAt = now.UTC()
	b.Metrics = cloneMetrics(current)
}

// IsSuppressed 判断文件是否在 suppressed_files 中（支持 path.Match 通配模式）。
// 路径比较统一使用 / 分隔。
func (b *Baseline) IsSuppressed(file string) bool {
	if b == nil || len(b.SuppressedFiles) == 0 {
		return false
	}
	candidate := filepath.ToSlash(filepath.Clean(file))
	for _, pattern := range b.SuppressedFiles {
		normalized := filepath.ToSlash(filepath.Clean(pattern))
		if normalized == candidate {
			return true
		}
		if strings.ContainsAny(normalized, "*?[") {
			if matched, err := path.Match(normalized, candidate); err == nil && matched {
				return true
			}
		}
	}
	return false
}

func cloneMetrics(metrics map[string]int) map[string]int {
	clone := make(map[string]int, len(metrics))
	for name, value := range metrics {
		if value < 0 {
			value = 0
		}
		clone[name] = value
	}
	return clone
}

func normalizeSuppressed(files []string) []string {
	if len(files) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(files))
	out := make([]string, 0, len(files))
	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		file = filepath.ToSlash(filepath.Clean(file))
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}
