package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// defaultGitTimeout 是单次 git 调用的默认超时。
const defaultGitTimeout = 30 * time.Second

// FileChange 是 git 变更集中的一条文件记录。
type FileChange struct {
	// Path 是变更后的路径（相对仓库根目录，已转换为本机分隔符）。
	Path string
	// OldPath 是重命名/复制前的路径；非重命名场景为空。
	OldPath string
	// Status 是 porcelain 状态码，如 "M"、"A"、"D"、"R "、"??"。
	Status string
}

// Git 通过 contract.Runner 执行 git 命令，解析变更文件集合。
type Git struct {
	runner  contract.Runner
	dir     string
	timeout time.Duration
}

// NewGit 创建 Git 解析器；dir 为目标仓库工作目录。
func NewGit(runner contract.Runner, dir string) *Git {
	return &Git{runner: runner, dir: dir, timeout: defaultGitTimeout}
}

// SetTimeout 覆盖单次 git 调用超时。
func (g *Git) SetTimeout(timeout time.Duration) {
	if g == nil {
		return
	}
	if timeout > 0 {
		g.timeout = timeout
	}
}

// Dir 返回仓库工作目录。
func (g *Git) Dir() string {
	if g == nil {
		return ""
	}
	return g.dir
}

// IsRepository 判断 dir 是否位于 git 工作树内。
func (g *Git) IsRepository(ctx context.Context) bool {
	res := g.raw(ctx, "rev-parse", "--is-inside-work-tree")
	if res.Err != nil || res.Skipped || res.ExitCode != 0 {
		return false
	}
	return strings.TrimSpace(res.Stdout) == "true"
}

// Status 返回 git status --porcelain 的解析结果（含暂存、未暂存与未跟踪文件）。
func (g *Git) Status(ctx context.Context) ([]FileChange, error) {
	out, err := g.output(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	return ParsePorcelainStatus(out), nil
}

// DiffFiles 返回 git diff --name-only 的文件列表（相对 base，默认 HEAD）。
func (g *Git) DiffFiles(ctx context.Context, base string) ([]string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "HEAD"
	}
	out, err := g.output(ctx, "diff", "--name-only", "-z", base, "--")
	if err != nil {
		return nil, err
	}
	return ParseNameOnly(out), nil
}

// ChangedFiles 返回增量模式下应检查的文件：git status 与 git diff 的并集，
// 去重、排序，并过滤已删除或已不存在的文件。仓库尚无 HEAD 提交时退化为仅使用 status。
func (g *Git) ChangedFiles(ctx context.Context) ([]string, error) {
	changes, err := g.Status(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		if change.Path != "" {
			set[change.Path] = struct{}{}
		}
	}

	if g.hasHEAD(ctx) {
		diffFiles, err := g.DiffFiles(ctx, "HEAD")
		if err != nil {
			return nil, err
		}
		for _, file := range diffFiles {
			set[file] = struct{}{}
		}
	}

	return existingFiles(g.dir, set), nil
}

// LastCommitFiles 返回最近一次提交（HEAD）中新增、复制、修改或重命名的文件列表，
// 仅保留当前工作区仍存在的文件。仓库尚无 HEAD 提交时返回空切片。
func (g *Git) LastCommitFiles(ctx context.Context) ([]string, error) {
	if g == nil || !g.hasHEAD(ctx) {
		return []string{}, nil
	}
	out, err := g.output(ctx, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "--root", "--diff-filter=ACMR", "HEAD")
	if err != nil {
		return nil, err
	}
	files := ParseNameOnly(out)
	set := make(map[string]struct{}, len(files))
	for _, file := range files {
		if file != "" {
			set[file] = struct{}{}
		}
	}
	return existingFiles(g.dir, set), nil
}

// HotfixFiles 统计最近 days 天内 message 含 fix/bug/hotfix 等关键词的提交
// 触及的文件次数。days <= 0 时默认 30 天。返回的 key 是相对仓库根目录、
// 使用正斜杠（filepath.ToSlash）的文件路径，便于 radar 与 Hotspot.File 对齐。
func (g *Git) HotfixFiles(ctx context.Context, days int) (map[string]int, error) {
	if days <= 0 {
		days = 30
	}
	counts := map[string]int{}
	if g == nil || !g.hasHEAD(ctx) {
		return counts, nil
	}
	out, err := g.output(ctx, "log",
		fmt.Sprintf("--since=%d.days", days),
		"--name-only",
		"--pretty=format:%H%x09%s",
	)
	if err != nil {
		return nil, err
	}
	return parseHotfixLog(out), nil
}

// parseHotfixLog 解析 git log --name-only 输出。兼容普通换行格式与 -z 的
// NUL 分隔格式，保证不同调用方/测试桩都能正确处理文件名。
func parseHotfixLog(out string) map[string]int {
	counts := map[string]int{}
	if strings.Contains(out, "\x00") {
		for _, token := range strings.Split(out, "\x00") {
			if token == "" {
				continue
			}
			lines := strings.Split(strings.ReplaceAll(token, "\r\n", "\n"), "\n")
			if len(lines) == 0 {
				continue
			}
			hash, subject, ok := splitLogHeader(strings.TrimRight(lines[0], "\r"))
			if !ok || !isHexHash(hash) || !fixCommitPattern.MatchString(subject) {
				continue
			}
			seen := map[string]struct{}{}
			for _, line := range lines[1:] {
				file := normalizeGitPath(line)
				if file == "" || file == "." {
					continue
				}
				if _, ok := seen[file]; ok {
					continue
				}
				seen[file] = struct{}{}
				counts[file]++
			}
		}
		return counts
	}

	currentFix := false
	seenThisCommit := map[string]struct{}{}
	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if line == "" {
			continue
		}
		if hash, subject, ok := splitLogHeader(line); ok && isHexHash(hash) {
			currentFix = fixCommitPattern.MatchString(subject)
			seenThisCommit = map[string]struct{}{}
			continue
		}
		if !currentFix {
			continue
		}
		file := normalizeGitPath(line)
		if file == "" || file == "." {
			continue
		}
		if _, ok := seenThisCommit[file]; ok {
			continue
		}
		seenThisCommit[file] = struct{}{}
		counts[file]++
	}
	return counts
}

// normalizeGitPath 去除 git 对非 ASCII/特殊文件名添加的 C 风格引号。
func normalizeGitPath(value string) string {
	value = strings.TrimSpace(strings.TrimRight(value, "\r"))
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
	}
	return filepath.ToSlash(filepath.Clean(value))
}

var fixCommitPattern = regexp.MustCompile(`(?i)(^|[^a-z0-9])(fix|fixes|fixed|bug|bugs|bugfix|hotfix|hotfixes)([^a-z0-9]|$)`)

func splitLogHeader(line string) (hash, subject string, ok bool) {
	index := strings.IndexByte(line, '\t')
	if index < 0 {
		return "", "", false
	}
	return line[:index], line[index+1:], true
}

func isHexHash(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// ListFiles 遍历 projectRoot 返回全部匹配扩展名的文件（相对路径，已排序）。
// extensions 为空时返回所有普通文件；扩展名可带或不带点，比较时忽略大小写。
// 默认跳过 .git、node_modules、vendor、dist、build 等目录。
func ListFiles(projectRoot string, extensions []string) ([]string, error) {
	root := filepath.Clean(projectRoot)
	if info, err := os.Stat(root); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("engine: %s is not a directory", root)
	}

	wantExt := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		wantExt[ext] = struct{}{}
	}

	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != root && skippedScanDir[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(wantExt) > 0 {
			if _, ok := wantExt[strings.ToLower(filepath.Ext(entry.Name()))]; !ok {
				return nil
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("engine: list files under %s: %w", root, err)
	}
	sort.Strings(files)
	return files, nil
}

var skippedScanDir = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	"target":       true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".idea":        true,
	".vscode":      true,
	".slopguard":   true,
}

// ParsePorcelainStatus 解析 `git status --porcelain=v1 -z` 输出。
func ParsePorcelainStatus(data string) []FileChange {
	tokens := splitNULTokens(data)
	var changes []FileChange
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if len(token) < 4 {
			continue
		}
		status := token[:2]
		change := FileChange{
			Path:   filepath.FromSlash(token[3:]),
			Status: status,
		}
		if status[0] == 'R' || status[0] == 'C' || status[1] == 'R' || status[1] == 'C' {
			if i+1 < len(tokens) {
				i++
				change.OldPath = filepath.FromSlash(tokens[i])
			}
		}
		changes = append(changes, change)
	}
	return changes
}

// ParseNameOnly 解析 `git ... --name-only -z` 输出。
func ParseNameOnly(data string) []string {
	tokens := splitNULTokens(data)
	files := make([]string, 0, len(tokens))
	for _, token := range tokens {
		files = append(files, filepath.FromSlash(token))
	}
	return files
}

func splitNULTokens(data string) []string {
	if data == "" {
		return nil
	}
	parts := strings.Split(data, "\x00")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (g *Git) hasHEAD(ctx context.Context) bool {
	res := g.raw(ctx, "rev-parse", "--verify", "--quiet", "HEAD")
	return res.Err == nil && !res.Skipped && res.ExitCode == 0
}

func (g *Git) output(ctx context.Context, args ...string) (string, error) {
	res := g.raw(ctx, args...)
	if res.Skipped {
		return "", errors.New("engine: git executable not found")
	}
	if res.Err != nil {
		return "", res.Err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("engine: git %s failed (exit %d): %s",
			strings.Join(args, " "), res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

func (g *Git) raw(ctx context.Context, args ...string) contract.RunResult {
	if g == nil || g.runner == nil {
		return contract.RunResult{ExitCode: -1, Err: errors.New("engine: git runner is nil")}
	}
	timeout := g.timeout
	if timeout <= 0 {
		timeout = defaultGitTimeout
	}
	return g.runner.Run(ctx, contract.RunSpec{
		Name:    "git",
		Args:    args,
		Dir:     g.dir,
		Timeout: timeout,
	})
}

func existingFiles(root string, set map[string]struct{}) []string {
	if len(set) == 0 {
		return []string{}
	}
	files := make([]string, 0, len(set))
	for file := range set {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, file)
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		files = append(files, file)
	}
	sort.Strings(files)
	return files
}
