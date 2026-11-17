// Package adapter 实现 SlopGuard 技术栈适配器的注册、发现与检查计划解析。
package adapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// ErrDuplicateAdapter 表示同名适配器被重复注册。
var ErrDuplicateAdapter = errors.New("adapter: duplicate adapter name")

// Registry 是 contract.Adapter 的并发安全注册表。
type Registry struct {
	mu       sync.RWMutex
	adapters []contract.Adapter
	byName   map[string]contract.Adapter
}

// NewRegistry 创建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]contract.Adapter)}
}

// NewDefaultRegistry 创建注册表并注册内置的 TS/JS 与 Python 适配器。
func NewDefaultRegistry() (*Registry, error) {
	r := NewRegistry()
	if err := r.RegisterAll(NewTypeScriptAdapter(), NewPythonAdapter()); err != nil {
		return nil, err
	}
	return r, nil
}

// Register 注册一个适配器。名称为空或重复时返回错误。
func (r *Registry) Register(a contract.Adapter) error {
	if a == nil {
		return errors.New("adapter: cannot register nil adapter")
	}
	name := strings.TrimSpace(a.Name())
	if name == "" {
		return errors.New("adapter: adapter name must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[name]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateAdapter, name)
	}
	r.byName[name] = a
	r.adapters = append(r.adapters, a)
	return nil
}

// RegisterAll 原子注册多个适配器：任一适配器非法或重名时不写入任何适配器。
func (r *Registry) RegisterAll(adapters ...contract.Adapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	names := make(map[string]struct{}, len(adapters))
	for i, a := range adapters {
		if a == nil {
			return fmt.Errorf("adapter: adapter #%d is nil", i)
		}
		name := strings.TrimSpace(a.Name())
		if name == "" {
			return fmt.Errorf("adapter: adapter #%d has empty name", i)
		}
		if _, dup := names[name]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateAdapter, name)
		}
		if _, dup := r.byName[name]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateAdapter, name)
		}
		names[name] = struct{}{}
	}
	for _, a := range adapters {
		name := strings.TrimSpace(a.Name())
		r.byName[name] = a
		r.adapters = append(r.adapters, a)
	}
	return nil
}

// Get 按名称查找适配器。
func (r *Registry) Get(name string) (contract.Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byName[strings.TrimSpace(name)]
	return a, ok
}

// All 返回全部适配器的副本，按名称排序，保证结果稳定。
func (r *Registry) All() []contract.Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := append([]contract.Adapter(nil), r.adapters...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Detect 返回适用于 projectRoot 的全部适配器，按名称排序。
func (r *Registry) Detect(projectRoot string) []contract.Adapter {
	var out []contract.Adapter
	for _, a := range r.All() {
		if a.Detect(projectRoot) {
			out = append(out, a)
		}
	}
	return out
}

// RegisterYAMLFile 加载单个 YAML 适配器文件并注册其中所有适配器。
func (r *Registry) RegisterYAMLFile(path string) error {
	adapters, err := LoadYAMLFile(path)
	if err != nil {
		return err
	}
	return r.RegisterAll(adapters...)
}

// RegisterYAMLDir 加载目录下全部 .yaml/.yml 适配器文件并注册。
func (r *Registry) RegisterYAMLDir(dir string) error {
	adapters, err := LoadYAMLDir(dir)
	if err != nil {
		return err
	}
	return r.RegisterAll(adapters...)
}

// SkippedCheck 描述一项因工具缺失被优雅跳过的检查。
type SkippedCheck struct {
	ID     string // 检查 ID
	Tool   string // 缺失的可执行文件名
	Reason string // 面向用户的跳过原因与安装提示
}

// CheckPlan 是经过工具可用性探测后的检查执行计划。
type CheckPlan struct {
	Runnable []contract.Check
	Skipped  []SkippedCheck
}

// Service 把注册表与 Runner 组合成 CLI/引擎可直接使用的适配器服务。
type Service struct {
	registry *Registry
	runner   contract.Runner
}

// NewService 创建适配器服务；registry 为空时使用空注册表。
func NewService(registry *Registry, runner contract.Runner) *Service {
	if registry == nil {
		registry = NewRegistry()
	}
	return &Service{registry: registry, runner: runner}
}

// Detect 返回适用于 root 的适配器列表。
func (s *Service) Detect(root string) ([]contract.Adapter, error) {
	if s == nil || s.registry == nil {
		return nil, errors.New("adapter: nil registry")
	}
	return s.registry.Detect(root), nil
}

// PlanChecks 探测并拆分检查任务。
func (s *Service) PlanChecks(root string, checks []contract.Check) (CheckPlan, error) {
	if s == nil {
		return CheckPlan{}, errors.New("adapter: nil service")
	}
	return PlanChecks(s.runner, root, checks)
}

// Registry 返回底层注册表。
func (s *Service) Registry() *Registry {
	if s == nil {
		return nil
	}
	return s.registry
}

// PlanChecks 使用 contract.Runner.LookPath 探测每项检查的可执行文件。
// 缺失的 optional 检查放入 Skipped；缺失的必需检查返回聚合错误。
// 项目内 node_modules/.bin 与 .venv 中的本地工具会被优先解析。
func PlanChecks(runner contract.Runner, projectRoot string, checks []contract.Check) (CheckPlan, error) {
	if runner == nil {
		return CheckPlan{}, errors.New("adapter: nil runner")
	}
	var plan CheckPlan
	var missing []string
	for _, c := range checks {
		check := c
		if len(check.Cmd) == 0 || strings.TrimSpace(check.Cmd[0]) == "" {
			return CheckPlan{}, fmt.Errorf("adapter: check %q has empty command", check.ID)
		}
		tool := check.Cmd[0]
		resolved, ok := resolveExecutable(runner, projectRoot, tool)
		if !ok {
			if check.Optional {
				plan.Skipped = append(plan.Skipped, SkippedCheck{
					ID:     check.ID,
					Tool:   tool,
					Reason: missingToolReason(tool),
				})
				continue
			}
			item := fmt.Sprintf("%s（%s）", check.ID, tool)
			if hint := installHintFor(tool); hint != "" {
				item += "，安装：" + hint
			}
			missing = append(missing, item)
			continue
		}
		check.Cmd = append([]string(nil), check.Cmd...)
		check.Cmd[0] = resolved
		plan.Runnable = append(plan.Runnable, check)
	}
	if len(missing) > 0 {
		return plan, fmt.Errorf("adapter: 缺少必需工具：%s", strings.Join(missing, "；"))
	}
	return plan, nil
}

// resolveExecutable 优先解析项目内本地二进制，其次通过 Runner.LookPath 查找。
func resolveExecutable(runner contract.Runner, projectRoot, tool string) (string, bool) {
	if strings.TrimSpace(tool) == "" {
		return "", false
	}
	if filepath.IsAbs(tool) || strings.ContainsAny(tool, `/\`) {
		p := tool
		if !filepath.IsAbs(p) && projectRoot != "" {
			p = filepath.Join(projectRoot, p)
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, true
		}
		return "", false
	}
	if projectRoot != "" {
		if p, ok := findProjectTool(projectRoot, tool); ok {
			return p, true
		}
	}
	return runner.LookPath(tool)
}

// findProjectTool 查找 Node/Python 项目内的本地工具。
func findProjectTool(projectRoot, tool string) (string, bool) {
	dirs := []string{
		filepath.Join(projectRoot, "node_modules", ".bin"),
		filepath.Join(projectRoot, ".venv", "Scripts"),
		filepath.Join(projectRoot, ".venv", "bin"),
		filepath.Join(projectRoot, "venv", "Scripts"),
		filepath.Join(projectRoot, "venv", "bin"),
	}
	names := []string{tool, tool + ".cmd", tool + ".exe", tool + ".bat"}
	for _, dir := range dirs {
		for _, name := range names {
			p := filepath.Join(dir, name)
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				return p, true
			}
		}
	}
	return "", false
}

var installHints = map[string]string{
	"eslint":        "npm install -D eslint",
	"tsc":           "npm install -D typescript",
	"knip":          "npm install -D knip",
	"jscpd":         "npm install -D jscpd",
	"ruff":          "pip install ruff（或 uv add --dev ruff）",
	"pyright":       "pip install pyright（或 npm install -D pyright）",
	"semgrep":       "pip install semgrep",
	"vulture":       "pip install vulture",
	"golangci-lint": "brew install golangci-lint（或参考官方安装文档）",
}

func installHintFor(tool string) string {
	return installHints[strings.ToLower(filepath.Base(tool))]
}

func missingToolReason(tool string) string {
	if hint := installHintFor(tool); hint != "" {
		return fmt.Sprintf("未找到可执行文件 %q，已按 optional 跳过；安装提示：%s", tool, hint)
	}
	return fmt.Sprintf("未找到可执行文件 %q，已按 optional 跳过；请安装并确保在 PATH 中", tool)
}

// fileExists 判断路径是否为存在的普通文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
