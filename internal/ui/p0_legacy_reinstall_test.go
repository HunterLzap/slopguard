package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// p0DemoteBackupToLegacyV1 把一个 v2 备份记录降级为旧版记录（无安装快照），
// 写回磁盘并返回记录文件的原始字节，供“目标与备份均不得被修改”的断言使用。
func p0DemoteBackupToLegacyV1(t *testing.T, backupsDir, kind, id, target string, rec configBackupRecord) []byte {
	t.Helper()
	legacy := rec
	legacy.SchemaVersion = 0
	legacy.InstalledSHA256 = ""
	legacy.InstalledBytes = nil
	legacy.InstalledMode = 0
	legacy.InstalledAt = ""
	encoded, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy backup: %v", err)
	}
	encoded = append(encoded, '\n')
	recordPath := backupRecordPath(backupsDir, kind, id, target)
	if err := os.WriteFile(recordPath, encoded, 0o600); err != nil {
		t.Fatalf("write legacy backup: %v", err)
	}
	return encoded
}

func p0ReadFileOrFatal(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// 场景 1：旧版 MCP 备份缺安装快照，但当前内容仍等于原始基线 → ensureBackup 正常放行。
func TestP0LegacyReinstallBaselineCurrentEqualsOriginal(t *testing.T) {
	backupsDir := filepath.Join(t.TempDir(), "backups")
	target := filepath.Join(t.TempDir(), "claude.json")
	original := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"}}}\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, rec, err := ensureBackup(backupsDir, "mcp", "legacy-current-original", target)
	if err != nil {
		t.Fatalf("initial backup: %v", err)
	}
	p0DemoteBackupToLegacyV1(t, backupsDir, "mcp", "legacy-current-original", target, rec)

	// 当前内容 == OriginalBytes：可以证明没有 SlopGuard 安装残留，允许继续接入。
	if _, _, err := ensureBackup(backupsDir, "mcp", "legacy-current-original", target); err != nil {
		t.Fatalf("legacy backup with current==original should pass: %v", err)
	}
}

// 场景 2：旧版 MCP 备份缺安装快照，当前内容 = 原始内容 + 合法 slopguard 条目，
// migrateLegacyBackup 可证明归属 → ensureBackup 成功；安装快照落盘后卸载可恢复原字节。
func TestP0LegacyReinstallBaselineProvableMigration(t *testing.T) {
	backupsDir := filepath.Join(t.TempDir(), "backups")
	target := filepath.Join(t.TempDir(), "claude.json")
	original := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"}}}\n")
	installed := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"},\"slopguard\":{\"command\":\"slopguard\",\"args\":[\"mcp\"]}}}\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, rec, err := ensureBackup(backupsDir, "mcp", "legacy-provable", target)
	if err != nil {
		t.Fatalf("initial backup: %v", err)
	}
	p0DemoteBackupToLegacyV1(t, backupsDir, "mcp", "legacy-provable", target, rec)

	// 模拟旧版本已完成接入：当前内容 = 原始内容 + slopguard 条目。
	if err := os.WriteFile(target, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureBackup(backupsDir, "mcp", "legacy-provable", target); err != nil {
		t.Fatalf("provable legacy backup should be accepted: %v", err)
	}

	// 本次重新接入完成后记录安装快照。
	if err := recordBackupInstalled(backupsDir, backupTarget{Kind: "mcp", ID: "legacy-provable", Path: target}, installed, 0o600); err != nil {
		t.Fatalf("record installed snapshot: %v", err)
	}
	_, latest, ok, err := lookupBackupChecked(backupsDir, "mcp", "legacy-provable", target)
	if err != nil || !ok {
		t.Fatalf("lookup upgraded backup: ok=%v err=%v", ok, err)
	}
	if latest.SchemaVersion < backupRecordSchemaVersion || latest.InstalledSHA256 == "" || len(latest.InstalledBytes) == 0 {
		t.Fatalf("backup not upgraded after reinstall: %+v", latest)
	}
	plan, err := planManagedUninstall(backupsDir, backupTarget{Kind: "mcp", ID: "legacy-provable", Path: target}, latest)
	if err != nil {
		t.Fatalf("plan uninstall: %v", err)
	}
	if plan.Action != uninstallRestoreOriginal {
		t.Fatalf("plan action = %q, want restore_original", plan.Action)
	}
	if !bytes.Equal(plan.NewData, original) {
		t.Fatalf("restore data = %q, want original %q", plan.NewData, original)
	}
	if err := applyManagedUninstall(backupsDir, backupTarget{Kind: "mcp", ID: "legacy-provable", Path: target}, plan); err != nil {
		t.Fatalf("apply uninstall: %v", err)
	}
	if got := p0ReadFileOrFatal(t, target); !bytes.Equal(got, original) {
		t.Fatalf("restored content = %q, want %q", got, original)
	}
}

// 场景 3：旧版 MCP 备份缺安装快照，当前内容无法证明归属（安装后用户新增 server）
// → ensureBackup 必须返回 ErrLegacyBackupNeedsReview，且目标文件与备份记录字节不变。
func TestP0LegacyReinstallBaselineUnprovableMCP(t *testing.T) {
	backupsDir := filepath.Join(t.TempDir(), "backups")
	target := filepath.Join(t.TempDir(), "claude.json")
	original := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"}}}\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, rec, err := ensureBackup(backupsDir, "mcp", "legacy-unprovable", target)
	if err != nil {
		t.Fatalf("initial backup: %v", err)
	}
	recordPath := backupRecordPath(backupsDir, "mcp", "legacy-unprovable", target)
	beforeRecord := p0DemoteBackupToLegacyV1(t, backupsDir, "mcp", "legacy-unprovable", target, rec)

	// 旧版本安装后用户在 slopguard 之外又新增了 postgres server：
	// 剥离受管条目后的内容 != OriginalBytes，无法证明旧基线仍然有效。
	current := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"},\"slopguard\":{\"command\":\"slopguard\",\"args\":[\"mcp\"]},\"postgres\":{\"command\":\"postgres-mcp\"}}}\n")
	if err := os.WriteFile(target, current, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = ensureBackup(backupsDir, "mcp", "legacy-unprovable", target)
	if !errors.Is(err, ErrLegacyBackupNeedsReview) {
		t.Fatalf("ensureBackup err = %v, want ErrLegacyBackupNeedsReview (stale legacy baseline must not be silently reused)", err)
	}
	if got := p0ReadFileOrFatal(t, target); !bytes.Equal(got, current) {
		t.Fatalf("failed legacy reinstall changed target: %q -> %q", current, got)
	}
	if got := p0ReadFileOrFatal(t, recordPath); !bytes.Equal(got, beforeRecord) {
		t.Fatalf("failed legacy reinstall changed backup record: %q -> %q", beforeRecord, got)
	}
}

// 场景 4：旧版 skill 备份缺安装快照，当前内容被修改且无法证明归属
// → ErrLegacyBackupNeedsReview，文件与备份记录都不变。
func TestP0LegacyReinstallBaselineUnprovableSkill(t *testing.T) {
	backupsDir := filepath.Join(t.TempDir(), "backups")
	target := filepath.Join(t.TempDir(), "SKILL.md")
	original := []byte("# user skill\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, rec, err := ensureBackup(backupsDir, "skill", "skill-legacy-reinstall", target)
	if err != nil {
		t.Fatalf("initial backup: %v", err)
	}
	recordPath := backupRecordPath(backupsDir, "skill", "skill-legacy-reinstall", target)
	beforeRecord := p0DemoteBackupToLegacyV1(t, backupsDir, "skill", "skill-legacy-reinstall", target, rec)

	current := []byte("# user skill\n\nchanged after backup\n")
	if err := os.WriteFile(target, current, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = ensureBackup(backupsDir, "skill", "skill-legacy-reinstall", target)
	if !errors.Is(err, ErrLegacyBackupNeedsReview) {
		t.Fatalf("ensureBackup err = %v, want ErrLegacyBackupNeedsReview", err)
	}
	if got := p0ReadFileOrFatal(t, target); !bytes.Equal(got, current) {
		t.Fatalf("failed legacy skill reinstall changed target: %q -> %q", current, got)
	}
	if got := p0ReadFileOrFatal(t, recordPath); !bytes.Equal(got, beforeRecord) {
		t.Fatalf("failed legacy skill reinstall changed backup record: %q -> %q", beforeRecord, got)
	}
}
