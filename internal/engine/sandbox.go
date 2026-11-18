// Package engine 提供 SlopGuard 的确定性执行底座。
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/contract"
)

// DryRunOptions 描述一次候选规则的沙箱试跑。
type DryRunOptions struct {
	// Engine 是 eslint、ruff 或 semgrep。
	Engine string
	// RuleID 是候选规则 ID；用于从工具输出中筛选该规则的命中。
	RuleID string
	// RulePayload 是规则配置片段：eslint 为单条规则值 JSON（如 ["error", {...}]），
	// ruff 为 TOML 片段，semgrep 为规则 YAML；由 adapter.GenerateTempConfig 生成临时配置。
	RulePayload string
	// ProjectRoot 是项目根目录；为空时使用当前目录。
	ProjectRoot string
	// SampleBad 是可选坏样本代码；会写入临时文件并单独验证是否命中。
	SampleBad string
	// Files 是可选的相对项目根文件列表；为空时扫描项目根。
	Files []string
	// Threshold 是命中数告警阈值；<= 0 时默认 20。
	Threshold int
	// Timeout 是单次工具执行超时；0 使用 Runner 默认值。
	Timeout time.Duration
}

// DryRunResult 是一次规则沙箱试跑结果。
type DryRunResult struct {
	Engine        string   `json:"engine"`
	RuleID        string   `json:"rule_id,omitempty"`
	Hits          int      `json:"hits"`
	SampleFiles   []string `json:"sample_files"`
	SampleMatched bool     `json:"sample_matched"`
	DurationMs    int64    `json:"duration_ms"`
	Warning       string   `json:"warning,omitempty"`
}

const defaultDryRunThreshold = 20

type dryRunHit struct {
	File    string
	Rule    string
	Message string
	Line    int
}

// DryRunRule 在临时目录生成试跑配置，通过 contract.Runner 执行对应工具，
// 绝不修改项目正式配置。返回新增命中数、最多 5 个命中文件样例、sample_bad
// 是否命中、耗时与告警。
func DryRunRule(ctx context.Context, runner contract.Runner, opts DryRunOptions) (DryRunResult, error) {
	result := DryRunResult{
		Engine:      strings.ToLower(strings.TrimSpace(opts.Engine)),
		RuleID:      strings.TrimSpace(opts.RuleID),
		SampleFiles: []string{},
	}
	if runner == nil {
		return result, errors.New("engine: dry-run runner is nil")
	}
	engineName := result.Engine
	switch engineName {
	case "eslint", "ruff", "semgrep":
	default:
		return result, fmt.Errorf("engine: unsupported dry-run engine %q", opts.Engine)
	}
	payload := strings.TrimSpace(opts.RulePayload)
	if payload == "" {
		return result, errors.New("engine: dry-run rule payload is empty")
	}
	if result.RuleID == "" {
		return result, errors.New("engine: dry-run rule id is empty")
	}
	root := strings.TrimSpace(opts.ProjectRoot)
	if root == "" {
		root = "."
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return result, fmt.Errorf("engine: resolve dry-run project root: %w", err)
	}
	if info, statErr := os.Stat(root); statErr != nil || !info.IsDir() {
		if statErr == nil {
			statErr = errors.New("not a directory")
		}
		return result, fmt.Errorf("engine: dry-run project root %s: %w", root, statErr)
	}

	configPath, cleanup, err := adapter.GenerateTempConfig(engineName, result.RuleID, []byte(payload), root)
	if err != nil {
		return result, err
	}
	defer cleanup()

	targets := append([]string(nil), opts.Files...)
	if len(targets) == 0 {
		targets = []string{"."}
	}
	samplePath := ""
	if strings.TrimSpace(opts.SampleBad) != "" {
		sampleDir := dryRunSampleDir(root, targets)
		sampleFile, err := os.CreateTemp(sampleDir, "slopguard-dryrun-*"+sampleExtension(engineName, targets))
		if err != nil {
			return result, fmt.Errorf("engine: write dry-run sample: %w", err)
		}
		samplePath = sampleFile.Name()
		defer func() { _ = os.Remove(samplePath) }()
		if _, err := sampleFile.WriteString(opts.SampleBad); err != nil {
			_ = sampleFile.Close()
			return result, fmt.Errorf("engine: write dry-run sample: %w", err)
		}
		if err := sampleFile.Close(); err != nil {
			return result, fmt.Errorf("engine: write dry-run sample: %w", err)
		}
		targets = append(targets, samplePath)
	}

	tool := dryRunTool(root, engineName)
	if path, ok := runner.LookPath(tool); ok && strings.TrimSpace(path) != "" {
		tool = path
	} else if _, ok := runner.LookPath(tool); !ok {
		return result, fmt.Errorf("engine: dry-run tool %q not found", tool)
	}

	args := dryRunArgs(engineName, configPath, targets)
	env := map[string]string(nil)
	if engineName == "eslint" && strings.EqualFold(filepath.Ext(configPath), ".json") {
		// ESLint 9 默认 flat config；legacy .eslintrc 需显式切回 eslintrc 模式。
		env = map[string]string{"ESLINT_USE_FLAT_CONFIG": "false"}
	}
	run := runner.Run(ctx, contract.RunSpec{
		Name:    tool,
		Args:    args,
		Dir:     root,
		Env:     env,
		Timeout: opts.Timeout,
	})
	result.DurationMs = run.Duration.Milliseconds()
	if run.Skipped {
		return result, fmt.Errorf("engine: dry-run tool %q not found", tool)
	}
	if run.Err != nil {
		return result, run.Err
	}
	if run.ExitCode > 1 && strings.TrimSpace(run.Stdout) == "" {
		return result, fmt.Errorf("engine: dry-run %s failed (exit %d): %s",
			engineName, run.ExitCode, firstLine(run.Stderr))
	}

	hits, err := parseDryRunOutput(engineName, run.Stdout)
	if err != nil {
		return result, err
	}
	projectHits := make([]dryRunHit, 0, len(hits))
	sampleHits := 0
	for _, hit := range hits {
		// 无规则 ID 的输出（ignored 文件、fatal 解析错误等）不是规则命中。
		if strings.TrimSpace(hit.Rule) == "" {
			continue
		}
		if result.RuleID != "" && !strings.EqualFold(hit.Rule, result.RuleID) {
			continue
		}
		file := relativeDryRunFile(root, hit.File)
		if samplePath != "" && sameDryRunFile(file, relativeDryRunFile(root, samplePath)) {
			sampleHits++
			continue
		}
		projectHits = append(projectHits, dryRunHit{File: file, Rule: hit.Rule, Message: hit.Message, Line: hit.Line})
	}
	result.Hits = len(projectHits)
	result.SampleMatched = sampleHits > 0
	result.SampleFiles = uniqueFirstFiles(projectHits, 5)

	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = defaultDryRunThreshold
	}
	var warnings []string
	if result.Hits == 0 && !result.SampleMatched {
		warnings = append(warnings, "rule matched 0 occurrences; rule may be invalid or not applicable")
	}
	if result.Hits > threshold {
		warnings = append(warnings, fmt.Sprintf("rule matched %d occurrences, exceeds threshold %d", result.Hits, threshold))
	}
	if strings.TrimSpace(opts.SampleBad) != "" && !result.SampleMatched {
		warnings = append(warnings, "sample_bad was not matched")
	}
	result.Warning = strings.Join(warnings, "; ")
	return result, nil
}

func dryRunArgs(engineName, configPath string, targets []string) []string {
	switch engineName {
	case "eslint":
		var args []string
		if strings.EqualFold(filepath.Ext(configPath), ".json") {
			// Legacy .eslintrc.json：禁用项目自身配置查找，只使用临时配置。
			args = []string{"--format", "json", "--no-eslintrc", "--config", configPath}
		} else {
			// Flat config（eslint.config.mjs 等）：禁止向上查找项目配置。
			args = []string{"--format", "json", "--no-config-lookup", "--config", configPath}
		}
		return append(args, targets...)
	case "ruff":
		args := []string{"check", "--config", configPath, "--output-format", "json", "--no-cache"}
		return append(args, targets...)
	case "semgrep":
		args := []string{"--config", configPath, "--json", "--quiet"}
		return append(args, targets...)
	default:
		return nil
	}
}

func dryRunTool(root, engineName string) string {
	var candidates []string
	switch engineName {
	case "eslint":
		candidates = []string{
			filepath.Join(root, "node_modules", ".bin", "eslint"),
			filepath.Join(root, "node_modules", ".bin", "eslint.cmd"),
			filepath.Join(root, "node_modules", ".bin", "eslint.exe"),
		}
	case "ruff":
		candidates = []string{
			filepath.Join(root, ".venv", "Scripts", "ruff.exe"),
			filepath.Join(root, ".venv", "bin", "ruff"),
			filepath.Join(root, "venv", "Scripts", "ruff.exe"),
			filepath.Join(root, "venv", "bin", "ruff"),
		}
	case "semgrep":
		candidates = []string{
			filepath.Join(root, ".venv", "Scripts", "semgrep.exe"),
			filepath.Join(root, ".venv", "bin", "semgrep"),
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return engineName
}

// dryRunSampleDir 返回放置 sample_bad 的目录：优先跟随首个文件目标的目录，
// 使样本位于项目内并匹配项目配置的 files 范围，避免 ESLint 因 base path 忽略它。
func dryRunSampleDir(root string, targets []string) string {
	for _, target := range targets {
		if target == "" || target == "." {
			continue
		}
		path := target
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return filepath.Dir(path)
		}
	}
	return root
}

func sampleExtension(engineName string, targets []string) string {
	if engineName != "eslint" {
		return ".py"
	}
	for _, target := range targets {
		switch strings.ToLower(filepath.Ext(target)) {
		case ".ts":
			return ".ts"
		case ".tsx":
			return ".tsx"
		case ".jsx":
			return ".jsx"
		case ".mjs":
			return ".mjs"
		case ".cjs":
			return ".cjs"
		}
	}
	return ".js"
}

func firstLine(value string) string {
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func relativeDryRunFile(root, file string) string {
	if file == "" {
		return ""
	}
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(filepath.Clean(path))
	}
	return filepath.ToSlash(filepath.Clean(relative))
}

func sameDryRunFile(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}

func uniqueFirstFiles(hits []dryRunHit, limit int) []string {
	files := make([]string, 0, limit)
	seen := make(map[string]struct{}, len(hits))
	for _, hit := range hits {
		if hit.File == "" {
			continue
		}
		if _, ok := seen[hit.File]; ok {
			continue
		}
		seen[hit.File] = struct{}{}
		files = append(files, hit.File)
		if len(files) >= limit {
			break
		}
	}
	return files
}

type eslintDryRunFile struct {
	FilePath string `json:"filePath"`
	Messages []struct {
		RuleID   string `json:"ruleId"`
		Severity int    `json:"severity"`
		Message  string `json:"message"`
		Line     int    `json:"line"`
	} `json:"messages"`
}

type ruffDryRunFinding struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Filename string `json:"filename"`
	Location struct {
		Row int `json:"row"`
	} `json:"location"`
}

type semgrepDryRunOutput struct {
	Results []struct {
		CheckID string `json:"check_id"`
		Path    string `json:"path"`
		Start   struct {
			Line int `json:"line"`
		} `json:"start"`
	} `json:"results"`
}

func parseDryRunOutput(engineName, stdout string) ([]dryRunHit, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, nil
	}
	switch engineName {
	case "eslint":
		var files []eslintDryRunFile
		if err := json.Unmarshal([]byte(stdout), &files); err != nil {
			return nil, fmt.Errorf("engine: parse eslint dry-run output: %w", err)
		}
		var hits []dryRunHit
		for _, file := range files {
			for _, message := range file.Messages {
				if message.Severity == 0 {
					continue
				}
				// ruleId 为空的消息不是规则命中：ESLint 对 ignored 文件返回
				// "File ignored because ..."，fatal 解析错误也没有 ruleId。
				if message.RuleID == "" {
					continue
				}
				hits = append(hits, dryRunHit{
					File: file.FilePath, Rule: message.RuleID, Message: message.Message, Line: message.Line,
				})
			}
		}
		return hits, nil
	case "ruff":
		var findings []ruffDryRunFinding
		if err := json.Unmarshal([]byte(stdout), &findings); err != nil {
			return nil, fmt.Errorf("engine: parse ruff dry-run output: %w", err)
		}
		hits := make([]dryRunHit, 0, len(findings))
		for _, finding := range findings {
			hits = append(hits, dryRunHit{
				File: finding.Filename, Rule: finding.Code, Message: finding.Message, Line: finding.Location.Row,
			})
		}
		return hits, nil
	case "semgrep":
		var output semgrepDryRunOutput
		if err := json.Unmarshal([]byte(stdout), &output); err != nil {
			return nil, fmt.Errorf("engine: parse semgrep dry-run output: %w", err)
		}
		hits := make([]dryRunHit, 0, len(output.Results))
		for _, result := range output.Results {
			hits = append(hits, dryRunHit{
				File: result.Path, Rule: result.CheckID, Line: result.Start.Line,
			})
		}
		return hits, nil
	default:
		return nil, fmt.Errorf("engine: unsupported dry-run engine %q", engineName)
	}
}
