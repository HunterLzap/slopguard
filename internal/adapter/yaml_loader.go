package adapter

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"gopkg.in/yaml.v3"
)

// maxYAMLFileSize 限制单个适配器 YAML 文件大小，避免误读超大文件。
const maxYAMLFileSize = 1 << 20

var adapterNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// yamlAdapterConfig 是声明式 Adapter 的严格 Schema。
// 未知字段会被 yaml.Decoder.KnownFields(true) 拒绝。
type yamlAdapterConfig struct {
	Name           string             `yaml:"name"`
	Detect         yamlDetectConfig   `yaml:"detect"`
	Extensions     []string           `yaml:"extensions"`
	Checks         []yamlCheckConfig  `yaml:"checks"`
	EscapePatterns []yamlEscapeConfig `yaml:"escape_patterns"`
}

type yamlDetectConfig struct {
	FilesAny []string `yaml:"files_any"`
}

type yamlCheckConfig struct {
	ID         string   `yaml:"id"`
	Cmd        []string `yaml:"cmd"`
	Optional   bool     `yaml:"optional"`
	TimeoutSec int      `yaml:"timeout_sec"`
}

type yamlEscapeConfig struct {
	Name  string `yaml:"name"`
	Regex string `yaml:"regex"`
}

// YAMLAdapter 是由声明式 YAML 定义、实现 contract.Adapter 的适配器。
type YAMLAdapter struct {
	name       string
	filesAny   []string
	extensions []string
	checks     []contract.Check
	escapes    []contract.EscapePattern
}

// LoadYAMLFile 加载单个 YAML 文件中的全部 Adapter 定义。
func LoadYAMLFile(path string) ([]contract.Adapter, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("adapter: read YAML file %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("adapter: %s is a directory, expected a YAML file", path)
	}
	if info.Size() > maxYAMLFileSize {
		return nil, fmt.Errorf("adapter: YAML file %s is too large (%d bytes, limit %d)", path, info.Size(), maxYAMLFileSize)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("adapter: read YAML file %s: %w", path, err)
	}
	return LoadYAML(data, path)
}

// LoadYAML 解析 YAML 内容，支持用 --- 分隔的多个 Adapter 定义。
// 严格 Schema：未知字段、缺失必填字段、非法正则或非法超时都会返回明确错误。
func LoadYAML(data []byte, source string) ([]contract.Adapter, error) {
	if strings.TrimSpace(source) == "" {
		source = "<yaml>"
	}
	if len(data) > maxYAMLFileSize {
		return nil, fmt.Errorf("adapter: YAML %s is too large (%d bytes, limit %d)", source, len(data), maxYAMLFileSize)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var adapters []contract.Adapter
	for doc := 1; ; doc++ {
		var cfg yamlAdapterConfig
		err := dec.Decode(&cfg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("adapter YAML %s: document %d: %w", source, doc, err)
		}
		if isEmptyYAMLDoc(cfg) {
			continue
		}
		a, err := newYAMLAdapter(cfg)
		if err != nil {
			return nil, fmt.Errorf("adapter YAML %s: document %d: %w", source, doc, err)
		}
		adapters = append(adapters, a)
	}
	if len(adapters) == 0 {
		return nil, fmt.Errorf("adapter YAML %s: no adapter definitions found", source)
	}
	return adapters, nil
}

// LoadYAMLDir 加载目录下全部 .yaml/.yml 文件（按文件名排序）。
// 目录不存在时返回空结果，便于把用户扩展目录当作可选配置。
func LoadYAMLDir(dir string) ([]contract.Adapter, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("adapter: read YAML directory %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext == ".yaml" || ext == ".yml" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	var adapters []contract.Adapter
	seen := make(map[string]string)
	for _, name := range names {
		path := filepath.Join(dir, name)
		loaded, err := LoadYAMLFile(path)
		if err != nil {
			return nil, err
		}
		for _, a := range loaded {
			if prev, ok := seen[a.Name()]; ok {
				return nil, fmt.Errorf("adapter: duplicate adapter name %q in %s and %s", a.Name(), prev, path)
			}
			seen[a.Name()] = path
			adapters = append(adapters, a)
		}
	}
	return adapters, nil
}

func isEmptyYAMLDoc(cfg yamlAdapterConfig) bool {
	return cfg.Name == "" && len(cfg.Detect.FilesAny) == 0 && len(cfg.Extensions) == 0 &&
		len(cfg.Checks) == 0 && len(cfg.EscapePatterns) == 0
}

func newYAMLAdapter(cfg yamlAdapterConfig) (*YAMLAdapter, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return nil, errors.New("missing required field: name")
	}
	if !adapterNameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid name %q: only lowercase letters, digits, '.', '_' and '-' are allowed", name)
	}

	if len(cfg.Detect.FilesAny) == 0 {
		return nil, errors.New("missing required field: detect.files_any（至少一个探测文件）")
	}
	filesAny := make([]string, 0, len(cfg.Detect.FilesAny))
	seenFiles := make(map[string]struct{}, len(cfg.Detect.FilesAny))
	for _, pattern := range cfg.Detect.FilesAny {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return nil, errors.New("detect.files_any contains an empty pattern")
		}
		if filepath.IsAbs(pattern) || containsParentDir(pattern) {
			return nil, fmt.Errorf("detect.files_any pattern %q must be a relative path inside the project", pattern)
		}
		if _, err := filepath.Match(pattern, "x"); err != nil {
			return nil, fmt.Errorf("detect.files_any pattern %q is invalid: %w", pattern, err)
		}
		if _, dup := seenFiles[pattern]; dup {
			return nil, fmt.Errorf("duplicate detect.files_any pattern %q", pattern)
		}
		seenFiles[pattern] = struct{}{}
		filesAny = append(filesAny, pattern)
	}

	if len(cfg.Extensions) == 0 {
		return nil, errors.New("missing required field: extensions（至少一个扩展名）")
	}
	extensions := make([]string, 0, len(cfg.Extensions))
	seenExt := make(map[string]struct{}, len(cfg.Extensions))
	for _, ext := range cfg.Extensions {
		ext = strings.TrimSpace(ext)
		if len(ext) < 2 || !strings.HasPrefix(ext, ".") {
			return nil, fmt.Errorf("invalid extension %q: must start with '.' and contain at least one character", ext)
		}
		key := strings.ToLower(ext)
		if _, dup := seenExt[key]; dup {
			return nil, fmt.Errorf("duplicate extension %q", ext)
		}
		seenExt[key] = struct{}{}
		extensions = append(extensions, ext)
	}

	if len(cfg.Checks) == 0 {
		return nil, errors.New("missing required field: checks（至少一项检查）")
	}
	checks := make([]contract.Check, 0, len(cfg.Checks))
	seenCheckIDs := make(map[string]struct{}, len(cfg.Checks))
	for i, check := range cfg.Checks {
		id := strings.TrimSpace(check.ID)
		if id == "" {
			return nil, fmt.Errorf("checks[%d]: missing required field: id", i)
		}
		if _, dup := seenCheckIDs[id]; dup {
			return nil, fmt.Errorf("duplicate check id %q", id)
		}
		seenCheckIDs[id] = struct{}{}
		if len(check.Cmd) == 0 || strings.TrimSpace(check.Cmd[0]) == "" {
			return nil, fmt.Errorf("check %q: cmd must not be empty", id)
		}
		if check.TimeoutSec < 0 || check.TimeoutSec > 3600 {
			return nil, fmt.Errorf("check %q: timeout_sec must be between 0 and 3600", id)
		}
		checks = append(checks, contract.Check{
			ID:       id,
			Cmd:      append([]string(nil), check.Cmd...),
			Optional: check.Optional,
			Timeout:  time.Duration(check.TimeoutSec) * time.Second,
		})
	}

	escapes := make([]contract.EscapePattern, 0, len(cfg.EscapePatterns))
	seenEscapeNames := make(map[string]struct{}, len(cfg.EscapePatterns))
	for i, escape := range cfg.EscapePatterns {
		escapeName := strings.TrimSpace(escape.Name)
		if escapeName == "" {
			return nil, fmt.Errorf("escape_patterns[%d]: missing required field: name", i)
		}
		if _, dup := seenEscapeNames[escapeName]; dup {
			return nil, fmt.Errorf("duplicate escape pattern name %q", escapeName)
		}
		seenEscapeNames[escapeName] = struct{}{}
		if strings.TrimSpace(escape.Regex) == "" {
			return nil, fmt.Errorf("escape pattern %q: regex must not be empty", escapeName)
		}
		if _, err := regexp.Compile(escape.Regex); err != nil {
			return nil, fmt.Errorf("escape pattern %q: invalid regex: %w", escapeName, err)
		}
		escapes = append(escapes, contract.EscapePattern{Name: escapeName, Regex: escape.Regex})
	}

	return &YAMLAdapter{
		name:       name,
		filesAny:   filesAny,
		extensions: extensions,
		checks:     checks,
		escapes:    escapes,
	}, nil
}

func containsParentDir(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// Name 实现 contract.Adapter。
func (a *YAMLAdapter) Name() string { return a.name }

// Detect 实现 contract.Adapter：任一 files_any 模式命中即适用。
func (a *YAMLAdapter) Detect(projectRoot string) bool {
	for _, pattern := range a.filesAny {
		if strings.ContainsAny(pattern, "*?[") {
			matches, err := filepath.Glob(filepath.Join(projectRoot, pattern))
			if err == nil {
				for _, match := range matches {
					if info, statErr := os.Stat(match); statErr == nil && !info.IsDir() {
						return true
					}
				}
			}
			continue
		}
		if fileExists(filepath.Join(projectRoot, pattern)) {
			return true
		}
	}
	return false
}

// Extensions 实现 contract.Adapter。
func (a *YAMLAdapter) Extensions() []string {
	return append([]string(nil), a.extensions...)
}

// Checks 实现 contract.Adapter。参数 {files} 会展开为传入的文件列表。
func (a *YAMLAdapter) Checks(_ string, files []string) []contract.Check {
	out := make([]contract.Check, 0, len(a.checks))
	for _, check := range a.checks {
		cmd := make([]string, 0, len(check.Cmd)+len(files))
		for _, arg := range check.Cmd {
			if arg == "{files}" {
				cmd = append(cmd, files...)
				continue
			}
			cmd = append(cmd, arg)
		}
		out = append(out, contract.Check{
			ID:       check.ID,
			Cmd:      cmd,
			Optional: check.Optional,
			Timeout:  check.Timeout,
		})
	}
	return out
}

// EscapePatterns 实现 contract.Adapter。
func (a *YAMLAdapter) EscapePatterns() []contract.EscapePattern {
	return append([]contract.EscapePattern(nil), a.escapes...)
}

// ParseOutput 实现 contract.Adapter。自定义工具没有通用输出格式，
// 返回空结果，由引擎按退出码做失败兜底。
func (a *YAMLAdapter) ParseOutput(_ string, _ string) []contract.Finding {
	return nil
}
