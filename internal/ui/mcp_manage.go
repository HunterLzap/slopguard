package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/skills"
)

type mcpClientStatus struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	ConfigPath  string `json:"config_path"`
	Detected    bool   `json:"detected"`
	Configured  bool   `json:"configured"`
	Scope       string `json:"scope"`
	Recommended bool   `json:"recommended"`
	HasBackup   bool   `json:"has_backup"`
	BackupAt    string `json:"backup_at,omitempty"`
}

type skillTargetStatus struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	ConfigPath  string `json:"config_path"`
	Detected    bool   `json:"detected"`
	Configured  bool   `json:"configured"`
	Scope       string `json:"scope"`
	Recommended bool   `json:"recommended"`
	HasBackup   bool   `json:"has_backup"`
	BackupAt    string `json:"backup_at,omitempty"`
}

type configBackupRecord struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	TargetPath    string `json:"target_path"`
	Existed       bool   `json:"existed"`
	OriginalMode  uint32 `json:"original_mode"`
	OriginalBytes []byte `json:"original_bytes,omitempty"`
	BackupAt      string `json:"backup_at"`
}

func sanitizeBackupToken(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "target"
	}
	return out
}

func backupRecordPath(backupsDir, kind, id, targetPath string) string {
	clean := filepath.Clean(targetPath)
	sum := sha256.Sum256([]byte(clean))
	short := hex.EncodeToString(sum[:6])
	name := fmt.Sprintf("%s-%s-%s.json", sanitizeBackupToken(kind), sanitizeBackupToken(id), short)
	return filepath.Join(backupsDir, name)
}

func lookupBackup(backupsDir, kind, id, targetPath string) (string, configBackupRecord, bool) {
	if strings.TrimSpace(backupsDir) == "" || strings.TrimSpace(targetPath) == "" {
		return "", configBackupRecord{}, false
	}
	path := backupRecordPath(backupsDir, kind, id, targetPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", configBackupRecord{}, false
	}
	var rec configBackupRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return "", configBackupRecord{}, false
	}
	return path, rec, true
}

// ensureBackup creates a backup of targetPath before SlopGuard modifies it.
// If a backup already exists for this target, it is preserved untouched so
// repeated updates never overwrite the pre-SlopGuard original state.
func ensureBackup(backupsDir, kind, id, targetPath string) (string, configBackupRecord, error) {
	if strings.TrimSpace(backupsDir) == "" {
		return "", configBackupRecord{}, errors.New("无法定位备份目录")
	}
	if existingPath, existingRec, ok := lookupBackup(backupsDir, kind, id, targetPath); ok {
		return existingPath, existingRec, nil
	}
	rec := configBackupRecord{
		ID:         id,
		Kind:       kind,
		TargetPath: filepath.Clean(targetPath),
		BackupAt:   time.Now().UTC().Format(time.RFC3339),
	}
	info, err := os.Stat(targetPath)
	switch {
	case err == nil:
		data, readErr := os.ReadFile(targetPath)
		if readErr != nil {
			return "", configBackupRecord{}, fmt.Errorf("读取待备份文件失败: %w", readErr)
		}
		rec.Existed = true
		rec.OriginalMode = uint32(info.Mode().Perm())
		rec.OriginalBytes = data
	case os.IsNotExist(err):
		rec.Existed = false
		rec.OriginalMode = 0o644
	default:
		return "", configBackupRecord{}, fmt.Errorf("检查待备份文件失败: %w", err)
	}

	encoded, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", configBackupRecord{}, err
	}
	encoded = append(encoded, '\n')
	backupPath := backupRecordPath(backupsDir, kind, id, targetPath)
	if err := writeFileAtomic(backupPath, encoded, 0o644); err != nil {
		return "", configBackupRecord{}, fmt.Errorf("保存备份文件失败: %w", err)
	}
	return backupPath, rec, nil
}

func restoreFromBackup(backupsDir, kind, id, targetPath string) (restoredFromBackup bool, removedFile bool, err error) {
	backupPath, rec, ok := lookupBackup(backupsDir, kind, id, targetPath)
	if !ok {
		return false, false, nil
	}
	if rec.Existed {
		mode := os.FileMode(rec.OriginalMode)
		if mode == 0 {
			mode = 0o644
		}
		if err := writeFileAtomic(targetPath, rec.OriginalBytes, mode); err != nil {
			return false, false, fmt.Errorf("还原原文件失败: %w", err)
		}
	} else {
		if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
			return false, false, fmt.Errorf("删除新建配置文件失败: %w", err)
		}
		removedFile = true
		cleanupEmptySkillLeafDir(kind, targetPath)
	}
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return true, removedFile, fmt.Errorf("清理备份元数据失败: %w", err)
	}
	return true, removedFile, nil
}

func cleanupEmptySkillLeafDir(kind, targetPath string) {
	if kind != "skill" {
		return
	}
	parent := filepath.Dir(targetPath)
	if filepath.Base(parent) == "slopguard" {
		_ = os.Remove(parent)
	}
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".slopguard-tmp-*")
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

func discoverMCPClients(home, project string) []mcpClientStatus {
	project = strings.TrimSpace(project)
	backupsDir := ""
	if home != "" {
		backupsDir = filepath.Join(home, ".slopguard", "backups")
	}

	var list []mcpClientStatus
	add := func(id, name, path, scope string, alwaysDetect, recommended bool) {
		if path == "" {
			return
		}
		_, err := os.Stat(path)
		_, parentErr := os.Stat(filepath.Dir(path))
		detected := alwaysDetect || err == nil || parentErr == nil
		_, rec, hasBackup := lookupBackup(backupsDir, "mcp", id, path)
		list = append(list, mcpClientStatus{
			ID:          id,
			Kind:        "mcp",
			Name:        name,
			ConfigPath:  path,
			Detected:    detected,
			Configured:  isSlopGuardConfigured(path),
			Scope:       scope,
			Recommended: recommended,
			HasBackup:   hasBackup,
			BackupAt:    rec.BackupAt,
		})
	}

	// Project-level .mcp.json takes priority.
	if project != "" {
		add("project_mcp", "当前项目 (.mcp.json)", filepath.Join(project, ".mcp.json"), "project", true, true)
	}
	if home != "" {
		add("antigravity", "Antigravity (全局)", filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), "global", false, false)
		add("claude_code", "Claude Code (全局)", filepath.Join(home, ".claude.json"), "global", false, false)
		add("cursor", "Cursor (全局)", filepath.Join(home, ".cursor", "mcp.json"), "global", false, false)
	}
	return list
}

func mcpArgsForClient(clientID, project string) []string {
	if clientID == "project_mcp" && strings.TrimSpace(project) != "" {
		return []string{"mcp", "--root", strings.TrimSpace(project)}
	}
	return []string{"mcp"}
}

func isSlopGuardConfigured(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	servers, ok := raw["mcpServers"].(map[string]any)
	if !ok {
		return false
	}
	_, exists := servers["slopguard"]
	return exists
}

func renderMCPUpsert(path, binaryPath string, args []string) (oldText, newText string, existed bool, err error) {
	root := map[string]any{}
	if data, readErr := os.ReadFile(path); readErr == nil {
		existed = true
		oldText = string(data)
		if len(strings.TrimSpace(oldText)) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return "", "", true, fmt.Errorf("现有配置文件 %s 不是合法 JSON: %w", path, err)
			}
		}
	} else if !os.IsNotExist(readErr) {
		return "", "", false, readErr
	}

	servers, ok := root["mcpServers"].(map[string]any)
	if !ok || servers == nil {
		servers = map[string]any{}
	}
	servers["slopguard"] = map[string]any{
		"command": binaryPath,
		"args":    args,
	}
	root["mcpServers"] = servers

	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", "", existed, err
	}
	encoded = append(encoded, '\n')
	return oldText, string(encoded), existed, nil
}

func renderMCPRemove(path string) (oldText, newText string, removeFile bool, err error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return "", "", false, nil
		}
		return "", "", false, readErr
	}
	oldText = string(data)
	if len(strings.TrimSpace(oldText)) == 0 {
		return oldText, "", true, nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return "", "", false, fmt.Errorf("现有配置文件 %s 不是合法 JSON: %w", path, err)
	}
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		return oldText, oldText, false, nil
	}
	if _, exists := servers["slopguard"]; !exists {
		return oldText, oldText, false, nil
	}
	delete(servers, "slopguard")
	if len(servers) == 0 {
		delete(root, "mcpServers")
	} else {
		root["mcpServers"] = servers
	}
	if len(root) == 0 {
		return oldText, "", true, nil
	}
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", "", false, err
	}
	encoded = append(encoded, '\n')
	return oldText, string(encoded), false, nil
}

func discoverSkillTargets(home, project string) []skillTargetStatus {
	project = strings.TrimSpace(project)
	backupsDir := ""
	if home != "" {
		backupsDir = filepath.Join(home, ".slopguard", "backups")
	}

	var list []skillTargetStatus
	add := func(id, name, path, scope string, detected, recommended bool) {
		if path == "" {
			return
		}
		_, rec, hasBackup := lookupBackup(backupsDir, "skill", id, path)
		list = append(list, skillTargetStatus{
			ID:          id,
			Kind:        "skill",
			Name:        name,
			ConfigPath:  path,
			Detected:    detected,
			Configured:  isSkillConfigured(path),
			Scope:       scope,
			Recommended: recommended,
			HasBackup:   hasBackup,
			BackupAt:    rec.BackupAt,
		})
	}

	if project != "" {
		add(
			"project_claude_skill",
			"当前项目 Skill (.claude/skills)",
			filepath.Join(project, ".claude", "skills", "slopguard", "SKILL.md"),
			"project",
			true,
			true,
		)
		add(
			"project_cursor_rule",
			"当前项目 Cursor Rule (.cursor/rules)",
			filepath.Join(project, ".cursor", "rules", "slopguard.mdc"),
			"project",
			true,
			false,
		)
	}
	if home != "" {
		dirExists := func(p string) bool {
			info, err := os.Stat(p)
			return err == nil && info.IsDir()
		}
		add(
			"global_antigravity_skill",
			"Antigravity Skill (全局)",
			filepath.Join(home, ".gemini", "antigravity", "skills", "slopguard", "SKILL.md"),
			"global",
			dirExists(filepath.Join(home, ".gemini", "antigravity")),
			false,
		)
		add(
			"global_claude_skill",
			"Claude Code Skill (全局)",
			filepath.Join(home, ".claude", "skills", "slopguard", "SKILL.md"),
			"global",
			dirExists(filepath.Join(home, ".claude")),
			false,
		)
		add(
			"global_codex_skill",
			"Codex Skill (全局)",
			filepath.Join(home, ".codex", "skills", "slopguard", "SKILL.md"),
			"global",
			dirExists(filepath.Join(home, ".codex")),
			false,
		)
	}
	return list
}

func isSkillConfigured(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	text := string(data)
	return strings.Contains(text, "SlopGuard") && strings.Contains(text, "sg_verify")
}

func renderSkillContent(targetID string) string {
	doc := strings.TrimSpace(skills.Doc)
	if doc == "" {
		doc = "# SlopGuard\n\nUse sg_verify before finishing edits.\n"
	}
	if targetID == "project_cursor_rule" {
		if strings.HasPrefix(doc, "---\n") {
			return doc + "\n"
		}
		return "---\ndescription: SlopGuard deterministic code-quality guardrail MCP workflow\nalwaysApply: false\n---\n\n" + doc + "\n"
	}
	if strings.HasPrefix(doc, "---\n") {
		return doc + "\n"
	}
	return "---\nname: slopguard\ndescription: SlopGuard deterministic code-quality guardrail MCP workflow and mistake inbox analysis guide.\n---\n\n" + doc + "\n"
}

type diffOpKind int

const (
	diffEqual diffOpKind = iota
	diffDelete
	diffInsert
)

type diffOp struct {
	kind diffOpKind
	text string
}

// buildUnifiedDiff generates a readable unified diff between oldText and newText.
func buildUnifiedDiff(displayPath, oldText, newText string, contextLines int) string {
	if oldText == newText {
		return ""
	}
	if contextLines <= 0 {
		contextLines = 3
	}
	oldLines := splitUnifiedLines(oldText)
	newLines := splitUnifiedLines(newText)

	var ops []diffOp
	if len(oldLines)+len(newLines) > 4000 || len(oldLines)*len(newLines) > 1_000_000 {
		ops = boundedFallbackDiffOps(oldLines, newLines)
	} else {
		ops = lcsDiffOps(oldLines, newLines)
	}

	oldHeader := "--- a/" + displayPath
	newHeader := "+++ b/" + displayPath
	if len(oldLines) == 0 {
		oldHeader = "--- /dev/null"
	}
	if len(newLines) == 0 {
		newHeader = "+++ /dev/null"
	}

	var b strings.Builder
	b.WriteString(oldHeader)
	b.WriteByte('\n')
	b.WriteString(newHeader)
	b.WriteByte('\n')

	hunks := groupDiffHunks(ops, contextLines)
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart, h.oldCount, h.newStart, h.newCount)
		for _, op := range h.ops {
			switch op.kind {
			case diffEqual:
				b.WriteByte(' ')
			case diffDelete:
				b.WriteByte('-')
			case diffInsert:
				b.WriteByte('+')
			}
			b.WriteString(op.text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func splitUnifiedLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func lcsDiffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// Strip common prefix and suffix to keep DP compact.
	prefix := 0
	for prefix < n && prefix < m && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < (n-prefix) && suffix < (m-prefix) && a[n-1-suffix] == b[m-1-suffix] {
		suffix++
	}

	ops := make([]diffOp, 0, n+m)
	for i := 0; i < prefix; i++ {
		ops = append(ops, diffOp{kind: diffEqual, text: a[i]})
	}

	midA := a[prefix : n-suffix]
	midB := b[prefix : m-suffix]
	ma, mb := len(midA), len(midB)

	if ma > 0 || mb > 0 {
		dp := make([][]int, ma+1)
		for i := range dp {
			dp[i] = make([]int, mb+1)
		}
		for i := ma - 1; i >= 0; i-- {
			for j := mb - 1; j >= 0; j-- {
				if midA[i] == midB[j] {
					dp[i][j] = dp[i+1][j+1] + 1
				} else if dp[i+1][j] >= dp[i][j+1] {
					dp[i][j] = dp[i+1][j]
				} else {
					dp[i][j] = dp[i][j+1]
				}
			}
		}
		i, j := 0, 0
		for i < ma && j < mb {
			switch {
			case midA[i] == midB[j]:
				ops = append(ops, diffOp{kind: diffEqual, text: midA[i]})
				i++
				j++
			case dp[i+1][j] >= dp[i][j+1]:
				ops = append(ops, diffOp{kind: diffDelete, text: midA[i]})
				i++
			default:
				ops = append(ops, diffOp{kind: diffInsert, text: midB[j]})
				j++
			}
		}
		for ; i < ma; i++ {
			ops = append(ops, diffOp{kind: diffDelete, text: midA[i]})
		}
		for ; j < mb; j++ {
			ops = append(ops, diffOp{kind: diffInsert, text: midB[j]})
		}
	}

	for i := n - suffix; i < n; i++ {
		ops = append(ops, diffOp{kind: diffEqual, text: a[i]})
	}
	return ops
}

func boundedFallbackDiffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	prefix := 0
	for prefix < n && prefix < m && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < (n-prefix) && suffix < (m-prefix) && a[n-1-suffix] == b[m-1-suffix] {
		suffix++
	}
	ops := make([]diffOp, 0, n+m)
	for i := 0; i < prefix; i++ {
		ops = append(ops, diffOp{kind: diffEqual, text: a[i]})
	}
	for i := prefix; i < n-suffix; i++ {
		ops = append(ops, diffOp{kind: diffDelete, text: a[i]})
	}
	for j := prefix; j < m-suffix; j++ {
		ops = append(ops, diffOp{kind: diffInsert, text: b[j]})
	}
	for i := n - suffix; i < n; i++ {
		ops = append(ops, diffOp{kind: diffEqual, text: a[i]})
	}
	return ops
}

type diffHunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	ops      []diffOp
}

func groupDiffHunks(ops []diffOp, contextLines int) []diffHunk {
	type indexedOp struct {
		op      diffOp
		oldLine int
		newLine int
	}
	indexed := make([]indexedOp, len(ops))
	oldLine, newLine := 1, 1
	var changedIdx []int
	for i, op := range ops {
		indexed[i] = indexedOp{op: op, oldLine: oldLine, newLine: newLine}
		switch op.kind {
		case diffEqual:
			oldLine++
			newLine++
		case diffDelete:
			oldLine++
			changedIdx = append(changedIdx, i)
		case diffInsert:
			newLine++
			changedIdx = append(changedIdx, i)
		}
	}
	if len(changedIdx) == 0 {
		return nil
	}

	type span struct{ start, end int }
	var spans []span
	for _, idx := range changedIdx {
		s := idx - contextLines
		if s < 0 {
			s = 0
		}
		e := idx + contextLines + 1
		if e > len(indexed) {
			e = len(indexed)
		}
		if len(spans) > 0 && s <= spans[len(spans)-1].end {
			spans[len(spans)-1].end = e
		} else {
			spans = append(spans, span{start: s, end: e})
		}
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	hunks := make([]diffHunk, 0, len(spans))
	for _, sp := range spans {
		slice := indexed[sp.start:sp.end]
		h := diffHunk{
			oldStart: slice[0].oldLine,
			newStart: slice[0].newLine,
			ops:      make([]diffOp, 0, len(slice)),
		}
		for _, item := range slice {
			h.ops = append(h.ops, item.op)
			switch item.op.kind {
			case diffEqual:
				h.oldCount++
				h.newCount++
			case diffDelete:
				h.oldCount++
			case diffInsert:
				h.newCount++
			}
		}
		if h.oldCount == 0 {
			h.oldStart--
		}
		if h.newCount == 0 {
			h.newStart--
		}
		hunks = append(hunks, h)
	}
	return hunks
}
