package ui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/store"
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
		detected := alwaysDetect || err == nil
		if !detected && filepath.Dir(path) != home {
			_, parentErr := os.Stat(filepath.Dir(path))
			detected = parentErr == nil
		}
		if id == "claude_code" && !detected && home != "" {
			_, claudeDirErr := os.Stat(filepath.Join(home, ".claude"))
			detected = claudeDirErr == nil || lookupAgentCLIBinary(home, "claude") != ""
		}
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
		add("codex", "Codex (全局)", filepath.Join(home, ".codex", "config.toml"), "global", false, false)
	}
	return list
}

func mcpArgsForClient(clientID, project string) []string {
	if clientID == "project_mcp" && strings.TrimSpace(project) != "" {
		return []string{"mcp", "--root", strings.TrimSpace(project)}
	}
	return []string{"mcp"}
}

func isTOMLPath(path string) bool {
	return strings.EqualFold(filepath.Ext(strings.TrimSpace(path)), ".toml")
}

func parseCodexSlopGuardBlock(text string) (cmd string, args []string, found bool) {
	lines := strings.Split(text, "\n")
	inSection := false
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			sec := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if sec == "mcp_servers.slopguard" {
				inSection = true
				found = true
				continue
			}
			if inSection {
				break
			}
			continue
		}
		if !inSection || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if eq := strings.IndexByte(trimmed, '='); eq > 0 {
			key := strings.TrimSpace(trimmed[:eq])
			val := strings.TrimSpace(trimmed[eq+1:])
			switch key {
			case "command":
				if unq, err := strconv.Unquote(val); err == nil {
					cmd = unq
				} else {
					cmd = strings.Trim(val, `"'`)
				}
			case "args":
				var parsed []string
				if err := json.Unmarshal([]byte(val), &parsed); err == nil {
					args = parsed
				}
			}
		}
	}
	return cmd, args, found
}

func readConfiguredMCPCommand(path string) (command string, args []string, configured bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, false
	}
	if isTOMLPath(path) {
		cmd, parsedArgs, found := parseCodexSlopGuardBlock(string(data))
		return cmd, parsedArgs, found && cmd != ""
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", nil, false
	}
	servers, ok := raw["mcpServers"].(map[string]any)
	if !ok {
		return "", nil, false
	}
	entry, exists := servers["slopguard"].(map[string]any)
	if !exists {
		return "", nil, false
	}
	cmd, _ := entry["command"].(string)
	if rawArgs, ok := entry["args"].([]any); ok {
		for _, a := range rawArgs {
			if s, ok := a.(string); ok {
				args = append(args, s)
			}
		}
	}
	return strings.TrimSpace(cmd), args, strings.TrimSpace(cmd) != ""
}

func isExecutableFile(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return false
	}
	return true
}

func isSlopGuardConfigured(path string) bool {
	_, _, configured := readConfiguredMCPCommand(path)
	return configured
}

func formatCodexSlopGuardSection(binaryPath string, args []string) string {
	argsJSON, _ := json.Marshal(args)
	return fmt.Sprintf("[mcp_servers.slopguard]\ncommand = %s\nargs = %s\n", strconv.Quote(binaryPath), string(argsJSON))
}

func replaceOrRemoveCodexSection(oldText, replacement string, remove bool) string {
	normalized := strings.ReplaceAll(oldText, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	var out []string
	inSection := false
	replaced := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			sec := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if sec == "mcp_servers.slopguard" {
				inSection = true
				if !remove && !replaced {
					out = append(out, strings.TrimRight(replacement, "\n"))
					replaced = true
				}
				continue
			}
			inSection = false
		}
		if inSection {
			continue
		}
		out = append(out, line)
	}
	if !remove && !replaced {
		joined := strings.TrimRight(strings.Join(out, "\n"), "\n")
		if joined == "" {
			return replacement
		}
		return joined + "\n\n" + replacement
	}
	res := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if res == "" {
		return ""
	}
	return res + "\n"
}

func renderMCPUpsert(path, binaryPath string, args []string) (oldText, newText string, existed bool, err error) {
	if isTOMLPath(path) {
		if data, readErr := os.ReadFile(path); readErr == nil {
			existed = true
			oldText = string(data)
		} else if !os.IsNotExist(readErr) {
			return "", "", false, readErr
		}
		section := formatCodexSlopGuardSection(binaryPath, args)
		return oldText, replaceOrRemoveCodexSection(oldText, section, false), existed, nil
	}

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
	if isTOMLPath(path) {
		newText = replaceOrRemoveCodexSection(oldText, "", true)
		return oldText, newText, strings.TrimSpace(newText) == "", nil
	}
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

func sharedSkillPath(home string) string {
	home = strings.TrimSpace(home)
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".slopguard", "shared-skill.md")
}

func defaultSharedSkillContent() string {
	doc := strings.TrimSpace(skills.Doc)
	if doc == "" {
		doc = "# SlopGuard\n\nUse sg_verify before finishing edits."
	}
	return doc + "\n"
}

func loadSharedSkill(home string) (content string, isCustom bool, updatedAt string) {
	path := sharedSkillPath(home)
	if path != "" {
		if info, err := os.Stat(path); err == nil {
			if data, readErr := os.ReadFile(path); readErr == nil && strings.TrimSpace(string(data)) != "" {
				return strings.TrimSpace(string(data)) + "\n", true, info.ModTime().UTC().Format(time.RFC3339)
			}
		}
	}
	return defaultSharedSkillContent(), false, ""
}

func renderSkillContent(home, targetID string) string {
	doc, _, _ := loadSharedSkill(home)
	doc = strings.TrimSpace(doc)
	if doc == "" {
		doc = "# SlopGuard\n\nUse sg_verify before finishing edits."
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

type agentProfile struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Badge        string             `json:"badge"`
	Scope        string             `json:"scope"`
	Recommended  bool               `json:"recommended"`
	Detected     bool               `json:"detected"`
	Enabled      bool               `json:"enabled"`
	Partial      bool               `json:"partial"`
	HasBackup    bool               `json:"has_backup"`
	NeedsProject bool               `json:"needs_project"`
	BinaryOK     bool               `json:"binary_ok"`
	HealthStatus string             `json:"health_status"` // not_configured | broken | pending_activation | active
	HealthDetail string             `json:"health_detail,omitempty"`
	LastEventAt  string             `json:"last_event_at,omitempty"`
	PrimaryPath  string             `json:"primary_path"`
	Note         string             `json:"note,omitempty"`
	MCPTarget    *mcpClientStatus   `json:"mcp_target,omitempty"`
	SkillTarget  *skillTargetStatus `json:"skill_target,omitempty"`
}

func detectClientInstalled(home, agentID string) bool {
	switch agentID {
	case "project":
		return true
	case "antigravity":
		if _, err := os.Stat("/Applications/Antigravity.app"); err == nil {
			return true
		}
		if lookupAgentCLIBinary(home, "agy") != "" {
			return true
		}
		_, err := os.Stat(filepath.Join(home, ".gemini", "antigravity"))
		return err == nil
	case "claude_code":
		if lookupAgentCLIBinary(home, "claude") != "" {
			return true
		}
		if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
			return true
		}
		_, err := os.Stat(filepath.Join(home, ".claude.json"))
		return err == nil
	case "cursor":
		if _, err := os.Stat("/Applications/Cursor.app"); err == nil {
			return true
		}
		_, err := os.Stat(filepath.Join(home, ".cursor"))
		return err == nil
	case "codex":
		if lookupAgentCLIBinary(home, "codex") != "" {
			return true
		}
		_, err := os.Stat(filepath.Join(home, ".codex"))
		return err == nil
	default:
		return false
	}
}

func isSlopGuardInAgyMCPList(home string) bool {
	agyBin := lookupAgentCLIBinary(home, "agy")
	if agyBin == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, agyBin, "mcp", "list")
	cmd.Env = augmentedCLIEnv(home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "slopguard" && strings.EqualFold(fields[2], "enabled") {
			return true
		}
	}
	return false
}

func syncAgyMCPRegistry(home, exe string, enable bool) {
	agyBin := lookupAgentCLIBinary(home, "agy")
	if agyBin == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Remove first so re-adding updates path cleanly when SlopGuard.app moves.
	rmCmd := exec.CommandContext(ctx, agyBin, "mcp", "remove", "slopguard")
	rmCmd.Env = augmentedCLIEnv(home)
	_ = rmCmd.Run()
	if !enable || strings.TrimSpace(exe) == "" {
		return
	}
	addCmd := exec.CommandContext(ctx, agyBin, "mcp", "add", "slopguard", exe, "mcp")
	addCmd.Env = augmentedCLIEnv(home)
	_ = addCmd.Run()
}

func discoverAgentProfiles(home, project string) []agentProfile {
	mcpList := discoverMCPClients(home, project)
	skillList := discoverSkillTargets(home, project)

	mcpByID := make(map[string]mcpClientStatus, len(mcpList))
	for _, m := range mcpList {
		mcpByID[m.ID] = m
	}
	skillByID := make(map[string]skillTargetStatus, len(skillList))
	for _, sk := range skillList {
		skillByID[sk.ID] = sk
	}

	build := func(id, name, badge, scope string, recommended bool, mcpID, skillID, note string) agentProfile {
		prof := agentProfile{
			ID:          id,
			Name:        name,
			Badge:       badge,
			Scope:       scope,
			Recommended: recommended,
			Detected:    detectClientInstalled(home, id),
			Note:        note,
		}
		var total, configured int
		if mcpID != "" {
			if m, ok := mcpByID[mcpID]; ok {
				cp := m
				prof.MCPTarget = &cp
				total++
				if m.Configured {
					configured++
					cmd, _, _ := readConfiguredMCPCommand(m.ConfigPath)
					prof.BinaryOK = isExecutableFile(cmd)
				}
				prof.Detected = prof.Detected || m.Detected
				prof.HasBackup = prof.HasBackup || m.HasBackup
				if prof.PrimaryPath == "" {
					prof.PrimaryPath = m.ConfigPath
				}
			}
		}
		if skillID != "" {
			if sk, ok := skillByID[skillID]; ok {
				cp := sk
				prof.SkillTarget = &cp
				total++
				if sk.Configured {
					configured++
				}
				prof.Detected = prof.Detected || sk.Detected
				prof.HasBackup = prof.HasBackup || sk.HasBackup
				if prof.PrimaryPath == "" {
					prof.PrimaryPath = sk.ConfigPath
				}
			}
		}
		if scope == "project" && strings.TrimSpace(project) == "" {
			prof.NeedsProject = true
			prof.PrimaryPath = "请先在顶部下拉框选择或添加项目"
		}
		prof.Enabled = total > 0 && configured == total
		prof.Partial = configured > 0 && configured < total
		switch {
		case !prof.Enabled && !prof.Partial:
			prof.HealthStatus = "not_configured"
			prof.HealthDetail = "尚未写入 MCP 与护栏规则"
		case prof.Partial:
			prof.HealthStatus = "broken"
			prof.HealthDetail = "仅配置了部分文件（缺 MCP 或缺 Skill），建议重新开关补全"
		case prof.MCPTarget != nil && !prof.BinaryOK:
			prof.HealthStatus = "broken"
			prof.HealthDetail = "配置指向的 slopguard 可执行文件不存在或无执行权限，请重新开关修复"
		default:
			prof.HealthStatus = "pending_activation"
			prof.HealthDetail = "配置已就绪，点击「自检」验证 MCP 握手与客户端激活状态"
		}
		return prof
	}

	return []agentProfile{
		build("project", "当前项目", "PR", "project", true, "project_mcp", "project_claude_skill", "推荐 · 仅作用于当前仓库（.mcp.json + Skill）"),
		build("antigravity", "Antigravity", "AG", "global", false, "antigravity", "global_antigravity_skill", "全局 MCP + Skill 护栏规则"),
		build("claude_code", "Claude Code", "CC", "global", false, "claude_code", "global_claude_skill", "全局 MCP + Skill 护栏规则"),
		build("cursor", "Cursor", "CU", "global", false, "cursor", "", "全局 MCP 服务"),
		build("codex", "Codex", "CX", "global", false, "codex", "global_codex_skill", "全局 MCP (config.toml) + Skill"),
	}
}

type AgentVerifyReport struct {
	AgentID       string   `json:"agent_id"`
	Name          string   `json:"name"`
	Installed     bool     `json:"installed"`
	Configured    bool     `json:"configured"`
	BinaryPath    string   `json:"binary_path,omitempty"`
	BinaryOK      bool     `json:"binary_ok"`
	SkillOK       bool     `json:"skill_ok"`
	HandshakeOK   bool     `json:"handshake_ok"`
	LatencyMS     int64    `json:"latency_ms,omitempty"`
	Tools         []string `json:"tools,omitempty"`
	CLIRegistered *bool    `json:"cli_registered,omitempty"`
	EventCount    int      `json:"event_count"`
	LastEventAt   string   `json:"last_event_at,omitempty"`
	HealthStatus  string   `json:"health_status"`
	Diagnosis     string   `json:"diagnosis"`
}

func runStdioMCPHandshake(ctx context.Context, binaryPath string, args []string) (tools []string, latencyMS int64, err error) {
	if !isExecutableFile(binaryPath) {
		return nil, 0, errors.New("MCP 可执行文件不存在或不可执行")
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(callCtx, binaryPath, args...)
	var stdinBuf bytes.Buffer
	stdinBuf.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"slopguard-selfcheck","version":"1.0"}}}` + "\n")
	stdinBuf.WriteString(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n")
	cmd.Stdin = &stdinBuf

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	if runErr := cmd.Run(); runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return nil, time.Since(started).Milliseconds(), fmt.Errorf("MCP 进程启动失败: %s", msg)
	}
	latencyMS = time.Since(started).Milliseconds()

	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var resp struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &resp) != nil {
			continue
		}
		if resp.Error != nil && resp.Error.Message != "" {
			return nil, latencyMS, errors.New(resp.Error.Message)
		}
		if resp.ID == 2 && len(resp.Result) > 0 {
			var listRes struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			if json.Unmarshal(resp.Result, &listRes) == nil {
				for _, t := range listRes.Tools {
					if t.Name != "" {
						tools = append(tools, t.Name)
					}
				}
			}
		}
	}
	if len(tools) == 0 {
		return nil, latencyMS, errors.New("MCP 握手未返回任何工具列表")
	}
	return tools, latencyMS, nil
}

func verifyAgentProfile(ctx context.Context, home, project string, prof agentProfile, db *store.Store, checkRealCLI bool) AgentVerifyReport {
	rep := AgentVerifyReport{
		AgentID:   prof.ID,
		Name:      prof.Name,
		Installed: prof.Detected,
		SkillOK:   prof.SkillTarget == nil || prof.SkillTarget.Configured,
	}

	if db != nil {
		queryProj := ""
		if prof.Scope == "project" {
			queryProj = project
		}
		if events, err := db.ListMCPEvents(ctx, queryProj, "", 50); err == nil && len(events) > 0 {
			rep.EventCount = len(events)
			rep.LastEventAt = events[0].CreatedAt.UTC().Format(time.RFC3339)
		}
	}

	if prof.NeedsProject {
		rep.HealthStatus = "not_configured"
		rep.Diagnosis = "请先在顶部下拉框选择或添加项目"
		return rep
	}
	if prof.MCPTarget == nil {
		if rep.SkillOK {
			rep.Configured = true
			rep.HealthStatus = "active"
			rep.Diagnosis = "规则文件已部署就绪"
		} else {
			rep.HealthStatus = "not_configured"
			rep.Diagnosis = "尚未部署规则文件"
		}
		return rep
	}

	cmd, args, configured := readConfiguredMCPCommand(prof.MCPTarget.ConfigPath)
	rep.Configured = configured
	rep.BinaryPath = cmd
	rep.BinaryOK = configured && isExecutableFile(cmd)

	if !configured {
		rep.HealthStatus = "not_configured"
		rep.Diagnosis = "尚未开启接入"
		return rep
	}
	if !rep.BinaryOK {
		rep.HealthStatus = "broken"
		rep.Diagnosis = fmt.Sprintf("配置里的命令不可用 (%s)，请关闭后重新开启接入修复路径", cmd)
		return rep
	}
	if !rep.SkillOK {
		rep.HealthStatus = "broken"
		rep.Diagnosis = "MCP 已写但缺少 SKILL 规则文件，建议重新开关补全"
		return rep
	}

	tools, latencyMS, hsErr := runStdioMCPHandshake(ctx, cmd, args)
	rep.LatencyMS = latencyMS
	rep.Tools = tools
	rep.HandshakeOK = hsErr == nil
	if hsErr != nil {
		rep.HealthStatus = "broken"
		rep.Diagnosis = hsErr.Error()
		return rep
	}

	cliRegistered := false
	if prof.ID == "antigravity" && checkRealCLI {
		reg := isSlopGuardInAgyMCPList(home)
		rep.CLIRegistered = &reg
		cliRegistered = reg
	}

	if rep.EventCount > 0 || cliRegistered {
		rep.HealthStatus = "active"
		if rep.EventCount > 0 {
			rep.Diagnosis = fmt.Sprintf("MCP 握手通过 (%dms, %d个工具) · 已记录 %d 次真实调用", latencyMS, len(tools), rep.EventCount)
		} else {
			rep.Diagnosis = fmt.Sprintf("MCP 握手通过 (%dms, %d个工具) · 客户端 CLI 已注册", latencyMS, len(tools))
		}
		return rep
	}

	rep.HealthStatus = "pending_activation"
	rep.Diagnosis = fmt.Sprintf("MCP 握手通过 (%dms, %d个工具) · 请重启「%s」并运行一次任务完成激活", latencyMS, len(tools), prof.Name)
	return rep
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
