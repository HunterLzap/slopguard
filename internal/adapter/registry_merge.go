package adapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// GlobalAdaptersDir 返回用户级 YAML 适配器目录（~/.slopguard/adapters）。
func GlobalAdaptersDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("adapter: locate user home: %w", err)
	}
	return filepath.Join(home, ".slopguard", "adapters"), nil
}

// ProjectAdaptersDir 返回项目级 YAML 适配器目录。
func ProjectAdaptersDir(projectRoot string) string {
	return filepath.Join(projectRoot, ".slopguard", "adapters")
}

// LoadMergedRegistry 创建内置适配器注册表，并依次合并：
//  1. 用户全局目录 ~/.slopguard/adapters
//  2. 项目目录 <projectRoot>/.slopguard/adapters
//
// 两个目录都按文件名排序加载并保持严格 Schema 校验；同名 Adapter
// 以项目级定义覆盖全局级定义（项目优先）。目录不存在时视为未配置。
func LoadMergedRegistry(projectRoot string) (*Registry, error) {
	root := strings.TrimSpace(projectRoot)
	if root == "" {
		return nil, errors.New("adapter: project root must not be empty")
	}
	registry, err := NewDefaultRegistry()
	if err != nil {
		return nil, err
	}

	globalDir, err := GlobalAdaptersDir()
	if err != nil {
		return nil, err
	}
	globalAdapters, err := loadAdaptersDirIfPresent(globalDir)
	if err != nil {
		return nil, err
	}
	projectAdapters, err := loadAdaptersDirIfPresent(ProjectAdaptersDir(root))
	if err != nil {
		return nil, err
	}

	merged := make(map[string]contract.Adapter, len(globalAdapters)+len(projectAdapters))
	for _, a := range globalAdapters {
		merged[a.Name()] = a
	}
	for _, a := range projectAdapters {
		merged[a.Name()] = a
	}

	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, exists := registry.Get(name); exists {
			return nil, fmt.Errorf("adapter: YAML adapter %q conflicts with a built-in adapter", name)
		}
		if err := registry.Register(merged[name]); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// loadAdaptersDirIfPresent 加载可选目录；不存在返回空结果。
func loadAdaptersDirIfPresent(dir string) ([]contract.Adapter, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	adapters, err := LoadYAMLDir(dir)
	if err != nil {
		return nil, fmt.Errorf("adapter: load YAML adapters from %s: %w", dir, err)
	}
	return adapters, nil
}
