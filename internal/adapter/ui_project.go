package adapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// UIScreenshot 是项目最近一次 UI 烟雾验证留下的截图文件。
type UIScreenshot struct {
	// Path 是截图相对项目根的路径；截图目录在项目外时为绝对路径。
	Path       string    `json:"path"`
	ModifiedAt time.Time `json:"modified_at"`
}

// ListUIScreenshots 读取项目 .slopguard/ui.yaml 的 screenshot_dir，并列出其中的
// png/jpg/jpeg 文件，按修改时间倒序。项目、配置或截图目录不存在时返回空列表。
func ListUIScreenshots(projectRoot string) ([]UIScreenshot, error) {
	root, ok, err := existingAdapterProjectRoot(projectRoot)
	if err != nil {
		return nil, err
	}
	empty := []UIScreenshot{}
	if !ok {
		return empty, nil
	}
	cfgPath, err := findUIConfig(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return empty, nil
		}
		return nil, err
	}
	cfg, err := loadUIConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	dir, err := resolveScreenshotDir(root, cfg.ScreenshotDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return empty, nil
		}
		return nil, fmt.Errorf("adapter: read UI screenshot dir %s: %w", dir, err)
	}

	screenshots := make([]UIScreenshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		screenshots = append(screenshots, UIScreenshot{
			Path:       displayPath(root, filepath.Join(dir, entry.Name())),
			ModifiedAt: info.ModTime(),
		})
	}
	sort.Slice(screenshots, func(i, j int) bool {
		if screenshots[i].ModifiedAt.Equal(screenshots[j].ModifiedAt) {
			return screenshots[i].Path < screenshots[j].Path
		}
		return screenshots[i].ModifiedAt.After(screenshots[j].ModifiedAt)
	})
	return screenshots, nil
}

func existingAdapterProjectRoot(projectRoot string) (string, bool, error) {
	root := strings.TrimSpace(projectRoot)
	if root == "" {
		return "", false, nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", false, fmt.Errorf("adapter: resolve project root %s: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("adapter: stat project root %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("adapter: project root %s is not a directory", abs)
	}
	return abs, true, nil
}
