package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const (
	skillManagedStartMarker = "<!-- slopguard:begin -->"
	skillManagedEndMarker   = "<!-- slopguard:end -->"
)

type targetSnapshot struct {
	Data    []byte
	Existed bool
	Mode    os.FileMode
}

func readTargetSnapshot(path string) (targetSnapshot, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return targetSnapshot{}, nil
		}
		return targetSnapshot{}, err
	}
	if info.IsDir() {
		return targetSnapshot{}, fmt.Errorf("目标不是文件: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return targetSnapshot{}, err
	}
	return targetSnapshot{Data: data, Existed: true, Mode: info.Mode().Perm()}, nil
}

func resolveTargetRealPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	abs = filepath.Clean(abs)
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil && resolved != "" {
		return filepath.Clean(resolved)
	}
	// Walk upward to the deepest existing ancestor so non-existent parent/leaf paths
	// on macOS (/var -> /private/var) resolve identically before and after file creation.
	var tail []string
	curr := abs
	for {
		parent := filepath.Dir(curr)
		if parent == curr {
			break
		}
		tail = append(tail, filepath.Base(curr))
		curr = parent
		if resolved, evalErr := filepath.EvalSymlinks(curr); evalErr == nil && resolved != "" {
			out := resolved
			for i := len(tail) - 1; i >= 0; i-- {
				out = filepath.Join(out, tail[i])
			}
			return filepath.Clean(out)
		}
	}
	return abs
}

func inferBackupScope(targetPath string) string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "project"
	}
	abs, err := filepath.Abs(targetPath)
	if err != nil {
		return "project"
	}
	rel, err := filepath.Rel(home, abs)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel) {
		return "global"
	}
	return "project"
}

func hardenBackupDirPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("检查备份目录失败: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("备份路径不是目录: %s", path)
	}
	if runtime.GOOS == "windows" {
		// Windows 无 Unix mode 语义，使用受保护 DACL（当前用户/SYSTEM/Administrators）。
		return hardenSecretDir(path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("收紧备份目录权限失败: %w", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("备份目录权限过宽 (%v)，拒绝使用", info.Mode().Perm())
	}
	return nil
}

func hardenBackupFilePermissions(path string, mustExist bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) && !mustExist {
			return nil
		}
		return fmt.Errorf("检查备份文件权限失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("备份文件不能是符号链接: %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("备份路径不是普通文件: %s", path)
	}
	if runtime.GOOS == "windows" {
		return hardenSecretFile(path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("收紧备份文件权限失败: %w", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("备份文件权限过宽 (%v)，拒绝使用", info.Mode().Perm())
	}
	return nil
}

func writeBackupRecord(backupsDir string, target backupTarget, rec configBackupRecord) error {
	encoded, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	path := backupRecordPath(backupsDir, target.Kind, target.ID, target.Path)
	if err := writeFileAtomicWithDirMode(path, encoded, 0o600, 0o700); err != nil {
		return err
	}
	return hardenBackupFilePermissions(path, true)
}

// migrateLegacyBackup 只读地判断旧备份能否安全补齐安装快照。
// 只有当前内容可被证明为“原始内容 + SlopGuard 受管内容”时才会返回升级后的记录。
func migrateLegacyBackup(target backupTarget, rec configBackupRecord) (configBackupRecord, error) {
	if rec.SchemaVersion >= backupRecordSchemaVersion && strings.TrimSpace(rec.InstalledSHA256) != "" && len(rec.InstalledBytes) > 0 {
		return rec, nil
	}
	current, err := readTargetSnapshot(target.Path)
	if err != nil {
		return rec, err
	}
	if !current.Existed {
		if !rec.Existed {
			return rec, nil
		}
		return rec, ErrLegacyBackupNeedsReview
	}
	currentHash := fileSHA256Hex(current.Data)
	if rec.OriginalSHA256 != "" && currentHash == rec.OriginalSHA256 {
		return rec, nil
	}
	// 旧版没有 InstalledBytes/InstalledSHA256。只有 MCP JSON 且当前
	// mcpServers.slopguard 条目能验证为当前可信 SlopGuard 二进制时，
	// 才允许把“原始内容 + 受管条目”升级为安装快照；其他旧格式安全停止。
	if target.Kind != "mcp" || isTOMLPath(target.Path) {
		return rec, ErrLegacyBackupNeedsReview
	}
	entry, hasEntry := legacySlopGuardMCPEntry(current.Data)
	if !hasEntry {
		return rec, ErrLegacyBackupNeedsReview
	}
	command, _ := entry["command"].(string)
	args := legacyMCPEntryArgs(entry)
	if err := validateTrustedSlopGuardCommand(command, args, ""); err != nil && !legacySlopGuardCommandShape(command, args) {
		return rec, ErrLegacyBackupNeedsReview
	}
	stripped, err := stripManagedContent(target.Kind, target.Path, current.Data)
	if err != nil {
		return rec, err
	}
	if !configMatchesOriginal(target.Kind, stripped, rec) {
		return rec, ErrLegacyBackupNeedsReview
	}
	rec.SchemaVersion = backupRecordSchemaVersion
	rec.Kind = target.Kind
	if rec.OriginalSHA256 == "" && (rec.Existed || len(rec.OriginalBytes) > 0) {
		rec.OriginalSHA256 = fileSHA256Hex(rec.OriginalBytes)
	}
	if rec.ID == "" {
		rec.ID = target.ID
	}
	if rec.TargetPath == "" {
		rec.TargetPath = filepath.Clean(target.Path)
	}
	rec.TargetRealPath = resolveTargetRealPath(target.Path)
	rec.InstalledBytes = append([]byte(nil), current.Data...)
	rec.InstalledSHA256 = currentHash
	rec.InstalledMode = uint32(current.Mode.Perm())
	rec.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	return rec, nil
}

func legacySlopGuardMCPEntry(data []byte) (map[string]any, bool) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, false
	}
	entry, ok := mcpSlopGuardEntry(root)
	if !ok {
		return nil, false
	}
	value, ok := entry.(map[string]any)
	return value, ok
}

func legacyMCPEntryArgs(entry map[string]any) []string {
	raw, ok := entry["args"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func legacySlopGuardCommandShape(command string, args []string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(command)))
	if base != "slopguard" && base != "slopguard.exe" {
		return false
	}
	if len(args) == 1 && args[0] == "mcp" {
		return true
	}
	return len(args) == 3 && args[0] == "mcp" && args[1] == "--root" && strings.TrimSpace(args[2]) != ""
}

func configMatchesOriginal(kind string, stripped []byte, rec configBackupRecord) bool {
	if !rec.Existed {
		return len(bytes.TrimSpace(stripped)) == 0
	}
	if kind == "mcp" {
		return jsonDocumentsEqual(stripped, rec.OriginalBytes)
	}
	return normalizeConfigText(stripped) == normalizeConfigText(rec.OriginalBytes)
}

func normalizeConfigText(data []byte) string {
	return strings.ReplaceAll(strings.TrimRight(string(data), "\r\n"), "\r\n", "\n")
}

func jsonDocumentsEqual(left, right []byte) bool {
	var a, b any
	if err := json.Unmarshal(left, &a); err != nil {
		return false
	}
	if err := json.Unmarshal(right, &b); err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func stripManagedContent(kind, targetPath string, data []byte) ([]byte, error) {
	switch kind {
	case "mcp":
		if isTOMLPath(targetPath) && looksLikeTOML(data) {
			return []byte(replaceOrRemoveCodexSection(string(data), "", true)), nil
		}
		return stripMCPJSONSlopGuard(data)
	case "hook":
		return []byte(stripHookMarkerBlock(string(data))), nil
	case "skill":
		block := extractSkillManagedBlock(string(data))
		if block == "" {
			return nil, ErrLegacyBackupNeedsReview
		}
		return []byte(removeSkillManagedBlock(string(data))), nil
	default:
		return nil, ErrLegacyBackupNeedsReview
	}
}

func looksLikeTOML(data []byte) bool {
	text := string(data)
	return strings.Contains(text, "[mcp_servers.") || strings.Contains(text, "mcp_servers.")
}

func stripMCPJSONSlopGuard(data []byte) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("解析 MCP 配置失败: %w", err)
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers != nil {
		delete(servers, "slopguard")
		if len(servers) == 0 {
			delete(root, "mcpServers")
		} else {
			root["mcpServers"] = servers
		}
	}
	if len(root) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// planManagedUninstall 计算三方合并卸载计划，不修改任何文件。
func planManagedUninstall(backupsDir string, target backupTarget, rec configBackupRecord) (uninstallPlan, error) {
	plan := uninstallPlan{Action: uninstallNoop}
	if err := validateBackupRecordDigests(rec); err != nil {
		return plan, err
	}
	current, err := readTargetSnapshot(target.Path)
	if err != nil {
		return plan, err
	}
	originalRec := rec
	if strings.TrimSpace(rec.InstalledSHA256) == "" || len(rec.InstalledBytes) == 0 {
		upgraded, migrateErr := migrateLegacyBackup(target, rec)
		if migrateErr != nil {
			return plan, migrateErr
		}
		rec = upgraded
		if strings.TrimSpace(rec.InstalledSHA256) != "" && len(rec.InstalledBytes) > 0 {
			plan.upgradedRecord = &rec
		}
	}
	if !current.Existed {
		if !rec.Existed {
			plan.Reason = "目标文件原本不存在"
			return plan, nil
		}
		return plan, ErrManagedTargetMissing
	}
	currentHash := fileSHA256Hex(current.Data)
	if rec.InstalledSHA256 != "" && currentHash == rec.InstalledSHA256 {
		return restoreOriginalPlan(target, rec, current.Mode)
	}
	if rec.Existed && rec.OriginalSHA256 != "" && currentHash == rec.OriginalSHA256 {
		plan.Reason = "目标已回到安装前状态"
		return plan, nil
	}
	if len(rec.InstalledBytes) == 0 {
		return plan, ErrLegacyBackupNeedsReview
	}
	switch target.Kind {
	case "mcp":
		if isTOMLPath(target.Path) {
			plan, err = planTOMLUninstall(target, rec, current)
		} else {
			plan, err = planMCPUninstall(target, rec, current)
		}
	case "hook":
		plan, err = planHookUninstall(target, rec, current)
	case "skill":
		plan, err = planSkillUninstall(target, rec, current)
	default:
		return plan, fmt.Errorf("不支持受管类型 %q", target.Kind)
	}
	if err != nil {
		return plan, err
	}
	if plan.upgradedRecord == nil && originalRec.SchemaVersion < backupRecordSchemaVersion && rec.SchemaVersion >= backupRecordSchemaVersion {
		plan.upgradedRecord = &rec
	}
	return plan, nil
}

func restoreOriginalPlan(target backupTarget, rec configBackupRecord, fallbackMode os.FileMode) (uninstallPlan, error) {
	if !rec.Existed {
		return uninstallPlan{Action: uninstallDeleteFile, Reason: "原文件不存在，删除安装时创建的文件"}, nil
	}
	mode := os.FileMode(rec.OriginalMode)
	if mode == 0 {
		mode = fallbackMode
	}
	return uninstallPlan{Action: uninstallRestoreOriginal, NewData: append([]byte(nil), rec.OriginalBytes...), NewMode: mode, Reason: "恢复安装前原文件"}, nil
}

func planTOMLUninstall(target backupTarget, rec configBackupRecord, current targetSnapshot) (uninstallPlan, error) {
	currentEntry, currentHas, err := tomlSlopGuardEntry(current.Data)
	if err != nil {
		return uninstallPlan{}, fmt.Errorf("%w: 当前 Codex 配置不是合法 TOML: %v", ErrManagedConfigModified, err)
	}
	installedEntry, installedHas, err := tomlSlopGuardEntry(rec.InstalledBytes)
	if err != nil {
		return uninstallPlan{}, ErrLegacyBackupNeedsReview
	}
	if !currentHas {
		if rec.Existed && len(rec.OriginalBytes) > 0 {
			if originalEntry, has, _ := tomlSlopGuardEntry(rec.OriginalBytes); has {
				merged := strings.TrimRight(string(current.Data), "\n")
				section, marshalErr := renderTOMLSlopGuardSection(originalEntry)
				if marshalErr != nil {
					return uninstallPlan{}, marshalErr
				}
				if merged != "" {
					merged += "\n\n"
				}
				merged += section + "\n"
				if tomlDocumentsEqual([]byte(merged), rec.OriginalBytes) {
					return restoreOriginalPlan(target, rec, current.Mode)
				}
				return uninstallPlan{Action: uninstallWriteMerged, NewData: []byte(merged), NewMode: current.Mode, Reason: "恢复安装前用户同名 Codex 段"}, nil
			}
		}
		return uninstallPlan{Action: uninstallNoop, Reason: "当前 Codex 配置已无 SlopGuard 段"}, nil
	}
	if !installedHas {
		return uninstallPlan{}, ErrLegacyBackupNeedsReview
	}
	if !tomlEntryEqual(currentEntry, installedEntry) {
		return uninstallPlan{}, fmt.Errorf("%w: Codex mcp_servers.slopguard 段已被用户修改", ErrManagedConfigModified)
	}
	stripped := replaceOrRemoveCodexSection(string(current.Data), "", true)
	if entry, stillThere, _ := tomlSlopGuardEntry([]byte(stripped)); stillThere && tomlEntryEqual(entry, currentEntry) {
		return uninstallPlan{}, fmt.Errorf("%w: Codex 配置使用内联 mcp_servers.slopguard，无法安全文本合并", ErrManagedConfigModified)
	}
	// 恢复安装前用户自己的同名 [mcp_servers.slopguard] 段。
	if rec.Existed {
		if originalEntry, has, _ := tomlSlopGuardEntry(rec.OriginalBytes); has {
			section, marshalErr := renderTOMLSlopGuardSection(originalEntry)
			if marshalErr != nil {
				return uninstallPlan{}, marshalErr
			}
			stripped = strings.TrimRight(stripped, "\n")
			if stripped != "" {
				stripped += "\n\n"
			}
			stripped += section + "\n"
		}
	}
	if rec.Existed && tomlDocumentsEqual([]byte(stripped), rec.OriginalBytes) {
		return restoreOriginalPlan(target, rec, current.Mode)
	}
	if strings.TrimSpace(stripped) == "" {
		return uninstallPlan{Action: uninstallDeleteFile, Reason: "移除 SlopGuard Codex 段后配置为空"}, nil
	}
	return uninstallPlan{Action: uninstallWriteMerged, NewData: []byte(stripped), NewMode: current.Mode, Reason: "仅移除 Codex 的 SlopGuard 段"}, nil
}

func tomlEntryEqual(left, right any) bool {
	return reflect.DeepEqual(left, right)
}

func tomlDocumentsEqual(left, right []byte) bool {
	if len(bytes.TrimSpace(left)) == 0 || len(bytes.TrimSpace(right)) == 0 {
		return len(bytes.TrimSpace(left)) == len(bytes.TrimSpace(right))
	}
	var a, b map[string]any
	if err := toml.Unmarshal(left, &a); err != nil {
		return false
	}
	if err := toml.Unmarshal(right, &b); err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func renderTOMLSlopGuardSection(entry map[string]any) (string, error) {
	doc := map[string]any{"mcp_servers": map[string]any{"slopguard": entry}}
	encoded, err := toml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("重建 Codex slopguard 段失败: %w", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(encoded), "\n"), "\n") {
		if strings.TrimSpace(line) == "[mcp_servers]" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), nil
}

func tomlSlopGuardEntry(data []byte) (map[string]any, bool, error) {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	servers, ok := doc["mcp_servers"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	entry, ok := servers["slopguard"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	return entry, true, nil
}

func planMCPUninstall(target backupTarget, rec configBackupRecord, current targetSnapshot) (uninstallPlan, error) {
	var currentRoot, installedRoot map[string]any
	if err := json.Unmarshal(current.Data, &currentRoot); err != nil {
		return uninstallPlan{}, fmt.Errorf("%w: 当前 MCP 配置不是合法 JSON", ErrManagedConfigModified)
	}
	if err := json.Unmarshal(rec.InstalledBytes, &installedRoot); err != nil {
		return uninstallPlan{}, ErrLegacyBackupNeedsReview
	}
	currentEntry, currentHas := mcpSlopGuardEntry(currentRoot)
	installedEntry, installedHas := mcpSlopGuardEntry(installedRoot)
	if !currentHas {
		// 用户可能只删除了受管条目；若安装前本来有用户自己的同名条目，
		// 仍需把它恢复回来，不能简单 noop。
		if rec.Existed && len(rec.OriginalBytes) > 0 {
			var originalRoot map[string]any
			if err := json.Unmarshal(rec.OriginalBytes, &originalRoot); err == nil {
				if originalEntry, has := mcpSlopGuardEntry(originalRoot); has {
					servers, _ := currentRoot["mcpServers"].(map[string]any)
					if servers == nil {
						servers = map[string]any{}
					}
					servers["slopguard"] = originalEntry
					currentRoot["mcpServers"] = servers
					newData, marshalErr := json.MarshalIndent(currentRoot, "", "  ")
					if marshalErr != nil {
						return uninstallPlan{}, marshalErr
					}
					newData = append(newData, '\n')
					if jsonDocumentsEqual(newData, rec.OriginalBytes) {
						return restoreOriginalPlan(target, rec, current.Mode)
					}
					return uninstallPlan{Action: uninstallWriteMerged, NewData: newData, NewMode: current.Mode, Reason: "恢复安装前用户同名 slopguard 条目"}, nil
				}
			}
		}
		return uninstallPlan{Action: uninstallNoop, Reason: "当前配置已无 SlopGuard 条目"}, nil
	}
	if !installedHas {
		return uninstallPlan{}, ErrLegacyBackupNeedsReview
	}
	if !jsonValueEqual(currentEntry, installedEntry) {
		return uninstallPlan{}, fmt.Errorf("%w: mcpServers.slopguard 条目已被用户修改", ErrManagedConfigModified)
	}
	servers, _ := currentRoot["mcpServers"].(map[string]any)
	delete(servers, "slopguard")
	// 原文件本来就有用户自己的同名 slopguard 条目时，删除受管条目后必须恢复它，
	// 而不是把用户原有配置一起删掉。
	if len(rec.OriginalBytes) > 0 {
		var originalRoot map[string]any
		if err := json.Unmarshal(rec.OriginalBytes, &originalRoot); err == nil {
			if originalEntry, has := mcpSlopGuardEntry(originalRoot); has {
				if servers == nil {
					servers = map[string]any{}
				}
				servers["slopguard"] = originalEntry
			}
		}
	}
	if len(servers) == 0 {
		delete(currentRoot, "mcpServers")
	} else {
		currentRoot["mcpServers"] = servers
	}
	if len(currentRoot) == 0 {
		return uninstallPlan{Action: uninstallDeleteFile, Reason: "移除 SlopGuard 条目后配置为空"}, nil
	}
	newData, err := json.MarshalIndent(currentRoot, "", "  ")
	if err != nil {
		return uninstallPlan{}, err
	}
	newData = append(newData, '\n')
	if rec.Existed && jsonDocumentsEqual(newData, rec.OriginalBytes) {
		return restoreOriginalPlan(target, rec, current.Mode)
	}
	return uninstallPlan{Action: uninstallWriteMerged, NewData: newData, NewMode: current.Mode, Reason: "仅移除 SlopGuard 条目，保留用户其他配置"}, nil
}

func mcpSlopGuardEntry(root map[string]any) (any, bool) {
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		return nil, false
	}
	entry, exists := servers["slopguard"]
	return entry, exists
}

func jsonValueEqual(left, right any) bool {
	return reflect.DeepEqual(left, right)
}

func planHookUninstall(target backupTarget, rec configBackupRecord, current targetSnapshot) (uninstallPlan, error) {
	currentBlocks := extractAllHookManagedBlocks(string(current.Data))
	installedBlock := extractHookManagedBlock(string(rec.InstalledBytes))
	for _, block := range currentBlocks {
		if block != installedBlock {
			return uninstallPlan{}, fmt.Errorf("%w: Git hook 中的 SlopGuard 块已被用户修改", ErrManagedConfigModified)
		}
	}
	stripped := stripHookMarkerBlock(string(current.Data))
	trimmed := strings.TrimSpace(stripped)
	if rec.Existed && normalizeConfigText([]byte(stripped)) == normalizeConfigText(rec.OriginalBytes) {
		return restoreOriginalPlan(target, rec, current.Mode)
	}
	if trimmed == "" || trimmed == "#!/bin/sh" || trimmed == "#!/usr/bin/env sh" {
		return uninstallPlan{Action: uninstallDeleteFile, Reason: "移除 SlopGuard hook 后文件为空"}, nil
	}
	return uninstallPlan{Action: uninstallWriteMerged, NewData: []byte(stripped), NewMode: current.Mode, Reason: "仅移除 SlopGuard hook 块"}, nil
}

func extractAllHookManagedBlocks(text string) []string {
	var blocks []string
	rest := text
	for {
		start := strings.Index(rest, gitHookStartMarker)
		if start < 0 {
			break
		}
		endRel := strings.Index(rest[start:], gitHookEndMarker)
		if endRel < 0 {
			break
		}
		end := start + endRel + len(gitHookEndMarker)
		blocks = append(blocks, rest[start:end])
		rest = rest[end:]
	}
	return blocks
}

func extractHookManagedBlock(text string) string {
	start := strings.Index(text, gitHookStartMarker)
	if start < 0 {
		return ""
	}
	end := strings.Index(text[start:], gitHookEndMarker)
	if end < 0 {
		return ""
	}
	return text[start : start+end+len(gitHookEndMarker)]
}

func planSkillUninstall(target backupTarget, rec configBackupRecord, current targetSnapshot) (uninstallPlan, error) {
	currentBlocks := extractAllSkillManagedBlocks(string(current.Data))
	installedBlock := extractSkillManagedBlock(string(rec.InstalledBytes))
	for _, block := range currentBlocks {
		if block != installedBlock {
			return uninstallPlan{}, fmt.Errorf("%w: Skill 受管块已被用户修改", ErrManagedConfigModified)
		}
	}
	if len(currentBlocks) == 0 {
		if fileSHA256Hex(current.Data) != rec.InstalledSHA256 {
			return uninstallPlan{}, ErrLegacyBackupNeedsReview
		}
		return restoreOriginalPlan(target, rec, current.Mode)
	}
	stripped := removeSkillManagedBlock(string(current.Data))
	if rec.Existed {
		merged := mergeOriginalWithSkillLeftover(rec.OriginalBytes, []byte(stripped))
		if normalizeConfigText(merged) == normalizeConfigText(rec.OriginalBytes) {
			return restoreOriginalPlan(target, rec, current.Mode)
		}
		return uninstallPlan{Action: uninstallWriteMerged, NewData: merged, NewMode: current.Mode, Reason: "恢复原同名 Skill 并保留受管块外用户新增内容"}, nil
	}
	if strings.TrimSpace(stripped) == "" {
		return uninstallPlan{Action: uninstallDeleteFile, Reason: "移除 SlopGuard 受管块后文件为空"}, nil
	}
	return uninstallPlan{Action: uninstallWriteMerged, NewData: []byte(stripped), NewMode: current.Mode, Reason: "仅移除 SlopGuard Skill 受管块"}, nil
}

// mergeOriginalWithSkillLeftover 把安装前的同名 Skill 原内容与受管块之外保留的
// 用户新增内容合并；没有新增时返回原字节，供三方合并做精确恢复判断。
func mergeOriginalWithSkillLeftover(original, leftover []byte) []byte {
	if strings.TrimSpace(string(leftover)) == "" {
		return append([]byte(nil), original...)
	}
	base := strings.TrimRight(string(original), "\r\n")
	addition := strings.TrimLeft(string(leftover), "\r\n")
	if base == "" {
		return []byte(addition)
	}
	return []byte(base + "\n\n" + addition)
}

func extractAllSkillManagedBlocks(text string) []string {
	var blocks []string
	rest := text
	for {
		start := strings.Index(rest, skillManagedStartMarker)
		if start < 0 {
			break
		}
		endRel := strings.Index(rest[start:], skillManagedEndMarker)
		if endRel < 0 {
			break
		}
		end := start + endRel + len(skillManagedEndMarker)
		blocks = append(blocks, rest[start:end])
		rest = rest[end:]
	}
	return blocks
}

func replaceAllSkillManagedBlocks(text, replacement string) string {
	var out strings.Builder
	rest := text
	replaced := false
	for {
		start := strings.Index(rest, skillManagedStartMarker)
		if start < 0 {
			out.WriteString(rest)
			break
		}
		endRel := strings.Index(rest[start:], skillManagedEndMarker)
		if endRel < 0 {
			out.WriteString(rest)
			break
		}
		end := start + endRel + len(skillManagedEndMarker)
		out.WriteString(rest[:start])
		if !replaced {
			out.WriteString(replacement)
			replaced = true
		}
		rest = rest[end:]
	}
	return out.String()
}

func extractSkillManagedBlock(text string) string {
	start := strings.Index(text, skillManagedStartMarker)
	if start < 0 {
		return ""
	}
	end := strings.Index(text[start:], skillManagedEndMarker)
	if end < 0 {
		return ""
	}
	return text[start : start+end+len(skillManagedEndMarker)]
}

func removeSkillManagedBlock(text string) string {
	blocks := extractAllSkillManagedBlocks(text)
	for _, block := range blocks {
		text = strings.Replace(text, block, "", 1)
	}
	return text
}

// applyManagedUninstall 执行 planManagedUninstall 的结果，并在成功后清理备份元数据。
func applyManagedUninstall(backupsDir string, target backupTarget, plan uninstallPlan) error {
	if plan.upgradedRecord != nil {
		if err := writeBackupRecord(backupsDir, target, *plan.upgradedRecord); err != nil {
			return fmt.Errorf("保存升级后的备份记录失败: %w", err)
		}
	}
	switch plan.Action {
	case uninstallNoop:
		return removeBackupRecord(backupsDir, target)
	case uninstallRestoreOriginal, uninstallWriteMerged:
		if err := writeFileAtomic(target.Path, plan.NewData, plan.NewMode); err != nil {
			return err
		}
	case uninstallDeleteFile:
		if err := os.Remove(target.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		cleanupEmptySkillLeafDir(target.Kind, target.Path)
	default:
		return fmt.Errorf("未知卸载动作 %q", plan.Action)
	}
	return removeBackupRecord(backupsDir, target)
}

// verify 在真正写入前校验卸载计划与预览记录一致。delete_file 必须对应预览中
// 的删除语义；restore_original/write_merged 必须对应 preview 记录的最终内容 hash。
// restore_original 允许合法恢复为“空文件”，此时 NewData 为空但 hash 应为空内容摘要。
func (p uninstallPlan) verify(lease *applyLease, targetPath string) error {
	if p.Action == uninstallNoop {
		return nil
	}
	if lease == nil {
		return errors.New("ui: missing preview lease")
	}
	switch p.Action {
	case uninstallDeleteFile:
		return lease.checkDeletion(targetPath)
	case uninstallRestoreOriginal, uninstallWriteMerged:
		return lease.checkNewContent(targetPath, p.NewData)
	default:
		return fmt.Errorf("未知卸载动作 %q", p.Action)
	}
}

func removeBackupRecord(backupsDir string, target backupTarget) error {
	path := backupRecordPath(backupsDir, target.Kind, target.ID, target.Path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
