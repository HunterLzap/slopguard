package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
	"gopkg.in/yaml.v3"
)

const (
	// ConfigRelPath 是项目级 SlopGuard 配置文件相对路径。
	ConfigRelPath = ".slopguard/config.yaml"
	// BaselineRelPath 是项目级基线文件相对路径。
	BaselineRelPath = ".slopguard/baseline.json"
)

// ProjectConfig 描述 .slopguard/config.yaml 的完整配置模型。
type ProjectConfig struct {
	Version  int          `yaml:"version"`
	Adapters []string     `yaml:"adapters"`
	Verify   VerifyConfig `yaml:"verify,omitempty"`
	Probes   ProbesConfig `yaml:"probes,omitempty"`
}

// VerifyConfig 控制 verify 的并发、工具严格度与消融测试配置。
type VerifyConfig struct {
	Jobs          int      `yaml:"jobs,omitempty"`
	StrictTools   bool     `yaml:"strict_tools,omitempty"`
	RequiredTools []string `yaml:"required_tools,omitempty"`
	TestCmd       string   `yaml:"test_cmd,omitempty"`
	TestPatterns  []string `yaml:"test_patterns,omitempty"`
}

// ProbesConfig 控制 diff 新增注释探针的行为。
type ProbesConfig struct {
	Enabled  *bool  `yaml:"enabled,omitempty"`
	Severity string `yaml:"severity,omitempty"`
}

// IsEnabled 返回探针是否启用（未配置时默认开启）。
func (p ProbesConfig) IsEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}

// EffectiveSeverity 返回探针的生效严重级别（未配置时默认 error）。
func (p ProbesConfig) EffectiveSeverity() (contract.Severity, error) {
	s := strings.ToLower(strings.TrimSpace(p.Severity))
	switch s {
	case "", "error":
		return contract.SeverityError, nil
	case "warn", "warning":
		return contract.SeverityWarn, nil
	default:
		return "", fmt.Errorf("invalid probes.severity %q (want error or warning)", p.Severity)
	}
}

// ConfigPath 返回项目配置文件的绝对/拼接路径。
func ConfigPath(root string) string {
	return filepath.Join(root, filepath.FromSlash(ConfigRelPath))
}

// BaselinePath 返回项目基线文件的绝对/拼接路径。
func BaselinePath(root string) string {
	return filepath.Join(root, filepath.FromSlash(BaselineRelPath))
}

// LoadProjectConfig 读取并校验 .slopguard/config.yaml。
// 文件不存在时原样返回 os.ErrNotExist，便于调用方按默认配置回退。
func LoadProjectConfig(root string) (ProjectConfig, error) {
	data, err := os.ReadFile(ConfigPath(root))
	if err != nil {
		return ProjectConfig{}, err
	}
	var cfg ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ProjectConfig{}, fmt.Errorf("parse %s: %w", ConfigRelPath, err)
	}
	if _, err := cfg.Probes.EffectiveSeverity(); err != nil {
		return ProjectConfig{}, fmt.Errorf("parse %s: %w", ConfigRelPath, err)
	}
	return cfg, nil
}
