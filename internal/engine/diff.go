package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// DiffLine 描述统一 diff hunk 中的一行新增或删除内容。
type DiffLine struct {
	Line int
	Text string
}

// DiffHunk 描述一个文件内的连续变更块。
type DiffHunk struct {
	OldStart int
	NewStart int
	Added    []DiffLine
	Removed  []DiffLine
}

// DiffFile 描述单个文件在 diff 中的增删行记录。
type DiffFile struct {
	Path      string // 仓库相对路径（统一正斜杠）
	IsNew     bool
	IsDeleted bool
	IsBinary  bool
	Hunks     []DiffHunk
}

// DiffContentLoader 按需读取文件在变更前/后的完整源码，用于恢复跨行字符串与块注释状态。
// 当返回 ok=false 时，Diff 解析器自动降级为仅对 hunk 内增删行进行词法剥离。
type DiffContentLoader func(slashPath string, isNewVersion bool) (content string, ok bool)

// ParseUnifiedDiff 解析 `git diff --unified=0 --no-color --no-renames` 输出。
func ParseUnifiedDiff(diffText string) []DiffFile {
	if strings.TrimSpace(diffText) == "" {
		return nil
	}
	normalized := strings.ReplaceAll(diffText, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	var files []DiffFile
	var current *DiffFile
	var currentHunk *DiffHunk
	oldLine := 0
	newLine := 0

	flushHunk := func() {
		if current != nil && currentHunk != nil {
			current.Hunks = append(current.Hunks, *currentHunk)
			currentHunk = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if current != nil && current.Path != "" {
			files = append(files, *current)
		}
		current = nil
	}

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flushFile()
			oldPath, newPath := parseDiffGitHeader(line)
			path := newPath
			if path == "" || path == "/dev/null" {
				path = oldPath
			}
			current = &DiffFile{Path: path}

		case current == nil:
			continue

		case strings.HasPrefix(line, "new file mode "):
			current.IsNew = true

		case strings.HasPrefix(line, "deleted file mode "):
			current.IsDeleted = true

		case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
			current.IsBinary = true

		case strings.HasPrefix(line, "--- "):
			raw := strings.TrimSpace(strings.TrimPrefix(line, "--- "))
			if raw == "/dev/null" {
				current.IsNew = true
			} else if p := cleanDiffPath(raw, "a/"); p != "" && current.Path == "" {
				current.Path = p
			}

		case strings.HasPrefix(line, "+++ "):
			raw := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			if raw == "/dev/null" {
				current.IsDeleted = true
			} else if p := cleanDiffPath(raw, "b/"); p != "" {
				current.Path = p
			}

		case strings.HasPrefix(line, "@@ "):
			flushHunk()
			osStart, nsStart, ok := parseHunkHeader(line)
			if !ok {
				continue
			}
			oldLine = osStart
			newLine = nsStart
			currentHunk = &DiffHunk{OldStart: osStart, NewStart: nsStart}

		case currentHunk != nil:
			if strings.HasPrefix(line, "+") {
				currentHunk.Added = append(currentHunk.Added, DiffLine{Line: newLine, Text: line[1:]})
				newLine++
			} else if strings.HasPrefix(line, "-") {
				currentHunk.Removed = append(currentHunk.Removed, DiffLine{Line: oldLine, Text: line[1:]})
				oldLine++
			} else if strings.HasPrefix(line, " ") {
				oldLine++
				newLine++
			}
		}
	}
	flushFile()
	return files
}

// BuildDiffFileFromContent 把未跟踪新文件的全部内容构造为全量新增行的 DiffFile。
func BuildDiffFileFromContent(relPath, content string) DiffFile {
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	rawLines := splitRawLines(norm)
	added := make([]DiffLine, len(rawLines))
	for i, text := range rawLines {
		added[i] = DiffLine{Line: i + 1, Text: text}
	}
	df := DiffFile{
		Path:  filepath.ToSlash(filepath.Clean(relPath)),
		IsNew: true,
	}
	if len(added) > 0 {
		df.Hunks = []DiffHunk{{OldStart: 0, NewStart: 1, Added: added}}
	}
	return df
}

// LexDiffFile 对单个 DiffFile 的新增行（+）与删除行（-）进行词法剥离。
// 当 loader 能读取到完整的新/旧版本源码时，基于完整源码精确追踪多行字符串与块注释状态；
// 否则回退到按 hunk 内连续新增/删除行独立剥离。
func LexDiffFile(df DiffFile, loader DiffContentLoader) (addedLexed, removedLexed []LexedLine) {
	if df.IsBinary || len(df.Hunks) == 0 {
		return nil, nil
	}

	var newByLine map[int]LexedLine
	if !df.IsDeleted && loader != nil {
		if full, ok := loader(df.Path, true); ok {
			lexed := LexLines(df.Path, full)
			newByLine = make(map[int]LexedLine, len(lexed))
			for _, l := range lexed {
				newByLine[l.Line] = l
			}
		}
	}
	var oldByLine map[int]LexedLine
	if !df.IsNew && loader != nil {
		if full, ok := loader(df.Path, false); ok {
			lexed := LexLines(df.Path, full)
			oldByLine = make(map[int]LexedLine, len(lexed))
			for _, l := range lexed {
				oldByLine[l.Line] = l
			}
		}
	}

	for _, h := range df.Hunks {
		if len(h.Added) > 0 {
			if newByLine != nil {
				for _, al := range h.Added {
					if l, ok := newByLine[al.Line]; ok {
						addedLexed = append(addedLexed, l)
					} else {
						addedLexed = append(addedLexed, LexedLine{Line: al.Line, Code: al.Text, Raw: al.Text})
					}
				}
			} else {
				addedLexed = append(addedLexed, lexDiffLinesFallback(df.Path, h.Added)...)
			}
		}
		if len(h.Removed) > 0 {
			if oldByLine != nil {
				for _, rl := range h.Removed {
					if l, ok := oldByLine[rl.Line]; ok {
						removedLexed = append(removedLexed, l)
					} else {
						removedLexed = append(removedLexed, LexedLine{Line: rl.Line, Code: rl.Text, Raw: rl.Text})
					}
				}
			} else {
				removedLexed = append(removedLexed, lexDiffLinesFallback(df.Path, h.Removed)...)
			}
		}
	}
	return addedLexed, removedLexed
}

func lexDiffLinesFallback(filename string, lines []DiffLine) []LexedLine {
	if len(lines) == 0 {
		return nil
	}
	var sb strings.Builder
	for i, l := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(l.Text)
	}
	lexed := lexLinesWithStart(filename, sb.String(), 1)
	for i := range lexed {
		if i < len(lines) {
			lexed[i].Line = lines[i].Line
		}
	}
	return lexed
}

// AdapterEscapeRule 将逃生舱正则与其适用文件扩展名绑定，便于 diff 模式下按文件过滤匹配。
type AdapterEscapeRule struct {
	Extensions []string
	Patterns   []contract.EscapePattern
}

// DiffRatchetResult 描述基于统一 diff 的增量逃生舱净变化计算结果。
type DiffRatchetResult struct {
	Added        map[string]int
	Removed      map[string]int
	Delta        map[string]int
	NetEscapes   map[string]int
	Findings     []contract.Finding
	Improvements []contract.MetricDelta
}

// EvaluateDiffRatchet 直接基于 diff 新增行与删除行计算逃生舱 delta = added - removed：
// - delta > 0 产出 ratchet:<metric> 阻断 finding；
// - delta < 0 产出 Improvements 提示可执行 slopguard baseline --tighten。
func EvaluateDiffRatchet(
	files []DiffFile,
	rules []AdapterEscapeRule,
	baseline *Baseline,
	loader DiffContentLoader,
) (DiffRatchetResult, error) {
	allPatterns := make([]contract.EscapePattern, 0)
	seen := make(map[string]struct{})
	type compiledRule struct {
		extSet   map[string]struct{}
		compiled []compiledEscapePattern
	}
	compiledRules := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		extSet := make(map[string]struct{}, len(r.Extensions))
		for _, ext := range r.Extensions {
			ext = strings.ToLower(strings.TrimSpace(ext))
			if ext == "" {
				continue
			}
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			extSet[ext] = struct{}{}
		}
		comp, _, err := compileEscapePatterns(r.Patterns)
		if err != nil {
			return DiffRatchetResult{}, err
		}
		for _, p := range r.Patterns {
			if p.Name == "" {
				continue
			}
			if _, ok := seen[p.Name]; !ok {
				seen[p.Name] = struct{}{}
				allPatterns = append(allPatterns, p)
			}
		}
		compiledRules = append(compiledRules, compiledRule{extSet: extSet, compiled: comp})
	}

	added := make(map[string]int, len(allPatterns))
	removed := make(map[string]int, len(allPatterns))
	for _, p := range allPatterns {
		added[p.Name] = 0
		removed[p.Name] = 0
	}

	for _, df := range files {
		if df.IsBinary {
			continue
		}
		if baseline != nil && baseline.IsSuppressed(df.Path) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(df.Path))
		var matched []compiledEscapePattern
		matchedSeen := make(map[string]struct{})
		for _, cr := range compiledRules {
			if len(cr.extSet) > 0 {
				if _, ok := cr.extSet[ext]; !ok {
					continue
				}
			}
			for _, cp := range cr.compiled {
				if _, dup := matchedSeen[cp.name]; dup {
					continue
				}
				matchedSeen[cp.name] = struct{}{}
				matched = append(matched, cp)
			}
		}
		if len(matched) == 0 {
			continue
		}
		addedLexed, removedLexed := LexDiffFile(df, loader)
		countEscapesOnLexedLines(addedLexed, matched, added)
		countEscapesOnLexedLines(removedLexed, matched, removed)
	}

	deltaMap := make(map[string]int, len(allPatterns))
	netMap := make(map[string]int, len(allPatterns))
	var findings []contract.Finding
	var improvements []contract.MetricDelta

	for _, p := range allPatterns {
		d := added[p.Name] - removed[p.Name]
		deltaMap[p.Name] = d
		if d > 0 {
			netMap[p.Name] = d
			findings = append(findings, contract.Finding{
				Rule: "ratchet:" + p.Name,
				Message: fmt.Sprintf("escape hatch %s increased in diff: +%d (added %d, removed %d)",
					p.Name, d, added[p.Name], removed[p.Name]),
				Severity: contract.SeverityError,
			})
		} else {
			netMap[p.Name] = 0
			if d < 0 {
				baseVal := 0
				if baseline != nil {
					baseVal = baseline.Metrics[p.Name]
				}
				curVal := baseVal + d
				if curVal < 0 {
					curVal = 0
				}
				improvements = append(improvements, contract.MetricDelta{
					Name:     p.Name,
					Baseline: baseVal,
					Current:  curVal,
					Delta:    d,
				})
			}
		}
	}

	return DiffRatchetResult{
		Added:        added,
		Removed:      removed,
		Delta:        deltaMap,
		NetEscapes:   netMap,
		Findings:     findings,
		Improvements: improvements,
	}, nil
}

// ReadWorkingFileIfExists 读取工作区文件文本，用于 DiffContentLoader。
func ReadWorkingFileIfExists(root, slashPath string) (string, bool) {
	full := filepath.Join(root, filepath.FromSlash(slashPath))
	data, err := os.ReadFile(full)
	if err != nil {
		return "", false
	}
	return string(data), true
}

func parseDiffGitHeader(line string) (oldPath, newPath string) {
	rest := strings.TrimPrefix(line, "diff --git ")
	fields := splitDiffPathTokens(rest)
	if len(fields) >= 2 {
		return cleanDiffPath(fields[0], "a/"), cleanDiffPath(fields[1], "b/")
	}
	return "", ""
}

func splitDiffPathTokens(s string) []string {
	var tokens []string
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '"' {
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				if s[j] == '"' {
					j++
					break
				}
				j++
			}
			tokens = append(tokens, s[i:j])
			i = j
			continue
		}
		j := i
		for j < len(s) && s[j] != ' ' {
			j++
		}
		tokens = append(tokens, s[i:j])
		i = j
	}
	return tokens
}

func cleanDiffPath(raw, prefix string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "/dev/null" {
		return raw
	}
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		if unq, err := strconv.Unquote(raw); err == nil {
			raw = unq
		}
	}
	raw = strings.TrimPrefix(raw, prefix)
	return filepath.ToSlash(filepath.Clean(raw))
}

func parseHunkHeader(line string) (oldStart, newStart int, ok bool) {
	// 格式：@@ -old[,oldLen] +new[,newLen] @@[ optional context]
	rest := strings.TrimPrefix(line, "@@ ")
	end := strings.Index(rest, " @@")
	if end < 0 {
		return 0, 0, false
	}
	header := rest[:end]
	parts := strings.Fields(header)
	if len(parts) < 2 {
		return 0, 0, false
	}
	oldPart := strings.TrimPrefix(parts[0], "-")
	newPart := strings.TrimPrefix(parts[1], "+")
	osNum, err1 := parseRangeStart(oldPart)
	nsNum, err2 := parseRangeStart(newPart)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return osNum, nsNum, true
}

func parseRangeStart(part string) (int, error) {
	if idx := strings.IndexByte(part, ','); idx >= 0 {
		part = part[:idx]
	}
	return strconv.Atoi(part)
}
