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
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/engine"
	"github.com/HunterLzap/slopguard/internal/store"
	"github.com/HunterLzap/slopguard/skills"
	"github.com/pelletier/go-toml/v2"
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
	SchemaVersion   int    `json:"schema_version,omitempty"`
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Scope           string `json:"scope,omitempty"`
	TargetPath      string `json:"target_path"`
	TargetRealPath  string `json:"target_real_path,omitempty"`
	Existed         bool   `json:"existed"`
	OriginalMode    uint32 `json:"original_mode"`
	OriginalSHA256  string `json:"original_sha256,omitempty"`
	InstalledMode   uint32 `json:"installed_mode,omitempty"`
	InstalledSHA256 string `json:"installed_sha256,omitempty"`
	OriginalBytes   []byte `json:"original_bytes,omitempty"`
	InstalledBytes  []byte `json:"installed_bytes,omitempty"`
	InstalledAt     string `json:"installed_at,omitempty"`
	BackupAt        string `json:"backup_at"`
}

const backupRecordSchemaVersion = 2

// backupTarget 描述一个受管配置目标的所有权信息。
type backupTarget struct {
	Kind  string
	ID    string
	Scope string
	Path  string
}

type uninstallAction string

const (
	uninstallNoop            uninstallAction = "noop"
	uninstallRestoreOriginal uninstallAction = "restore_original"
	uninstallDeleteFile      uninstallAction = "delete_file"
	uninstallWriteMerged     uninstallAction = "write_merged"
)

// uninstallPlan 是安全卸载的纯数据预览结果；不包含任何写入副作用。
type uninstallPlan struct {
	Action  uninstallAction
	NewData []byte
	NewMode os.FileMode
	Diff    string
	Reason  string

	upgradedRecord *configBackupRecord
}

var (
	// ErrBackupMetadataMismatch 表示备份记录与目标/用途不匹配，不能信任该备份。
	ErrBackupMetadataMismatch = errors.New("备份元数据与目标不匹配")
	// ErrLegacyBackupNeedsReview 表示旧备份缺少可证明的安装快照，必须人工确认或重新安装。
	ErrLegacyBackupNeedsReview = errors.New("旧版本备份缺少安装快照，已阻止覆盖或删除")
	// ErrManagedConfigModified 表示用户修改了 SlopGuard 受管配置，必须保留现场并停止自动化操作。
	ErrManagedConfigModified = errors.New("SlopGuard 受管配置已被用户修改")
	// ErrManagedTargetMissing 表示目标文件在卸载时消失，不能盲目重建原始内容。
	ErrManagedTargetMissing = errors.New("受管目标文件已不存在")
)

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

func fileSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func currentFileSHA256(path string) (hash string, existed bool, err error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return "", false, nil
		}
		return "", false, readErr
	}
	return fileSHA256Hex(data), true, nil
}

// validateBackupRecordDigests 校验备份记录内嵌内容与摘要一致，防止损坏或
// 被篡改的备份被用于恢复/卸载。v2 记录必须同时具备内容与摘要。
func validateBackupRecordDigests(rec configBackupRecord) error {
	if !rec.Existed && len(rec.OriginalBytes) > 0 {
		return fmt.Errorf("%w: 备份标记原文件不存在但包含原始内容", ErrBackupMetadataMismatch)
	}
	if strings.TrimSpace(rec.OriginalSHA256) != "" {
		if got := fileSHA256Hex(rec.OriginalBytes); got != rec.OriginalSHA256 {
			return fmt.Errorf("%w: 原始内容摘要与备份内容不一致", ErrBackupMetadataMismatch)
		}
	} else if len(rec.OriginalBytes) > 0 && rec.SchemaVersion >= backupRecordSchemaVersion {
		return fmt.Errorf("%w: 备份缺少原始内容摘要", ErrBackupMetadataMismatch)
	}
	if strings.TrimSpace(rec.InstalledSHA256) != "" {
		if len(rec.InstalledBytes) == 0 {
			// 合法空文件安装快照：摘要应为空内容摘要；其余“有摘要无内容”视为损坏。
			if rec.SchemaVersion >= backupRecordSchemaVersion && rec.InstalledSHA256 != fileSHA256Hex(nil) {
				return fmt.Errorf("%w: 安装快照缺少内容", ErrBackupMetadataMismatch)
			}
		} else if got := fileSHA256Hex(rec.InstalledBytes); got != rec.InstalledSHA256 {
			return fmt.Errorf("%w: 安装快照摘要与备份内容不一致", ErrBackupMetadataMismatch)
		}
	} else if len(rec.InstalledBytes) > 0 && rec.SchemaVersion >= backupRecordSchemaVersion {
		return fmt.Errorf("%w: 安装快照缺少摘要", ErrBackupMetadataMismatch)
	}
	return nil
}

func existingFileModeOrDefault(path string, fallback os.FileMode) os.FileMode {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		if perm := info.Mode().Perm(); perm != 0 {
			return perm
		}
	}
	return fallback
}

func defaultModeForTarget(scope, kind string) os.FileMode {
	if scope == "global" && kind == "mcp" {
		return 0o600
	}
	return 0o644
}

func lookupBackupChecked(backupsDir, kind, id, targetPath string) (string, configBackupRecord, bool, error) {
	if strings.TrimSpace(backupsDir) == "" || strings.TrimSpace(targetPath) == "" {
		return "", configBackupRecord{}, false, nil
	}
	path := backupRecordPath(backupsDir, kind, id, targetPath)
	if err := hardenBackupFilePermissions(path, false); err != nil {
		return path, configBackupRecord{}, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", configBackupRecord{}, false, nil
		}
		return path, configBackupRecord{}, false, fmt.Errorf("读取备份记录失败 (%s): %w", path, err)
	}
	var rec configBackupRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return path, configBackupRecord{}, false, fmt.Errorf("备份记录已损坏 (%s)，已阻止盲目还原或删除: %w", path, err)
	}
	if err := validateBackupRecordDigests(rec); err != nil {
		return path, configBackupRecord{}, false, fmt.Errorf("备份记录完整性校验失败 (%s): %w", path, err)
	}
	if rec.Kind != "" && !strings.EqualFold(rec.Kind, kind) {
		return path, configBackupRecord{}, false, fmt.Errorf("%w: 备份 kind=%q，请求 kind=%q", ErrBackupMetadataMismatch, rec.Kind, kind)
	}
	if rec.ID != "" && strings.TrimSpace(rec.ID) != strings.TrimSpace(id) {
		return path, configBackupRecord{}, false, fmt.Errorf("%w: 备份 id=%q，请求 id=%q", ErrBackupMetadataMismatch, rec.ID, id)
	}
	if rec.TargetPath != "" && filepath.Clean(rec.TargetPath) != filepath.Clean(targetPath) {
		return path, configBackupRecord{}, false, fmt.Errorf("%w: 备份目标路径校验不一致 (%s != %s)", ErrBackupMetadataMismatch, rec.TargetPath, targetPath)
	}
	if rec.TargetRealPath != "" {
		if currentReal := resolveTargetRealPath(targetPath); currentReal != "" && filepath.Clean(currentReal) != filepath.Clean(rec.TargetRealPath) {
			return path, configBackupRecord{}, false, fmt.Errorf("%w: 目标真实路径已变化 (%s != %s)", ErrBackupMetadataMismatch, rec.TargetRealPath, currentReal)
		}
	}
	if rec.SchemaVersion > backupRecordSchemaVersion {
		return path, configBackupRecord{}, false, fmt.Errorf("%w: 不支持的备份版本 %d", ErrBackupMetadataMismatch, rec.SchemaVersion)
	}
	return path, rec, true, nil
}

func lookupBackup(backupsDir, kind, id, targetPath string) (string, configBackupRecord, bool) {
	p, rec, ok, err := lookupBackupChecked(backupsDir, kind, id, targetPath)
	if err != nil || !ok {
		return "", configBackupRecord{}, false
	}
	return p, rec, true
}

// ensureBackup creates a backup of targetPath before SlopGuard modifies it.
// 首次备份保留安装前原始状态；同一目标重复接入时，如果目标在上次安装后被
// 外部修改，则刷新“去掉受管内容后的基线”，避免后续卸载回滚用户新增配置。
func ensureBackup(backupsDir, kind, id, targetPath string) (string, configBackupRecord, error) {
	if strings.TrimSpace(backupsDir) == "" {
		return "", configBackupRecord{}, errors.New("无法定位备份目录")
	}
	if err := os.MkdirAll(backupsDir, 0o700); err != nil {
		return "", configBackupRecord{}, fmt.Errorf("创建备份目录失败: %w", err)
	}
	if err := hardenBackupDirPermissions(backupsDir); err != nil {
		return "", configBackupRecord{}, err
	}

	if existingPath, existingRec, ok, checkErr := lookupBackupChecked(backupsDir, kind, id, targetPath); checkErr != nil {
		return "", configBackupRecord{}, checkErr
	} else if ok {
		_ = os.Chmod(existingPath, 0o600)
		refreshed, changed, refreshErr := refreshBackupBaselineForReinstall(backupTarget{Kind: kind, ID: id, Scope: inferBackupScope(targetPath), Path: targetPath}, existingRec)
		if refreshErr != nil {
			return "", configBackupRecord{}, refreshErr
		}
		if changed {
			if err := writeBackupRecord(backupsDir, backupTarget{Kind: kind, ID: id, Scope: refreshed.Scope, Path: targetPath}, refreshed); err != nil {
				return "", configBackupRecord{}, fmt.Errorf("更新重复接入基线失败: %w", err)
			}
			return existingPath, refreshed, nil
		}
		return existingPath, existingRec, nil
	}
	rec := configBackupRecord{
		SchemaVersion:  backupRecordSchemaVersion,
		ID:             id,
		Kind:           kind,
		Scope:          inferBackupScope(targetPath),
		TargetPath:     filepath.Clean(targetPath),
		TargetRealPath: resolveTargetRealPath(targetPath),
		BackupAt:       time.Now().UTC().Format(time.RFC3339),
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
		rec.OriginalSHA256 = fileSHA256Hex(data)
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
	if err := writeFileAtomicWithDirMode(backupPath, encoded, 0o600, 0o700); err != nil {
		return "", configBackupRecord{}, fmt.Errorf("保存备份文件失败: %w", err)
	}
	if err := hardenBackupFilePermissions(backupPath, true); err != nil {
		return "", configBackupRecord{}, err
	}
	return backupPath, rec, nil
}

func recordBackupInstalledHash(backupsDir, kind, id, targetPath string, installedBytes []byte) error {
	return recordBackupInstalled(backupsDir, backupTarget{Kind: kind, ID: id, Path: targetPath}, installedBytes, 0)
}

// refreshBackupBaselineForReinstall 处理同一目标重复接入：若用户在上次安装后
// 修改过目标文件，则把“当前内容去掉受管内容”后的结果作为新的安装前基线，
// 避免以后卸载时把两次安装之间用户新增的配置回滚掉。
func refreshBackupBaselineForReinstall(target backupTarget, rec configBackupRecord) (configBackupRecord, bool, error) {
	if strings.TrimSpace(rec.InstalledSHA256) == "" || len(rec.InstalledBytes) == 0 {
		upgraded, migrateErr := migrateLegacyBackup(target, rec)
		if migrateErr != nil {
			return rec, false, migrateErr
		}
		rec = upgraded
	}
	if strings.TrimSpace(rec.InstalledSHA256) == "" || len(rec.InstalledBytes) == 0 {
		// 迁移后仍无安装快照：只有能证明当前内容就是原始基线时才允许重复接入。
		current, err := readTargetSnapshot(target.Path)
		if err != nil {
			return rec, false, err
		}
		if !current.Existed {
			if !rec.Existed {
				return rec, false, nil
			}
			return rec, false, ErrLegacyBackupNeedsReview
		}
		if rec.OriginalSHA256 != "" && fileSHA256Hex(current.Data) == rec.OriginalSHA256 {
			return rec, false, nil
		}
		return rec, false, ErrLegacyBackupNeedsReview
	}
	current, err := readTargetSnapshot(target.Path)
	if err != nil {
		return rec, false, err
	}
	if !current.Existed {
		return rec, false, nil
	}
	currentHash := fileSHA256Hex(current.Data)
	if currentHash == rec.InstalledSHA256 {
		return rec, false, nil
	}
	if rec.OriginalSHA256 != "" && currentHash == rec.OriginalSHA256 {
		return rec, false, nil
	}
	baseline, err := stripManagedForReinstallBaseline(target, rec, current.Data)
	if err != nil {
		return rec, false, err
	}
	rec.Existed = rec.Existed || len(baseline) > 0
	rec.OriginalMode = uint32(current.Mode.Perm())
	rec.OriginalBytes = append([]byte(nil), baseline...)
	rec.OriginalSHA256 = fileSHA256Hex(baseline)
	return rec, true, nil
}

func stripManagedForReinstallBaseline(target backupTarget, rec configBackupRecord, current []byte) ([]byte, error) {
	switch target.Kind {
	case "mcp":
		if isTOMLPath(target.Path) {
			return stripTOMLForReinstallBaseline(rec, current)
		}
		return stripMCPJSONForReinstallBaseline(rec, current)
	case "hook":
		return []byte(stripHookMarkerBlock(string(current))), nil
	case "skill":
		blocks := extractAllSkillManagedBlocks(string(current))
		if len(blocks) == 0 {
			return append([]byte(nil), current...), nil
		}
		installedBlock := extractSkillManagedBlock(string(rec.InstalledBytes))
		for _, block := range blocks {
			if block != installedBlock {
				return nil, fmt.Errorf("%w: Skill 受管块已被用户修改", ErrManagedConfigModified)
			}
		}
		leftover := removeSkillManagedBlock(string(current))
		if rec.Existed {
			return mergeOriginalWithSkillLeftover(rec.OriginalBytes, []byte(leftover)), nil
		}
		if strings.TrimSpace(leftover) == "" {
			return nil, nil
		}
		return []byte(leftover), nil
	default:
		return nil, fmt.Errorf("不支持的受管类型 %q", target.Kind)
	}
}

func mcpSlopGuardEntryBytes(data []byte) (any, bool) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, false
	}
	return mcpSlopGuardEntry(root)
}

func stripMCPJSONForReinstallBaseline(rec configBackupRecord, current []byte) ([]byte, error) {
	if len(bytes.TrimSpace(current)) == 0 {
		return nil, nil
	}
	var currentRoot map[string]any
	if err := json.Unmarshal(current, &currentRoot); err != nil {
		return nil, fmt.Errorf("解析 MCP 配置失败: %w", err)
	}
	if currentEntry, currentHas := mcpSlopGuardEntry(currentRoot); currentHas {
		installedEntry, installedHas := mcpSlopGuardEntryBytes(rec.InstalledBytes)
		if !installedHas || !jsonValueEqual(currentEntry, installedEntry) {
			return nil, fmt.Errorf("%w: mcpServers.slopguard 条目已被用户修改", ErrManagedConfigModified)
		}
		servers, ok := currentRoot["mcpServers"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("配置文件 mcpServers 字段不是 JSON 对象（已拒绝修改）")
		}
		delete(servers, "slopguard")
		if len(servers) == 0 {
			delete(currentRoot, "mcpServers")
		} else {
			currentRoot["mcpServers"] = servers
		}
	}
	if len(rec.OriginalBytes) > 0 {
		var originalRoot map[string]any
		if err := json.Unmarshal(rec.OriginalBytes, &originalRoot); err == nil {
			if originalEntry, has := mcpSlopGuardEntry(originalRoot); has {
				servers, _ := currentRoot["mcpServers"].(map[string]any)
				if servers == nil {
					servers = map[string]any{}
				}
				servers["slopguard"] = originalEntry
				currentRoot["mcpServers"] = servers
			}
		}
	}
	if len(currentRoot) == 0 {
		return nil, nil
	}
	encoded, err := json.MarshalIndent(currentRoot, "", "  ")
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	return encoded, nil
}

func stripTOMLForReinstallBaseline(rec configBackupRecord, current []byte) ([]byte, error) {
	var doc map[string]any
	if err := toml.Unmarshal(current, &doc); err != nil {
		return nil, fmt.Errorf("当前 Codex 配置不是合法 TOML: %w", err)
	}
	currentEntry, currentHas, err := tomlSlopGuardEntry(current)
	if err != nil {
		return nil, err
	}
	if currentHas {
		installedEntry, installedHas, instErr := tomlSlopGuardEntry(rec.InstalledBytes)
		if instErr != nil {
			return nil, ErrLegacyBackupNeedsReview
		}
		if !installedHas || !tomlEntryEqual(currentEntry, installedEntry) {
			return nil, fmt.Errorf("%w: Codex mcp_servers.slopguard 段已被用户修改", ErrManagedConfigModified)
		}
	}
	stripped := replaceOrRemoveCodexSection(string(current), "", true)
	var strippedDoc map[string]any
	if err := toml.Unmarshal([]byte(stripped), &strippedDoc); err != nil {
		return nil, fmt.Errorf("移除 Codex slopguard 段后的 TOML 非法: %w", err)
	}
	if _, stillThere := strippedDoc["mcp_servers"]; stillThere {
		if entry, has, _ := tomlSlopGuardEntry([]byte(stripped)); has && currentHas && tomlEntryEqual(entry, currentEntry) {
			return nil, fmt.Errorf("%w: Codex 配置使用内联 mcp_servers.slopguard，无法安全文本合并", ErrManagedConfigModified)
		}
	}
	if rec.Existed {
		if origEntry, has, _ := tomlSlopGuardEntry(rec.OriginalBytes); has {
			section, marshalErr := renderTOMLSlopGuardSection(origEntry)
			if marshalErr != nil {
				return nil, marshalErr
			}
			stripped = strings.TrimRight(stripped, "\n")
			if stripped != "" {
				stripped += "\n\n"
			}
			stripped += section
		}
	}
	return []byte(stripped), nil
}

// recordBackupInstalled 记录安装后的完整快照。它同时把旧版备份升级为 v2：
// 旧记录只保留 OriginalBytes 时，下一次成功安装后即可建立可信的 InstalledBytes/Hash。
func recordBackupInstalled(backupsDir string, target backupTarget, installedBytes []byte, mode os.FileMode) error {
	actual, readErr := os.ReadFile(target.Path)
	if readErr != nil {
		return fmt.Errorf("读取安装后的目标文件失败: %w", readErr)
	}
	if !bytes.Equal(actual, installedBytes) {
		return fmt.Errorf("目标文件内容与待记录的安装快照不一致，已拒绝写入备份摘要")
	}
	backupPath, rec, ok, err := lookupBackupChecked(backupsDir, target.Kind, target.ID, target.Path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("找不到待记录的备份元数据: %s", target.Path)
	}
	rec.SchemaVersion = backupRecordSchemaVersion
	rec.Kind = target.Kind
	rec.ID = target.ID
	if strings.TrimSpace(rec.OriginalSHA256) == "" && (rec.Existed || len(rec.OriginalBytes) > 0) {
		rec.OriginalSHA256 = fileSHA256Hex(rec.OriginalBytes)
	}
	if strings.TrimSpace(target.Scope) != "" {
		rec.Scope = target.Scope
	}
	rec.TargetPath = filepath.Clean(target.Path)
	rec.TargetRealPath = resolveTargetRealPath(target.Path)
	rec.InstalledSHA256 = fileSHA256Hex(installedBytes)
	rec.InstalledBytes = append([]byte(nil), installedBytes...)
	if mode != 0 {
		rec.InstalledMode = uint32(mode.Perm())
	}
	rec.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	encoded, mErr := json.MarshalIndent(rec, "", "  ")
	if mErr != nil {
		return mErr
	}
	encoded = append(encoded, '\n')
	if err := writeFileAtomicWithDirMode(backupPath, encoded, 0o600, 0o700); err != nil {
		return err
	}
	return hardenBackupFilePermissions(backupPath, true)
}

func targetExternallyModifiedSinceInstall(rec configBackupRecord, targetPath string) (modified bool, currentExisted bool, err error) {
	currHash, existed, err := currentFileSHA256(targetPath)
	if err != nil {
		return false, false, err
	}
	if strings.TrimSpace(rec.InstalledSHA256) == "" {
		// 旧版备份没有安装后快照：绝不能再默认视为“未修改”。
		upgraded, migrateErr := migrateLegacyBackup(backupTarget{Kind: rec.Kind, ID: rec.ID, Path: targetPath}, rec)
		if migrateErr != nil {
			return true, existed, nil
		}
		rec = upgraded
		if strings.TrimSpace(rec.InstalledSHA256) == "" {
			if rec.OriginalSHA256 != "" && currHash == rec.OriginalSHA256 && existed {
				return false, true, nil
			}
			return true, existed, nil
		}
	}
	if !existed {
		return true, false, nil
	}
	return currHash != rec.InstalledSHA256, true, nil
}

func restoreFromBackup(backupsDir, kind, id, targetPath string) (restoredFromBackup bool, removedFile bool, err error) {
	backupPath, rec, ok, checkErr := lookupBackupChecked(backupsDir, kind, id, targetPath)
	if checkErr != nil {
		return false, false, checkErr
	}
	if !ok {
		return false, false, nil
	}
	target := backupTarget{Kind: kind, ID: id, Scope: inferBackupScope(targetPath), Path: targetPath}
	plan, err := planManagedUninstall(backupsDir, target, rec)
	if err != nil {
		return false, false, err
	}
	if plan.Action == uninstallNoop {
		_ = os.Remove(backupPath)
		return false, false, nil
	}
	if err := applyManagedUninstall(backupsDir, target, plan); err != nil {
		return false, false, err
	}
	return true, plan.Action == uninstallDeleteFile, nil
}

// uninstallManagedTarget safely removes SlopGuard's own entries when disabling.
// If the target file has not been modified externally since install, it restores the exact
// original snapshot (or removes the newly created file). If external additions exist, it
// performs a targeted removal of SlopGuard entries only so user edits are never lost.
func uninstallManagedTarget(backupsDir, kind, id, targetPath, scope string) (restoredFromBackup bool, removedFile bool, err error) {
	target := backupTarget{Kind: kind, ID: id, Scope: scope, Path: targetPath}
	_, rec, hasBackup, checkErr := lookupBackupChecked(backupsDir, kind, id, targetPath)
	if checkErr != nil {
		return false, false, checkErr
	}
	if !hasBackup {
		// 没有备份就无法证明目标中的 SlopGuard 内容属于我们，安全停止而不删除用户文件。
		return false, false, fmt.Errorf("%w: %s", ErrLegacyBackupNeedsReview, targetPath)
	}
	plan, planErr := planManagedUninstall(backupsDir, target, rec)
	if planErr != nil {
		return false, false, planErr
	}
	if plan.Action == uninstallNoop {
		_ = removeBackupRecord(backupsDir, target)
		return false, false, nil
	}
	if applyErr := applyManagedUninstall(backupsDir, target, plan); applyErr != nil {
		return false, false, applyErr
	}
	return plan.Action == uninstallRestoreOriginal, plan.Action == uninstallDeleteFile, nil
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
	return writeFileAtomicWithDirMode(path, data, mode, 0o755)
}

func writeFileAtomicWithDirMode(path string, data []byte, mode, dirMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
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

func parseTOMLHeaderName(line string) (string, bool) {
	clean := stripTOMLInlineComment(line)
	if !strings.HasPrefix(clean, "[") || !strings.HasSuffix(clean, "]") {
		return "", false
	}
	if strings.HasPrefix(clean, "[[") && strings.HasSuffix(clean, "]]") {
		return strings.TrimSpace(clean[2 : len(clean)-2]), true
	}
	return strings.TrimSpace(clean[1 : len(clean)-1]), true
}

func stripTOMLInlineComment(line string) string {
	inBasic, inLiteral := false, false
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inBasic {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inBasic = false
			}
			continue
		}
		if inLiteral {
			if ch == '\'' {
				inLiteral = false
			}
			continue
		}
		switch ch {
		case '"':
			inBasic = true
		case '\'':
			inLiteral = true
		case '#':
			return strings.TrimSpace(line[:i])
		}
	}
	return strings.TrimSpace(line)
}

func isSlopGuardTOMLSection(sec string) bool {
	return sec == "mcp_servers.slopguard" || strings.HasPrefix(sec, "mcp_servers.slopguard.")
}

// validateTOMLBasic performs structural pure-Go TOML validation (multiline string closure,
// bracket matching, and header syntax) before writing ~/.codex/config.toml.
func validateTOMLBasic(text string) error {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	var multiDelim string
	arrayDepth := 0
	for lineNo, raw := range lines {
		line := raw
		if multiDelim != "" {
			if strings.Contains(line, multiDelim) {
				multiDelim = ""
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(line, `"""`) && strings.Count(line, `"""`)%2 == 1 {
			multiDelim = `"""`
			continue
		}
		if strings.Contains(line, `'''`) && strings.Count(line, `'''`)%2 == 1 {
			multiDelim = `'''`
			continue
		}
		clean := stripTOMLInlineComment(line)
		if clean == "" {
			continue
		}
		if arrayDepth == 0 && strings.HasPrefix(clean, "[") {
			if !strings.HasSuffix(clean, "]") {
				return fmt.Errorf("TOML 第 %d 行表头括号未闭合: %s", lineNo+1, clean)
			}
			inner := strings.TrimSpace(clean[1 : len(clean)-1])
			if strings.HasPrefix(clean, "[[") && strings.HasSuffix(clean, "]]") {
				inner = strings.TrimSpace(clean[2 : len(clean)-2])
			}
			if inner == "" {
				return fmt.Errorf("TOML 第 %d 行存在空表头", lineNo+1)
			}
			continue
		}
		if arrayDepth == 0 && !strings.Contains(clean, "=") {
			return fmt.Errorf("TOML 第 %d 行缺少 '=' 赋值: %s", lineNo+1, clean)
		}
		inBasic, inLiteral, escaped := false, false, false
		for i := 0; i < len(clean); i++ {
			ch := clean[i]
			if escaped {
				escaped = false
				continue
			}
			if inBasic {
				if ch == '\\' {
					escaped = true
				} else if ch == '"' {
					inBasic = false
				}
				continue
			}
			if inLiteral {
				if ch == '\'' {
					inLiteral = false
				}
				continue
			}
			switch ch {
			case '"':
				inBasic = true
			case '\'':
				inLiteral = true
			case '[', '{':
				arrayDepth++
			case ']', '}':
				arrayDepth--
				if arrayDepth < 0 {
					return fmt.Errorf("TOML 第 %d 行存在多余闭合括号", lineNo+1)
				}
			}
		}
		if inBasic || inLiteral {
			return fmt.Errorf("TOML 第 %d 行字符串引号未闭合", lineNo+1)
		}
	}
	if multiDelim != "" {
		return errors.New("TOML 多行字符串未闭合")
	}
	if arrayDepth != 0 {
		return errors.New("TOML 多行数组或内联表括号未闭合")
	}
	return nil
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
	multiDelim := ""

	for _, line := range lines {
		if multiDelim != "" {
			if strings.Contains(line, multiDelim) {
				multiDelim = ""
			}
			if !inSection {
				out = append(out, line)
			}
			continue
		}
		if strings.Contains(line, `"""`) && strings.Count(line, `"""`)%2 == 1 {
			multiDelim = `"""`
		} else if strings.Contains(line, `'''`) && strings.Count(line, `'''`)%2 == 1 {
			multiDelim = `'''`
		}

		if multiDelim == "" && isInlineSlopGuardAssignment(line) {
			if !remove && !replaced {
				out = append(out, strings.TrimRight(replacement, "\n"))
				replaced = true
			}
			continue
		}

		if sec, isHeader := parseTOMLHeaderName(line); isHeader {
			if isSlopGuardTOMLSection(sec) {
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

func isInlineSlopGuardAssignment(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "[") {
		return false
	}
	eq := strings.IndexByte(trimmed, '=')
	if eq <= 0 {
		return false
	}
	key := strings.ReplaceAll(strings.TrimSpace(trimmed[:eq]), " ", "")
	return key == "mcp_servers.slopguard"
}

func renderMCPUpsert(path, binaryPath string, args []string) (oldText, newText string, existed bool, err error) {
	if isTOMLPath(path) {
		if data, readErr := os.ReadFile(path); readErr == nil {
			existed = true
			oldText = string(data)
			if err := validateTOMLBasic(oldText); err != nil {
				return "", "", true, fmt.Errorf("现有 Codex 配置 %s 语法校验失败: %w", path, err)
			}
		} else if !os.IsNotExist(readErr) {
			return "", "", false, readErr
		}
		section := formatCodexSlopGuardSection(binaryPath, args)
		newText = replaceOrRemoveCodexSection(oldText, section, false)
		if err := validateTOMLBasic(newText); err != nil {
			return "", "", existed, fmt.Errorf("生成 Codex 配置校验失败: %w", err)
		}
		var parsed map[string]any
		if err := toml.Unmarshal([]byte(newText), &parsed); err != nil {
			return "", "", existed, fmt.Errorf("生成 Codex 配置不是合法 TOML（可能已存在同名内联表）: %w", err)
		}
		return oldText, newText, existed, nil
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

	rawServers, hasKey := root["mcpServers"]
	var servers map[string]any
	if !hasKey || rawServers == nil {
		servers = map[string]any{}
	} else if m, ok := rawServers.(map[string]any); ok {
		servers = m
	} else {
		return "", "", existed, fmt.Errorf("配置文件 %s 的 mcpServers 字段不是 JSON 对象（已拒绝覆盖）", path)
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
		if err := validateTOMLBasic(oldText); err != nil {
			return "", "", false, fmt.Errorf("现有 Codex 配置 %s 语法校验失败: %w", path, err)
		}
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
	rawServers, hasKey := root["mcpServers"]
	if !hasKey || rawServers == nil {
		return oldText, oldText, false, nil
	}
	servers, ok := rawServers.(map[string]any)
	if !ok {
		return "", "", false, fmt.Errorf("配置文件 %s 的 mcpServers 字段不是 JSON 对象（已拒绝修改）", path)
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
			return wrapSkillManagedBlock(doc + "\n")
		}
		return wrapSkillManagedBlock("---\ndescription: SlopGuard deterministic code-quality guardrail MCP workflow\nalwaysApply: false\n---\n\n" + doc + "\n")
	}
	if strings.HasPrefix(doc, "---\n") {
		return wrapSkillManagedBlock(doc + "\n")
	}
	return wrapSkillManagedBlock("---\nname: slopguard\ndescription: SlopGuard deterministic code-quality guardrail MCP workflow and mistake inbox analysis guide.\n---\n\n" + doc + "\n")
}

// mergeSkillInstallContent 计算 Skill 安装的最终内容：
//   - 文件不存在或为空：直接写入完整渲染内容；
//   - 已有 SlopGuard 受管块：只替换受管块，保留文件其余用户内容与 frontmatter；
//   - 已存在非空且没有受管块的同名文件：无法证明归属，安全停止，绝不覆盖。
func mergeSkillInstallContent(existing, rendered string) (string, error) {
	if strings.TrimSpace(existing) == "" {
		return rendered, nil
	}
	blocks := extractAllSkillManagedBlocks(existing)
	if len(blocks) == 0 {
		return "", fmt.Errorf("%w: 目标已存在非 SlopGuard 管理的同名文件，已安全停止", ErrManagedConfigModified)
	}
	replacement := extractSkillManagedBlock(rendered)
	if replacement == "" {
		return "", errors.New("渲染的 Skill 内容缺少受管块")
	}
	return replaceAllSkillManagedBlocks(existing, replacement), nil
}

// wrapSkillManagedBlock 把 SlopGuard 生成内容放进受管块；YAML frontmatter 必须留在文件最开头。
func wrapSkillManagedBlock(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if strings.HasPrefix(content, "---\n") {
		if end := strings.Index(content[4:], "\n---\n"); end >= 0 {
			frontEnd := 4 + end + len("\n---\n")
			front := content[:frontEnd]
			body := strings.TrimRight(content[frontEnd:], "\n")
			return front + "\n" + skillManagedStartMarker + "\n" + body + "\n" + skillManagedEndMarker + "\n"
		}
	}
	return skillManagedStartMarker + "\n" + strings.TrimRight(content, "\n") + "\n" + skillManagedEndMarker + "\n"
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
	res := engine.RunProcess(context.Background(), engine.ProcessSpec{
		Command:        agyBin,
		Args:           []string{"mcp", "list"},
		Env:            augmentedCLIEnv(home),
		Timeout:        4 * time.Second,
		MaxOutputBytes: 128 * 1024,
	})
	if res.Err != nil || res.ExitCode != 0 {
		return false
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "slopguard" && strings.EqualFold(fields[2], "enabled") {
			return true
		}
	}
	return false
}

func syncAgyMCPRegistry(home, exe string, enable bool) error {
	agyBin := lookupAgentCLIBinary(home, "agy")
	if agyBin == "" {
		return nil
	}
	// Remove first so re-adding updates path cleanly when SlopGuard.app moves.
	rmRes := engine.RunProcess(context.Background(), engine.ProcessSpec{
		Command:        agyBin,
		Args:           []string{"mcp", "remove", "slopguard"},
		Env:            augmentedCLIEnv(home),
		Timeout:        5 * time.Second,
		MaxOutputBytes: 64 * 1024,
	})
	if !enable || strings.TrimSpace(exe) == "" {
		if rmRes.Err != nil {
			return fmt.Errorf("agy mcp remove failed: %w", rmRes.Err)
		}
		return nil
	}
	addRes := engine.RunProcess(context.Background(), engine.ProcessSpec{
		Command:        agyBin,
		Args:           []string{"mcp", "add", "slopguard", exe, "mcp"},
		Env:            augmentedCLIEnv(home),
		Timeout:        5 * time.Second,
		MaxOutputBytes: 64 * 1024,
	})
	if addRes.Err != nil || addRes.ExitCode != 0 {
		msg := strings.TrimSpace(addRes.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(addRes.Stdout)
		}
		if msg == "" && addRes.Err != nil {
			msg = addRes.Err.Error()
		}
		return fmt.Errorf("agy mcp add 失败: %s", msg)
	}
	return nil
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
					cmd, args, _ := readConfiguredMCPCommand(m.ConfigPath)
					expectedProj := ""
					if scope == "project" {
						expectedProj = project
					}
					prof.BinaryOK = validateTrustedSlopGuardCommand(cmd, args, expectedProj) == nil
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
	AgentID           string   `json:"agent_id"`
	Name              string   `json:"name"`
	Scope             string   `json:"scope"`
	Installed         bool     `json:"installed"`
	Configured        bool     `json:"configured"`
	ProjectConfigured bool     `json:"project_configured"`
	GlobalConfigured  bool     `json:"global_configured"`
	ConfigMatched     bool     `json:"config_matched"`
	BinaryPath        string   `json:"binary_path,omitempty"`
	BinaryOK          bool     `json:"binary_ok"`
	SkillOK           bool     `json:"skill_ok"`
	HandshakeOK       bool     `json:"handshake_ok"`
	LatencyMS         int64    `json:"latency_ms,omitempty"`
	Tools             []string `json:"tools,omitempty"`
	CLIRegistered     *bool    `json:"cli_registered,omitempty"`
	EventCount        int      `json:"event_count"`
	LastEventAt       string   `json:"last_event_at,omitempty"`
	HealthStatus      string   `json:"health_status"`
	ActivationStatus  string   `json:"activation_status"`
	Diagnosis         string   `json:"diagnosis"`
}

// validateTrustedSlopGuardCommand ensures self-check never launches an arbitrary
// command read from project/global JSON or TOML files.
func validateTrustedSlopGuardCommand(cmd string, args []string, expectedProject string) error {
	cmd = strings.TrimSpace(cmd)
	if !isExecutableFile(cmd) {
		return fmt.Errorf("配置里的命令不可用 (%s)", cmd)
	}
	selfExe, err := resolveSelfBinary()
	if err != nil {
		return err
	}
	selfInfo, selfErr := os.Stat(selfExe)
	cmdReal := cmd
	if resolved, rErr := filepath.EvalSymlinks(cmd); rErr == nil && resolved != "" {
		cmdReal = resolved
	}
	cmdInfo, cmdErr := os.Stat(cmdReal)
	sameBinary := (selfErr == nil && cmdErr == nil && os.SameFile(selfInfo, cmdInfo)) || filepath.Clean(cmdReal) == filepath.Clean(selfExe)
	if !sameBinary {
		return fmt.Errorf("MCP 命令与当前可信 SlopGuard 二进制不一致 (%s != %s)，已拒绝执行（请重新开关修复路径）", cmdReal, selfExe)
	}

	if len(args) == 1 && args[0] == "mcp" {
		return nil
	}
	if len(args) == 3 && args[0] == "mcp" && args[1] == "--root" && strings.TrimSpace(args[2]) != "" {
		if expectedProject != "" {
			wantRoot, _ := filepath.Abs(strings.TrimSpace(expectedProject))
			gotRoot, _ := filepath.Abs(strings.TrimSpace(args[2]))
			if wantReal, err := filepath.EvalSymlinks(wantRoot); err == nil && wantReal != "" {
				wantRoot = wantReal
			}
			if gotReal, err := filepath.EvalSymlinks(gotRoot); err == nil && gotReal != "" {
				gotRoot = gotReal
			}
			if wantRoot != gotRoot {
				return fmt.Errorf("项目 MCP --root (%s) 与当前项目 (%s) 不匹配", gotRoot, wantRoot)
			}
		}
		return nil
	}
	return fmt.Errorf("MCP 参数不合法 %v，仅允许 [\"mcp\"] 或 [\"mcp\", \"--root\", \"<project>\"]", args)
}

func runStdioMCPHandshake(ctx context.Context, binaryPath string, args []string) (tools []string, latencyMS int64, err error) {
	if err := validateTrustedSlopGuardCommand(binaryPath, args, ""); err != nil {
		return nil, 0, err
	}
	started := time.Now()
	var parsedTools []string
	var handshakeErr error

	res := engine.RunStdioSession(ctx, engine.ProcessSpec{
		Command:        binaryPath,
		Args:           args,
		Env:            augmentedCLIEnv(""),
		Timeout:        5 * time.Second,
		MaxOutputBytes: 64 * 1024,
	}, func(stdin io.WriteCloser, stdout io.Reader) error {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 16*1024), 512*1024)

		readResponse := func(wantID int) (json.RawMessage, error) {
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				var resp struct {
					ID     *int            `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  *struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal([]byte(line), &resp) != nil {
					continue
				}
				if resp.Error != nil && resp.Error.Message != "" {
					return nil, errors.New(resp.Error.Message)
				}
				if resp.ID != nil && *resp.ID == wantID {
					return resp.Result, nil
				}
			}
			if scanErr := scanner.Err(); scanErr != nil {
				return nil, fmt.Errorf("读取 MCP 响应失败: %w", scanErr)
			}
			return nil, fmt.Errorf("MCP 进程在响应 id=%d 前提前关闭", wantID)
		}

		initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"slopguard-selfcheck","version":"1.0"}}}` + "\n"
		if _, wErr := io.WriteString(stdin, initReq); wErr != nil {
			return fmt.Errorf("发送 MCP initialize 失败: %w", wErr)
		}
		initResult, rErr := readResponse(1)
		if rErr != nil {
			return rErr
		}
		var initPayload struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		}
		if json.Unmarshal(initResult, &initPayload) != nil || strings.TrimSpace(initPayload.ServerInfo.Name) != "slopguard" {
			return fmt.Errorf("MCP initialize serverInfo 异常: %q", initPayload.ServerInfo.Name)
		}

		notif := `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}` + "\n"
		if _, wErr := io.WriteString(stdin, notif); wErr != nil {
			return fmt.Errorf("发送 notifications/initialized 失败: %w", wErr)
		}
		listReq := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n"
		if _, wErr := io.WriteString(stdin, listReq); wErr != nil {
			return fmt.Errorf("发送 tools/list 失败: %w", wErr)
		}
		_ = stdin.Close()

		listResult, rErr := readResponse(2)
		if rErr != nil {
			return rErr
		}
		var listRes struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(listResult, &listRes); err != nil {
			return fmt.Errorf("解析 MCP tools/list 失败: %w", err)
		}
		toolSet := make(map[string]bool, len(listRes.Tools))
		for _, t := range listRes.Tools {
			if t.Name != "" {
				parsedTools = append(parsedTools, t.Name)
				toolSet[t.Name] = true
			}
		}
		for _, required := range []string{"sg_verify", "sg_radar", "sg_list_inbox", "sg_submit_analysis"} {
			if !toolSet[required] {
				return fmt.Errorf("MCP 缺少核心工具 %s", required)
			}
		}
		return nil
	})
	latencyMS = time.Since(started).Milliseconds()
	if res.Err != nil {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = res.Err.Error()
		}
		handshakeErr = fmt.Errorf("MCP 握手失败: %s", msg)
		return nil, latencyMS, handshakeErr
	}
	if len(parsedTools) == 0 {
		return nil, latencyMS, errors.New("MCP 握手未返回任何工具列表")
	}
	return parsedTools, latencyMS, nil
}

func verifyAgentProfile(ctx context.Context, home, project string, prof agentProfile, db *store.Store, checkRealCLI bool) AgentVerifyReport {
	rep := AgentVerifyReport{
		AgentID:   prof.ID,
		Name:      prof.Name,
		Scope:     prof.Scope,
		Installed: prof.Detected,
		SkillOK:   prof.SkillTarget == nil || prof.SkillTarget.Configured,
	}

	if prof.Scope == "project" {
		rep.ProjectConfigured = prof.Enabled
	} else {
		rep.GlobalConfigured = prof.Enabled
	}

	globalEventCount := 0
	if db != nil {
		if events, err := db.ListMCPEvents(ctx, "", "", 50); err == nil {
			for _, ev := range events {
				if isFilesystemRootPath(ev.ProjectPath) {
					continue
				}
				globalEventCount++
				if rep.LastEventAt == "" {
					rep.LastEventAt = ev.CreatedAt.UTC().Format(time.RFC3339)
				}
			}
		}
		if project != "" {
			if projEvents, err := db.ListMCPEvents(ctx, project, "", 50); err == nil && len(projEvents) > 0 {
				rep.EventCount = len(projEvents)
				rep.LastEventAt = projEvents[0].CreatedAt.UTC().Format(time.RFC3339)
			}
		} else if prof.Scope == "global" {
			rep.EventCount = globalEventCount
		}
	}

	if prof.NeedsProject {
		rep.HealthStatus = "not_configured"
		rep.ActivationStatus = "not_configured"
		rep.Diagnosis = "请先在顶部下拉框选择或添加项目"
		return rep
	}
	if prof.MCPTarget == nil {
		if rep.SkillOK {
			rep.Configured = true
			rep.ConfigMatched = true
			rep.HealthStatus = "active"
			rep.ActivationStatus = "active"
			rep.Diagnosis = "规则文件已部署就绪"
		} else {
			rep.HealthStatus = "not_configured"
			rep.ActivationStatus = "not_configured"
			rep.Diagnosis = "尚未部署规则文件"
		}
		return rep
	}

	cmd, args, configured := readConfiguredMCPCommand(prof.MCPTarget.ConfigPath)
	rep.Configured = configured
	rep.BinaryPath = cmd

	if !configured {
		rep.HealthStatus = "not_configured"
		rep.ActivationStatus = "not_configured"
		rep.Diagnosis = "尚未开启接入"
		return rep
	}

	expectedProj := ""
	if prof.Scope == "project" {
		expectedProj = project
	}
	matchErr := validateTrustedSlopGuardCommand(cmd, args, expectedProj)
	rep.ConfigMatched = matchErr == nil
	rep.BinaryOK = isExecutableFile(cmd) && matchErr == nil
	if matchErr != nil {
		rep.HealthStatus = "broken"
		rep.ActivationStatus = "broken"
		rep.Diagnosis = matchErr.Error()
		return rep
	}
	if !rep.SkillOK {
		rep.HealthStatus = "broken"
		rep.ActivationStatus = "broken"
		rep.Diagnosis = "MCP 已写但缺少 SKILL 规则文件，建议重新开关补全"
		return rep
	}

	tools, latencyMS, hsErr := runStdioMCPHandshake(ctx, cmd, args)
	rep.LatencyMS = latencyMS
	rep.Tools = tools
	rep.HandshakeOK = hsErr == nil
	if hsErr != nil {
		rep.HealthStatus = "broken"
		rep.ActivationStatus = "broken"
		rep.Diagnosis = hsErr.Error()
		return rep
	}

	cliRegistered := false
	if prof.ID == "antigravity" && checkRealCLI {
		reg := isSlopGuardInAgyMCPList(home)
		rep.CLIRegistered = &reg
		cliRegistered = reg
	}

	if rep.EventCount > 0 || (prof.Scope == "global" && globalEventCount > 0) || cliRegistered {
		rep.HealthStatus = "active"
		if rep.EventCount > 0 {
			rep.ActivationStatus = "active"
			if prof.Scope == "global" && project != "" {
				rep.Diagnosis = fmt.Sprintf("MCP 握手通过 (%dms, %d个工具) · 当前项目已记录 %d 次调用", latencyMS, len(tools), rep.EventCount)
			} else {
				rep.Diagnosis = fmt.Sprintf("MCP 握手通过 (%dms, %d个工具) · 已记录 %d 次真实调用", latencyMS, len(tools), rep.EventCount)
			}
		} else if globalEventCount > 0 && prof.Scope == "global" {
			rep.ActivationStatus = "active"
			rep.Diagnosis = fmt.Sprintf("MCP 握手通过 (%dms, %d个工具) · 全局已记录 %d 次调用（当前项目 0 次）", latencyMS, len(tools), globalEventCount)
		} else {
			rep.ActivationStatus = "registered_idle"
			rep.Diagnosis = fmt.Sprintf("MCP 握手通过 (%dms, %d个工具) · 客户端 CLI 已注册", latencyMS, len(tools))
		}
		return rep
	}

	rep.HealthStatus = "pending_activation"
	rep.ActivationStatus = "pending_activation"
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
