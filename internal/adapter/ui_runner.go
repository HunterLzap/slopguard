package adapter

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"gopkg.in/yaml.v3"
)

//go:embed ui_smoke.spec.ts
var uiSmokeSpec []byte

// uiConfigRelPath 是项目级 UI 烟雾验证配置路径（相对项目根）。
const uiConfigRelPath = ".slopguard/ui.yaml"

// UIOptions 是 RunUISmoke 的调用参数。
type UIOptions struct {
	// ProjectRoot 是项目根目录。
	ProjectRoot string
	// BaseURL 覆盖 .slopguard/ui.yaml 中的 base_url。
	BaseURL string
	// Routes 覆盖配置中的路由列表。
	Routes []string
	// ScreenshotDir 覆盖截图输出目录（相对路径按项目根解析）。
	ScreenshotDir string
	// Timeout 覆盖配置中的 timeout_sec。
	Timeout time.Duration
}

type uiConfig struct {
	BaseURL       string   `yaml:"base_url"`
	Routes        []string `yaml:"routes"`
	ScreenshotDir string   `yaml:"screenshot_dir"`
	TimeoutSec    int      `yaml:"timeout_sec"`
}

// RunUISmoke 使用项目本地 Playwright 执行通用 UI 烟雾验证。
//
// 未配置 .slopguard/ui.yaml 或未安装 Playwright 时返回 Skipped=true 的结果，
// 不返回错误、不阻塞常规 verify。UI 检查失败时返回 Passed=false 与结构化
// Findings；截图路径始终写入 Screenshots（若已生成）。
func RunUISmoke(ctx context.Context, runner contract.Runner, opts UIOptions) (contract.UISmokeResult, error) {
	started := time.Now()
	if runner == nil {
		return contract.UISmokeResult{}, errors.New("adapter: nil runner")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	root, err := resolveUIRoot(opts.ProjectRoot)
	if err != nil {
		return contract.UISmokeResult{}, err
	}
	cfgPath, err := findUIConfig(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return contract.UISmokeResult{
				Skipped:    true,
				Reason:     "项目未配置 UI 烟雾验证（缺少 .slopguard/ui.yaml）",
				DurationMs: time.Since(started).Milliseconds(),
			}, nil
		}
		return contract.UISmokeResult{}, err
	}

	cfg, err := loadUIConfig(cfgPath)
	if err != nil {
		return contract.UISmokeResult{}, err
	}
	if err := applyUIOverrides(&cfg, opts); err != nil {
		return contract.UISmokeResult{}, err
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return contract.UISmokeResult{}, fmt.Errorf("adapter: UI config %s: base_url is required", cfgPath)
	}
	if len(cfg.Routes) == 0 {
		cfg.Routes = []string{"/"}
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 60
	}
	if cfg.TimeoutSec > 3600 {
		return contract.UISmokeResult{}, fmt.Errorf("adapter: UI config %s: timeout_sec must be between 1 and 3600", cfgPath)
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second

	playwright, ok := resolveExecutable(runner, root, "playwright")
	if !ok {
		return contract.UISmokeResult{
			Skipped:    true,
			Reason:     "未找到 Playwright，已跳过 UI 烟雾验证；安装提示：npm install -D @playwright/test && npx playwright install",
			DurationMs: time.Since(started).Milliseconds(),
		}, nil
	}

	screenshotDir, err := resolveScreenshotDir(root, cfg.ScreenshotDir)
	if err != nil {
		return contract.UISmokeResult{}, err
	}
	if err := os.MkdirAll(screenshotDir, 0o755); err != nil {
		return contract.UISmokeResult{}, fmt.Errorf("adapter: create UI screenshot dir %s: %w", screenshotDir, err)
	}

	stateDir := filepath.Join(root, ".slopguard")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return contract.UISmokeResult{}, fmt.Errorf("adapter: create UI temp dir under %s: %w", stateDir, err)
	}
	tempDir, err := os.MkdirTemp(stateDir, ".ui-smoke-*")
	if err != nil {
		return contract.UISmokeResult{}, fmt.Errorf("adapter: create UI temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	specPath := filepath.Join(tempDir, "smoke.spec.ts")
	configPath := filepath.Join(tempDir, "playwright.config.mjs")
	if err := os.WriteFile(specPath, uiSmokeSpec, 0o644); err != nil {
		return contract.UISmokeResult{}, fmt.Errorf("adapter: write UI smoke spec: %w", err)
	}
	if err := os.WriteFile(configPath, []byte(playwrightConfig(timeout)), 0o644); err != nil {
		return contract.UISmokeResult{}, fmt.Errorf("adapter: write Playwright config: %w", err)
	}

	routesJSON, err := json.Marshal(cfg.Routes)
	if err != nil {
		return contract.UISmokeResult{}, fmt.Errorf("adapter: encode UI routes: %w", err)
	}
	runResult := runner.Run(ctx, contract.RunSpec{
		Name: playwright,
		Args: []string{"test", "--config", configPath, "--reporter=line", specPath},
		Dir:  root,
		Env: map[string]string{
			"SLOPGUARD_BASE_URL":       cfg.BaseURL,
			"SLOPGUARD_ROUTES":         string(routesJSON),
			"SLOPGUARD_SCREENSHOT_DIR": screenshotDir,
		},
		Timeout: timeout,
	})

	result := contract.UISmokeResult{
		DurationMs:  time.Since(started).Milliseconds(),
		Screenshots: collectScreenshots(root, screenshotDir, started),
	}
	if runResult.Skipped {
		result.Skipped = true
		result.Reason = "Playwright 不可用，已跳过 UI 烟雾验证"
		return result, nil
	}
	if runResult.Err != nil {
		result.Reason = "UI 烟雾验证执行失败：" + runResult.Err.Error()
		result.Findings = []contract.Finding{{
			Rule:     "ui",
			Message:  runResult.Err.Error(),
			Severity: contract.SeverityError,
		}}
		return result, nil
	}
	if runResult.ExitCode == -1 {
		result.Reason = fmt.Sprintf("UI 烟雾验证超时或被中断（%s）", timeout)
		result.Findings = []contract.Finding{{
			Rule:     "ui",
			Message:  result.Reason,
			Severity: contract.SeverityError,
		}}
		return result, nil
	}
	if runResult.ExitCode == 0 {
		result.Passed = true
		result.Reason = "UI 烟雾验证通过"
		return result, nil
	}

	result.Passed = false
	result.Findings = parseUIFindings(runResult.Stdout, runResult.Stderr)
	if len(result.Findings) == 0 {
		result.Findings = []contract.Finding{{
			Rule:     "ui",
			Message:  "Playwright 退出码 " + fmt.Sprint(runResult.ExitCode) + "，详见 stderr",
			Severity: contract.SeverityError,
		}}
	}
	result.Reason = fmt.Sprintf("UI 烟雾验证失败，发现 %d 个问题", len(result.Findings))
	return result, nil
}

func resolveUIRoot(projectRoot string) (string, error) {
	root := strings.TrimSpace(projectRoot)
	if root == "" {
		return "", errors.New("adapter: UI project root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("adapter: resolve UI project root %s: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("adapter: UI project root %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("adapter: UI project root %s is not a directory", abs)
	}
	return abs, nil
}

func findUIConfig(root string) (string, error) {
	for _, name := range []string{"ui.yaml", "ui.yml"} {
		path := filepath.Join(root, ".slopguard", name)
		info, err := os.Stat(path)
		if err == nil {
			if info.IsDir() {
				return "", fmt.Errorf("adapter: UI config %s is a directory", path)
			}
			return path, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("adapter: inspect UI config %s: %w", path, err)
		}
	}
	return "", os.ErrNotExist
}

func loadUIConfig(path string) (uiConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return uiConfig{}, fmt.Errorf("adapter: read UI config %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return uiConfig{}, fmt.Errorf("adapter: UI config %s is empty", path)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg uiConfig
	if err := dec.Decode(&cfg); err != nil {
		return uiConfig{}, fmt.Errorf("adapter: parse UI config %s: %w", path, err)
	}
	return cfg, nil
}

func applyUIOverrides(cfg *uiConfig, opts UIOptions) error {
	if strings.TrimSpace(opts.BaseURL) != "" {
		cfg.BaseURL = opts.BaseURL
	}
	if len(opts.Routes) > 0 {
		cfg.Routes = append([]string(nil), opts.Routes...)
	}
	if strings.TrimSpace(opts.ScreenshotDir) != "" {
		cfg.ScreenshotDir = opts.ScreenshotDir
	}
	if opts.Timeout > 0 {
		if opts.Timeout > 3600*time.Second {
			return errors.New("adapter: UI timeout must not exceed 1 hour")
		}
		cfg.TimeoutSec = int((opts.Timeout + time.Second - 1) / time.Second)
		if cfg.TimeoutSec < 1 {
			cfg.TimeoutSec = 1
		}
	}
	for i, route := range cfg.Routes {
		if strings.TrimSpace(route) == "" {
			return fmt.Errorf("adapter: UI route #%d must not be empty", i)
		}
	}
	return nil
}

func resolveScreenshotDir(root, configured string) (string, error) {
	dir := strings.TrimSpace(configured)
	if dir == "" {
		dir = filepath.Join(".slopguard", "ui-screenshots")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("adapter: resolve UI screenshot dir: %w", err)
	}
	return abs, nil
}

func playwrightConfig(timeout time.Duration) string {
	milliseconds := timeout.Milliseconds()
	if milliseconds <= 0 {
		milliseconds = 60000
	}
	return fmt.Sprintf("export default { testDir: '.', timeout: %d, use: { headless: true } };\n", milliseconds)
}

func collectScreenshots(root, dir string, since time.Time) []string {
	var screenshots []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil
		}
		screenshots = append(screenshots, displayPath(root, path))
		return nil
	})
	if err != nil {
		return nil
	}
	sort.Strings(screenshots)
	return screenshots
}

func displayPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

func parseUIFindings(stdout, stderr string) []contract.Finding {
	seen := make(map[string]struct{})
	var findings []contract.Finding
	appendLine := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		file, message := "", line
		if marker := strings.Index(line, "SLOPGUARD_UI_FINDING\t"); marker >= 0 {
			parts := strings.SplitN(strings.TrimPrefix(line[marker:], "SLOPGUARD_UI_FINDING\t"), "\t", 2)
			if len(parts) == 2 {
				file, message = parts[0], parts[1]
			}
		}
		if message == "" {
			return
		}
		key := file + "\x00" + message
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		findings = append(findings, contract.Finding{
			File:     file,
			Rule:     "ui",
			Message:  message,
			Severity: contract.SeverityError,
		})
	}
	scan := func(text string) {
		scanner := bufio.NewScanner(strings.NewReader(text))
		scanner.Buffer(make([]byte, 1024), 256*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "SLOPGUARD_UI_FINDING\t") ||
				strings.Contains(line, "console.error:") ||
				strings.Contains(line, "pageerror:") ||
				strings.Contains(line, "Error:") {
				appendLine(line)
			}
		}
	}
	scan(stdout)
	scan(stderr)
	if len(findings) > 20 {
		findings = findings[:20]
	}
	return findings
}
