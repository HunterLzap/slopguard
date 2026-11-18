package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"gopkg.in/yaml.v3"
)

// MergeItem 描述一条全局 preset 规则在项目侧的合并结果。
type MergeItem struct {
	Engine     string `json:"engine"`
	RuleID     string `json:"rule_id"`
	ConfigPath string `json:"config_path,omitempty"`
	BackupPath string `json:"backup_path,omitempty"`
	Diff       string `json:"diff,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// MergePlan 是 MergePresets 的预览结果：列出待写入与跳过的规则及模拟 Diff，
// 不修改项目文件。调用方确认后把 plan 交给 ApplyPresets 落盘。
type MergePlan struct {
	Written []MergeItem `json:"written"`
	Skipped []MergeItem `json:"skipped"`
	Diffs   []string    `json:"diffs"`

	projectRoot string
	entries     []presetEntry
}

// MergeResult 是一次全局 preset 同步的落盘清单。
type MergeResult struct {
	Written []MergeItem `json:"written"`
	Skipped []MergeItem `json:"skipped"`
	Backups []string    `json:"backups"`
	Diffs   []string    `json:"diffs"`
}

type presetEntry struct {
	Engine  string
	RuleID  string
	Payload []byte
}

// presetEngines 是全局 presets 支持的引擎目录，顺序固定以保证输出稳定。
var presetEngines = []string{"eslint", "ruff", "semgrep"}

// presetConfigFiles 是预览时复制到临时项目根的配置文件候选，与 adapter 的
// 读写发现逻辑保持一致。
var presetConfigFiles = []string{
	"eslint.config.js",
	"eslint.config.mjs",
	"eslint.config.cjs",
	"eslint.config.ts",
	".eslintrc.json",
	".eslintrc",
	".eslintrc.yml",
	".eslintrc.yaml",
	"ruff.toml",
	".ruff.toml",
	"pyproject.toml",
	".semgrep.yml",
	".semgrep.yaml",
}

// MergePresets 把 globalDir（默认 ~/.slopguard/presets）下各引擎 rules.json
// 的通用规则非破坏性合并进 projectDir 的正式配置，等价于 PlanPresets +
// ApplyPresets。需要先预览再确认时请直接使用这两个函数。
func MergePresets(globalDir, projectDir string) (MergeResult, error) {
	plan, err := PlanPresets(globalDir, projectDir)
	if err != nil {
		return MergeResult{}, err
	}
	return ApplyPresets(plan)
}

// PlanPresets 计算待写入/跳过的规则与模拟 Diff，不修改项目文件。
// globalDir 不存在时视为没有全局沉淀，返回空计划且不报错。
func PlanPresets(globalDir, projectDir string) (MergePlan, error) {
	plan := MergePlan{
		Written: []MergeItem{},
		Skipped: []MergeItem{},
		Diffs:   []string{},
	}
	projectRoot, err := resolvePresetProjectDir(projectDir)
	if err != nil {
		return plan, err
	}
	presetRoot, err := resolvePresetGlobalDir(globalDir)
	if err != nil {
		return plan, err
	}
	info, err := os.Stat(presetRoot)
	if err != nil {
		if os.IsNotExist(err) {
			plan.projectRoot = projectRoot
			return plan, nil
		}
		return plan, fmt.Errorf("engine: stat presets directory %s: %w", presetRoot, err)
	}
	if !info.IsDir() {
		return plan, fmt.Errorf("engine: presets path %s is not a directory", presetRoot)
	}

	plan.projectRoot = projectRoot
	for _, engineName := range presetEngines {
		rules, err := readPresetRules(filepath.Join(presetRoot, engineName, "rules.json"))
		if err != nil {
			return plan, fmt.Errorf("engine: read %s presets: %w", engineName, err)
		}
		ruleIDs := make([]string, 0, len(rules))
		for ruleID := range rules {
			ruleIDs = append(ruleIDs, ruleID)
		}
		sort.Strings(ruleIDs)
		for _, ruleID := range ruleIDs {
			item := MergeItem{Engine: engineName, RuleID: ruleID}
			exists, err := projectRuleExists(engineName, ruleID, projectRoot)
			if err != nil {
				return plan, fmt.Errorf("engine: inspect %s config for rule %q: %w", engineName, ruleID, err)
			}
			if exists {
				item.Reason = "rule already exists in project config"
				plan.Skipped = append(plan.Skipped, item)
				continue
			}
			plan.entries = append(plan.entries, presetEntry{
				Engine: engineName, RuleID: ruleID, Payload: decodePresetPayload(rules[ruleID]),
			})
			plan.Written = append(plan.Written, item)
		}
	}
	if len(plan.entries) == 0 {
		return plan, nil
	}
	previews, err := previewPresetDiffs(projectRoot, plan.entries)
	if err != nil {
		return plan, err
	}
	for i := range previews {
		plan.Written[i].ConfigPath = previews[i].ConfigPath
		plan.Written[i].Diff = previews[i].Diff
		if previews[i].Diff != "" {
			plan.Diffs = append(plan.Diffs, previews[i].Diff)
		}
	}
	return plan, nil
}

// ApplyPresets 把 PlanPresets 的结果落到项目正式配置；写入前由
// adapter.ApplyRule 备份原文件并返回 Diff。重复调用时已存在的规则会跳过。
func ApplyPresets(plan MergePlan) (MergeResult, error) {
	result := MergeResult{
		Written: []MergeItem{},
		Skipped: []MergeItem{},
		Backups: []string{},
		Diffs:   []string{},
	}
	if strings.TrimSpace(plan.projectRoot) == "" {
		return result, errors.New("engine: merge plan was not created by PlanPresets")
	}
	result.Skipped = append(result.Skipped, plan.Skipped...)
	for _, entry := range plan.entries {
		item := MergeItem{Engine: entry.Engine, RuleID: entry.RuleID}
		exists, err := projectRuleExists(entry.Engine, entry.RuleID, plan.projectRoot)
		if err != nil {
			return result, fmt.Errorf("engine: inspect %s config for rule %q: %w", entry.Engine, entry.RuleID, err)
		}
		if exists {
			item.Reason = "rule already exists in project config"
			result.Skipped = append(result.Skipped, item)
			continue
		}
		applied, err := adapter.ApplyRule(entry.Engine, entry.RuleID, entry.Payload, plan.projectRoot)
		if err != nil {
			return result, fmt.Errorf("engine: apply preset %s/%s: %w", entry.Engine, entry.RuleID, err)
		}
		item.ConfigPath = applied.ConfigPath
		item.BackupPath = applied.BackupPath
		item.Diff = applied.Diff
		result.Written = append(result.Written, item)
		if applied.BackupPath != "" {
			result.Backups = append(result.Backups, applied.BackupPath)
		}
		if applied.Diff != "" {
			result.Diffs = append(result.Diffs, applied.Diff)
		}
	}
	return result, nil
}

type presetPreview struct {
	ConfigPath string
	Diff       string
}

// previewPresetDiffs 在临时项目副本上按顺序模拟 ApplyRule，生成不落盘的预览 Diff。
func previewPresetDiffs(projectRoot string, entries []presetEntry) ([]presetPreview, error) {
	tempRoot, err := os.MkdirTemp("", "slopguard-presets-plan-*")
	if err != nil {
		return nil, fmt.Errorf("engine: create preset preview dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempRoot) }()
	if err := copyPresetConfigFiles(projectRoot, tempRoot); err != nil {
		return nil, err
	}
	previews := make([]presetPreview, 0, len(entries))
	for _, entry := range entries {
		applied, err := adapter.ApplyRule(entry.Engine, entry.RuleID, entry.Payload, tempRoot)
		if err != nil {
			return nil, fmt.Errorf("engine: preview preset %s/%s: %w", entry.Engine, entry.RuleID, err)
		}
		configPath := applied.ConfigPath
		if relative, relErr := filepath.Rel(tempRoot, applied.ConfigPath); relErr == nil {
			configPath = filepath.Join(projectRoot, relative)
		}
		previews = append(previews, presetPreview{ConfigPath: configPath, Diff: applied.Diff})
	}
	return previews, nil
}

func copyPresetConfigFiles(srcRoot, dstRoot string) error {
	for _, name := range presetConfigFiles {
		source := filepath.Join(srcRoot, name)
		info, err := os.Stat(source)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("engine: stat preset config %s: %w", source, err)
		}
		if info.IsDir() {
			continue
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("engine: read preset config %s: %w", source, err)
		}
		if err := os.WriteFile(filepath.Join(dstRoot, name), data, info.Mode()); err != nil {
			return fmt.Errorf("engine: copy preset config %s: %w", source, err)
		}
	}
	return nil
}

func resolvePresetProjectDir(projectDir string) (string, error) {
	if strings.TrimSpace(projectDir) == "" {
		return "", errors.New("engine: preset project dir is empty")
	}
	root, err := filepath.Abs(projectDir)
	if err != nil {
		return "", fmt.Errorf("engine: resolve preset project dir: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("engine: stat preset project dir %s: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("engine: preset project dir %s is not a directory", root)
	}
	return root, nil
}

func resolvePresetGlobalDir(globalDir string) (string, error) {
	globalDir = strings.TrimSpace(globalDir)
	if globalDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("engine: locate user home for presets: %w", err)
		}
		globalDir = filepath.Join(home, ".slopguard", "presets")
	}
	root, err := filepath.Abs(globalDir)
	if err != nil {
		return "", fmt.Errorf("engine: resolve presets dir: %w", err)
	}
	return root, nil
}

func readPresetRules(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var rules map[string]json.RawMessage
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if rules == nil {
		rules = map[string]json.RawMessage{}
	}
	for ruleID := range rules {
		if strings.TrimSpace(ruleID) == "" {
			delete(rules, ruleID)
		}
	}
	return rules, nil
}

// decodePresetPayload 还原 global rules.json 中的 payload：
// adapter.MergeGlobalRuleTemplate 把非 JSON 内容（如 Ruff TOML、Semgrep YAML）
// 存为 JSON 字符串，这里解码回原始文本；真正的 JSON 规则值保持原样。
func decodePresetPayload(raw json.RawMessage) []byte {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []byte(text)
	}
	return append([]byte(nil), raw...)
}

func projectRuleExists(engineName, ruleID, projectRoot string) (bool, error) {
	switch engineName {
	case "eslint":
		return eslintRuleExists(projectRoot, ruleID)
	case "ruff":
		return ruffRuleExists(projectRoot, ruleID)
	case "semgrep":
		return semgrepRuleExists(projectRoot, ruleID)
	default:
		return false, fmt.Errorf("unsupported preset engine %q", engineName)
	}
}

func eslintRuleExists(projectRoot, ruleID string) (bool, error) {
	for _, name := range []string{"eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", "eslint.config.ts"} {
		path := filepath.Join(projectRoot, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, err
		}
		if textContainsRuleKey(string(data), ruleID) {
			return true, nil
		}
	}
	for _, name := range []string{".eslintrc.json", ".eslintrc", ".eslintrc.yml", ".eslintrc.yaml"} {
		path := filepath.Join(projectRoot, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, err
		}
		doc, err := parseLooseYAMLOrJSON(data)
		if err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
		if configMapHasRule(doc, ruleID) {
			return true, nil
		}
	}
	return false, nil
}

func ruffRuleExists(projectRoot, ruleID string) (bool, error) {
	for _, name := range []string{"ruff.toml", ".ruff.toml", "pyproject.toml"} {
		path := filepath.Join(projectRoot, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, err
		}
		if textContainsRuleKey(string(data), ruleID) {
			return true, nil
		}
	}
	return false, nil
}

func semgrepRuleExists(projectRoot, ruleID string) (bool, error) {
	for _, name := range []string{".semgrep.yml", ".semgrep.yaml"} {
		path := filepath.Join(projectRoot, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, err
		}
		var doc map[string]any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
		if semgrepDocHasRule(normalizePresetMap(doc), ruleID) {
			return true, nil
		}
	}
	return false, nil
}

func parseLooseYAMLOrJSON(data []byte) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err == nil {
		return normalizePresetMap(doc), nil
	}
	var yamlDoc map[string]any
	if err := yaml.Unmarshal(data, &yamlDoc); err != nil {
		return nil, err
	}
	return normalizePresetMap(yamlDoc), nil
}

func normalizePresetMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = normalizePresetValue(value)
	}
	return out
}

func normalizePresetValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		return normalizePresetMap(item)
	case map[any]any:
		out := make(map[string]any, len(item))
		for key, child := range item {
			out[fmt.Sprint(key)] = normalizePresetValue(child)
		}
		return out
	case []any:
		out := make([]any, len(item))
		for i, child := range item {
			out[i] = normalizePresetValue(child)
		}
		return out
	default:
		return value
	}
}

func configMapHasRule(doc map[string]any, ruleID string) bool {
	if rules, ok := doc["rules"].(map[string]any); ok {
		if _, exists := rules[ruleID]; exists {
			return true
		}
	}
	overrides, _ := doc["overrides"].([]any)
	for _, raw := range overrides {
		override, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if rules, ok := override["rules"].(map[string]any); ok {
			if _, exists := rules[ruleID]; exists {
				return true
			}
		}
	}
	return false
}

func semgrepDocHasRule(doc map[string]any, ruleID string) bool {
	rules, _ := doc["rules"].([]any)
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := rule["id"].(string); id == ruleID {
			return true
		}
	}
	return false
}

func textContainsRuleKey(text, ruleID string) bool {
	if strings.Contains(text, `"`+ruleID+`"`) || strings.Contains(text, `'`+ruleID+`'`) {
		return true
	}
	pattern := regexp.MustCompile(`(^|[^A-Za-z0-9_@/-])` + regexp.QuoteMeta(ruleID) + `\s*:`)
	return pattern.MatchString(text)
}
