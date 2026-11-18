package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// 规则引擎名称。
const (
	RuleEngineESLint  = "eslint"
	RuleEngineRuff    = "ruff"
	RuleEngineSemgrep = "semgrep"
)

// RuleApplyResult 是规则正式写入项目配置后的结果。
type RuleApplyResult struct {
	ConfigPath string `json:"config_path"`
	BackupPath string `json:"backup_path,omitempty"`
	Diff       string `json:"diff"`
}

// GenerateTempConfig 生成 DryRun 专用的临时规则配置。
// 临时目录独立于项目正式配置，调用方必须 defer cleanup()。
func GenerateTempConfig(engineName, ruleID string, payload []byte, projectRoot string) (string, func(), error) {
	engineName, err := normalizeRuleEngine(engineName)
	if err != nil {
		return "", nil, err
	}
	ruleID = strings.TrimSpace(ruleID)
	if ruleID == "" {
		return "", nil, errors.New("adapter: ruleID must not be empty")
	}
	root, err := validateRuleProjectRoot(projectRoot)
	if err != nil {
		return "", nil, err
	}
	tempDir, err := os.MkdirTemp("", "slopguard-rule-*")
	if err != nil {
		return "", nil, fmt.Errorf("adapter: create temp config dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }

	var path string
	switch engineName {
	case RuleEngineESLint:
		path, err = generateESLintTempConfig(tempDir, root, ruleID, payload)
	case RuleEngineRuff:
		path, err = generateRuffTempConfig(tempDir, root, ruleID, payload)
	case RuleEngineSemgrep:
		path, err = generateSemgrepTempConfig(tempDir, ruleID, payload)
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// ApplyRule 把一条规则合并写入项目正式配置，写前备份原文件并返回 Diff。
func ApplyRule(engineName, ruleID string, payload []byte, projectRoot string) (RuleApplyResult, error) {
	engineName, err := normalizeRuleEngine(engineName)
	if err != nil {
		return RuleApplyResult{}, err
	}
	ruleID = strings.TrimSpace(ruleID)
	if ruleID == "" {
		return RuleApplyResult{}, errors.New("adapter: ruleID must not be empty")
	}
	root, err := validateRuleProjectRoot(projectRoot)
	if err != nil {
		return RuleApplyResult{}, err
	}
	switch engineName {
	case RuleEngineESLint:
		return applyESLintRule(root, ruleID, payload)
	case RuleEngineRuff:
		return applyRuffRule(root, ruleID, payload)
	case RuleEngineSemgrep:
		return applySemgrepRule(root, ruleID, payload)
	default:
		return RuleApplyResult{}, fmt.Errorf("adapter: unsupported rule engine %q", engineName)
	}
}

// GlobalRuleTemplatePath 返回指定引擎的全局规则模板路径。
func GlobalRuleTemplatePath(engineName string) (string, error) {
	engineName, err := normalizeRuleEngine(engineName)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("adapter: locate user home: %w", err)
	}
	return filepath.Join(home, ".slopguard", "presets", engineName, "rules.json"), nil
}

// ReadGlobalRuleTemplate 读取指定引擎的全局规则模板；文件不存在时返回 os.ErrNotExist。
func ReadGlobalRuleTemplate(engineName string) ([]byte, error) {
	path, err := GlobalRuleTemplatePath(engineName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// WriteGlobalRuleTemplate 原子写入指定引擎的全局规则模板，自动创建目录。
func WriteGlobalRuleTemplate(engineName string, content []byte) (string, error) {
	path, err := GlobalRuleTemplatePath(engineName)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, content, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// MergeGlobalRuleTemplate 把一条规则合并进指定引擎的全局 rules.json。
func MergeGlobalRuleTemplate(engineName, ruleID string, payload []byte) (string, error) {
	if strings.TrimSpace(ruleID) == "" {
		return "", errors.New("adapter: ruleID must not be empty")
	}
	path, err := GlobalRuleTemplatePath(engineName)
	if err != nil {
		return "", err
	}
	rules := map[string]json.RawMessage{}
	if data, readErr := os.ReadFile(path); readErr == nil {
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &rules); err != nil {
				return "", fmt.Errorf("adapter: parse global rules %s: %w", path, err)
			}
		}
	} else if !os.IsNotExist(readErr) {
		return "", readErr
	}
	raw := json.RawMessage(payload)
	if !json.Valid(payload) {
		encoded, err := json.Marshal(string(payload))
		if err != nil {
			return "", err
		}
		raw = encoded
	}
	rules[ruleID] = raw
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func normalizeRuleEngine(engineName string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(engineName)) {
	case RuleEngineESLint:
		return RuleEngineESLint, nil
	case RuleEngineRuff:
		return RuleEngineRuff, nil
	case RuleEngineSemgrep:
		return RuleEngineSemgrep, nil
	default:
		return "", fmt.Errorf("adapter: unsupported rule engine %q", engineName)
	}
}

func validateRuleProjectRoot(projectRoot string) (string, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return "", errors.New("adapter: projectRoot must not be empty")
	}
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("adapter: resolve project root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("adapter: stat project root %s: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("adapter: %s is not a directory", root)
	}
	return root, nil
}

// ---------------------------------------------------------------------------
// ESLint
// ---------------------------------------------------------------------------

const (
	eslintFlatFilesJSON = `["**/*.js","**/*.mjs","**/*.cjs","**/*.jsx","**/*.ts","**/*.tsx"]`
)

func generateESLintTempConfig(tempDir, projectRoot, ruleID string, payload []byte) (string, error) {
	ruleValue, err := decodeESLintRuleValue(payload, ruleID)
	if err != nil {
		return "", err
	}
	flatPath, flat := findESLintFlatConfig(projectRoot)
	legacyPath, legacy := findESLintLegacyConfig(projectRoot)
	if flat {
		path := filepath.Join(tempDir, "eslint.config.mjs")
		ruleJSON, err := json.Marshal(ruleValue)
		if err != nil {
			return "", err
		}
		basePath := strconv.Quote(filepath.ToSlash(projectRoot))
		var body string
		if flatPath != "" {
			body = fmt.Sprintf("import base from %s;\nexport default [...(Array.isArray(base) ? base : [base]), { files: %s, basePath: %s, rules: { %q: %s } }];\n",
				strconv.Quote(fileURL(flatPath)), eslintFlatFilesJSON, basePath, ruleID, string(ruleJSON))
		} else {
			body = fmt.Sprintf("export default [{ files: %s, basePath: %s, rules: { %q: %s } }];\n",
				eslintFlatFilesJSON, basePath, ruleID, string(ruleJSON))
		}
		return path, writeFileAtomic(path, []byte(body), 0o644)
	}
	if legacy {
		path := filepath.Join(tempDir, ".eslintrc.json")
		var data []byte
		if legacyPath != "" {
			data, err = os.ReadFile(legacyPath)
			if err != nil {
				return "", err
			}
		}
		merged, err := mergeESLintJSONConfig(data, ruleID, ruleValue)
		if err != nil {
			return "", err
		}
		return path, writeFileAtomic(path, merged, 0o644)
	}
	// 没有现成 ESLint 配置时使用 flat config。
	path := filepath.Join(tempDir, "eslint.config.mjs")
	ruleJSON, err := json.Marshal(ruleValue)
	if err != nil {
		return "", err
	}
	body := fmt.Sprintf("export default [{ files: %s, basePath: %s, rules: { %q: %s } }];\n",
		eslintFlatFilesJSON, strconv.Quote(filepath.ToSlash(projectRoot)), ruleID, string(ruleJSON))
	return path, writeFileAtomic(path, []byte(body), 0o644)
}

func findESLintFlatConfig(root string) (string, bool) {
	for _, name := range []string{"eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", "eslint.config.ts"} {
		path := filepath.Join(root, name)
		if fileExists(path) {
			return path, true
		}
	}
	return "", false
}

func findESLintLegacyConfig(root string) (string, bool) {
	for _, name := range []string{".eslintrc.json", ".eslintrc", ".eslintrc.yml", ".eslintrc.yaml"} {
		path := filepath.Join(root, name)
		if fileExists(path) {
			return path, true
		}
	}
	return "", false
}

func mergeESLintJSONConfig(data []byte, ruleID string, ruleValue any) ([]byte, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			// .eslintrc / .eslintrc.yml 可能是 YAML；尝试按 YAML 解析。
			var yamlRoot map[string]any
			if yamlErr := yaml.Unmarshal(data, &yamlRoot); yamlErr != nil {
				return nil, fmt.Errorf("adapter: parse ESLint config: %w", err)
			}
			root = normalizeYAMLMap(yamlRoot)
		}
	}
	rules, _ := root["rules"].(map[string]any)
	if rules == nil {
		rules = map[string]any{}
		root["rules"] = rules
	}
	rules[ruleID] = ruleValue
	merged, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(merged, '\n'), nil
}

func mergeESLintYAMLConfig(data []byte, ruleID string, ruleValue any) ([]byte, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("adapter: parse ESLint YAML config: %w", err)
		}
		root = normalizeYAMLMap(root)
	}
	rules, _ := root["rules"].(map[string]any)
	if rules == nil {
		rules = map[string]any{}
		root["rules"] = rules
	}
	rules[ruleID] = ruleValue
	merged, err := yaml.Marshal(root)
	if err != nil {
		return nil, err
	}
	return merged, nil
}

func applyESLintRule(root, ruleID string, payload []byte) (RuleApplyResult, error) {
	ruleValue, err := decodeESLintRuleValue(payload, ruleID)
	if err != nil {
		return RuleApplyResult{}, err
	}
	flatPath, flat := findESLintFlatConfig(root)
	if flat {
		old, err := os.ReadFile(flatPath)
		if err != nil {
			return RuleApplyResult{}, err
		}
		newData, err := mergeESLintFlatConfig(old, ruleID, ruleValue)
		if err != nil {
			return RuleApplyResult{}, err
		}
		return writeRuleConfig(root, flatPath, old, newData)
	}
	legacyPath, legacy := findESLintLegacyConfig(root)
	if !legacy {
		legacyPath = filepath.Join(root, "eslint.config.mjs")
		ruleJSON, err := json.Marshal(ruleValue)
		if err != nil {
			return RuleApplyResult{}, err
		}
		newData := []byte(fmt.Sprintf("export default [{ files: %s, rules: { %q: %s } }];\n", eslintFlatFilesJSON, ruleID, string(ruleJSON)))
		return writeRuleConfig(root, legacyPath, nil, newData)
	}
	old, err := os.ReadFile(legacyPath)
	if err != nil {
		return RuleApplyResult{}, err
	}
	if ext := strings.ToLower(filepath.Ext(legacyPath)); ext == ".yml" || ext == ".yaml" {
		newData, err := mergeESLintYAMLConfig(old, ruleID, ruleValue)
		if err != nil {
			return RuleApplyResult{}, err
		}
		return writeRuleConfig(root, legacyPath, old, newData)
	}
	newData, err := mergeESLintJSONConfig(old, ruleID, ruleValue)
	if err != nil {
		return RuleApplyResult{}, err
	}
	return writeRuleConfig(root, legacyPath, old, newData)
}

func mergeESLintFlatConfig(data []byte, ruleID string, ruleValue any) ([]byte, error) {
	text := string(data)
	ruleJSON, err := json.Marshal(ruleValue)
	if err != nil {
		return nil, err
	}
	override := fmt.Sprintf("  { files: %s, rules: { %q: %s } }", eslintFlatFilesJSON, ruleID, string(ruleJSON))
	idx := strings.LastIndex(text, "]")
	if idx < 0 {
		return nil, errors.New("adapter: unsupported ESLint flat config shape; expected an exported array")
	}
	before := strings.TrimRight(text[:idx], " \t\r\n")
	after := text[idx:]
	if before == "" {
		return nil, errors.New("adapter: empty ESLint flat config")
	}
	separator := ","
	if strings.HasSuffix(before, "[") || strings.HasSuffix(before, ",") {
		separator = ""
	}
	newText := before + separator + "\n" + override + "\n" + after
	return []byte(newText), nil
}

// ---------------------------------------------------------------------------
// Ruff
// ---------------------------------------------------------------------------

func generateRuffTempConfig(tempDir, projectRoot, ruleID string, payload []byte) (string, error) {
	path := filepath.Join(tempDir, "ruff.toml")
	var builder strings.Builder
	builder.WriteString("[lint]\nselect = [")
	builder.WriteString(strconv.Quote(ruleID))
	builder.WriteString("]\n")
	if len(bytes.TrimSpace(payload)) > 0 {
		builder.WriteString("\n")
		builder.Write(payload)
		if !bytes.HasSuffix(payload, []byte("\n")) {
			builder.WriteString("\n")
		}
	}
	return path, writeFileAtomic(path, []byte(builder.String()), 0o644)
}

func applyRuffRule(root, ruleID string, payload []byte) (RuleApplyResult, error) {
	path := ""
	for _, name := range []string{"ruff.toml", ".ruff.toml", "pyproject.toml"} {
		candidate := filepath.Join(root, name)
		if fileExists(candidate) {
			path = candidate
			break
		}
	}
	if path == "" {
		path = filepath.Join(root, "ruff.toml")
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return RuleApplyResult{}, err
	}
	section := "[lint]"
	if filepath.Base(path) == "pyproject.toml" {
		section = "[tool.ruff.lint]"
	}
	newData, err := mergeRuffTOML(old, section, ruleID, payload)
	if err != nil {
		return RuleApplyResult{}, err
	}
	return writeRuleConfig(root, path, old, newData)
}

func mergeRuffTOML(data []byte, section, ruleID string, payload []byte) ([]byte, error) {
	text := string(data)
	selection := fmt.Sprintf("select = [%s]", strconv.Quote(ruleID))
	if strings.Contains(text, section) {
		// 在目标 section 中替换或追加 select。
		start := strings.Index(text, section)
		rest := text[start+len(section):]
		nextSection := strings.Index(rest, "\n[")
		body := rest
		if nextSection >= 0 {
			body = rest[:nextSection]
			rest = rest[nextSection:]
		}
		if strings.Contains(body, "select = [") {
			body = replaceTOMLSelect(body, ruleID)
		} else {
			body = "\n" + selection + strings.TrimRight(body, " \t\r\n")
		}
		text = text[:start+len(section)] + body + rest
	} else {
		text = strings.TrimRight(text, " \t\r\n")
		if text != "" {
			text += "\n\n"
		}
		text += section + "\n" + selection + "\n"
	}
	if len(bytes.TrimSpace(payload)) > 0 {
		text = strings.TrimRight(text, " \t\r\n") + "\n\n" + string(payload)
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
	}
	return []byte(text), nil
}

func replaceTOMLSelect(body, ruleID string) string {
	start := strings.Index(body, "select = [")
	if start < 0 {
		return body
	}
	end := strings.Index(body[start:], "]")
	if end < 0 {
		return body
	}
	end += start
	existing := strings.TrimSpace(body[start+len("select = [") : end])
	var values []string
	for _, part := range strings.Split(existing, ",") {
		part = strings.TrimSpace(part)
		if part == "" || part == strconv.Quote(ruleID) || part == "'"+ruleID+"'" {
			continue
		}
		values = append(values, part)
	}
	values = append(values, strconv.Quote(ruleID))
	return body[:start] + "select = [" + strings.Join(values, ", ") + "]" + body[end+1:]
}

// ---------------------------------------------------------------------------
// Semgrep
// ---------------------------------------------------------------------------

func generateSemgrepTempConfig(tempDir, ruleID string, payload []byte) (string, error) {
	doc, err := semgrepRuleDocument(payload, ruleID)
	if err != nil {
		return "", err
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	path := filepath.Join(tempDir, "semgrep-rule.yml")
	return path, writeFileAtomic(path, data, 0o644)
}

func semgrepRuleDocument(payload []byte, ruleID string) (map[string]any, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return map[string]any{"rules": []any{map[string]any{
			"id":            ruleID,
			"languages":     []any{"generic"},
			"message":       "SlopGuard dry-run rule " + ruleID,
			"severity":      "WARNING",
			"pattern-regex": `(?s).*`,
		}}}, nil
	}
	var value any
	if err := yaml.Unmarshal(payload, &value); err != nil {
		return nil, fmt.Errorf("adapter: parse Semgrep rule YAML: %w", err)
	}
	value = normalizeYAMLValue(value)
	switch item := value.(type) {
	case map[string]any:
		if _, ok := item["rules"]; ok {
			return item, nil
		}
		if _, ok := item["id"]; ok {
			return map[string]any{"rules": []any{item}}, nil
		}
		return nil, errors.New("adapter: Semgrep payload must be a rule or contain rules")
	case []any:
		return map[string]any{"rules": item}, nil
	default:
		return nil, errors.New("adapter: unsupported Semgrep payload shape")
	}
}

func applySemgrepRule(root, ruleID string, payload []byte) (RuleApplyResult, error) {
	path := ""
	for _, name := range []string{".semgrep.yml", ".semgrep.yaml"} {
		candidate := filepath.Join(root, name)
		if fileExists(candidate) {
			path = candidate
			break
		}
	}
	if path == "" {
		path = filepath.Join(root, ".semgrep.yml")
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return RuleApplyResult{}, err
	}
	document, err := semgrepRuleDocument(payload, ruleID)
	if err != nil {
		return RuleApplyResult{}, err
	}
	newRules, _ := document["rules"].([]any)
	rootDoc := map[string]any{}
	if len(bytes.TrimSpace(old)) > 0 {
		if err := yaml.Unmarshal(old, &rootDoc); err != nil {
			return RuleApplyResult{}, fmt.Errorf("adapter: parse Semgrep config: %w", err)
		}
		rootDoc = normalizeYAMLMap(rootDoc)
	}
	existingRules, _ := rootDoc["rules"].([]any)
	rootDoc["rules"] = append(existingRules, newRules...)
	newData, err := yaml.Marshal(rootDoc)
	if err != nil {
		return RuleApplyResult{}, err
	}
	return writeRuleConfig(root, path, old, newData)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func decodeESLintRuleValue(payload []byte, ruleID string) (any, error) {
	value, err := decodeRulePayload(payload)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return "error", nil
	}
	if object, ok := value.(map[string]any); ok {
		if rules, ok := object["rules"].(map[string]any); ok {
			if selected, exists := rules[ruleID]; exists {
				return selected, nil
			}
			if len(rules) == 1 {
				for _, selected := range rules {
					return selected, nil
				}
			}
			return rules, nil
		}
		if selected, exists := object[ruleID]; exists {
			return selected, nil
		}
		if len(object) == 1 {
			for _, selected := range object {
				return selected, nil
			}
		}
	}
	return value, nil
}

func decodeRulePayload(payload []byte) (any, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(payload, &value); err == nil {
		return value, nil
	}
	if err := yaml.Unmarshal(payload, &value); err != nil {
		return nil, fmt.Errorf("adapter: parse rule payload: %w", err)
	}
	return normalizeYAMLValue(value), nil
}

func normalizeYAMLValue(value any) any {
	switch item := value.(type) {
	case map[any]any:
		out := make(map[string]any, len(item))
		for key, child := range item {
			out[fmt.Sprint(key)] = normalizeYAMLValue(child)
		}
		return out
	case map[string]any:
		return normalizeYAMLMap(item)
	case []any:
		out := make([]any, len(item))
		for i, child := range item {
			out[i] = normalizeYAMLValue(child)
		}
		return out
	default:
		return value
	}
}

func normalizeYAMLMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = normalizeYAMLValue(value)
	}
	return out
}

func fileURL(path string) string {
	slashPath := filepath.ToSlash(path)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	parsed := url.URL{Scheme: "file", Path: slashPath}
	return parsed.String()
}

func writeRuleConfig(root, path string, oldData, newData []byte) (RuleApplyResult, error) {
	backupPath := ""
	if len(oldData) > 0 {
		var err error
		backupPath, err = backupFile(path)
		if err != nil {
			return RuleApplyResult{}, err
		}
	}
	if err := writeFileAtomic(path, newData, 0o644); err != nil {
		return RuleApplyResult{}, err
	}
	return RuleApplyResult{
		ConfigPath: path,
		BackupPath: backupPath,
		Diff:       unifiedDiff(root, path, oldData, newData),
	}, nil
}

func backupFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	backup := path + ".bak"
	if fileExists(backup) {
		backup = fmt.Sprintf("%s.bak-%d", path, time.Now().UnixNano())
	}
	if err := os.WriteFile(backup, data, 0o644); err != nil {
		return "", err
	}
	return backup, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".slopguard-rule-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func unifiedDiff(root, path string, oldData, newData []byte) string {
	rel := path
	if candidate, err := filepath.Rel(root, path); err == nil {
		rel = filepath.ToSlash(candidate)
	}
	oldLines := splitFileLines(oldData)
	newLines := splitFileLines(newData)
	var builder strings.Builder
	fmt.Fprintf(&builder, "--- a/%s\n+++ b/%s\n", rel, rel)
	fmt.Fprintf(&builder, "@@ -1,%d +1,%d @@\n", len(oldLines), len(newLines))
	for _, line := range oldLines {
		builder.WriteString("-")
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	for _, line := range newLines {
		builder.WriteString("+")
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	return builder.String()
}

func splitFileLines(data []byte) []string {
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}
