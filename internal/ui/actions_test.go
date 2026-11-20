package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
	"github.com/HunterLzap/slopguard/internal/store"
)

func postDashboardJSON(t *testing.T, handler http.Handler, path string, payload any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestUnifiedDiffAndBackupRestore(t *testing.T) {
	diffNew := buildUnifiedDiff("demo/.mcp.json", "", "{\n  \"mcpServers\": {}\n}\n", 3)
	if !strings.Contains(diffNew, "--- /dev/null") || !strings.Contains(diffNew, "+++ b/demo/.mcp.json") || !strings.Contains(diffNew, "+  \"mcpServers\": {}") {
		t.Fatalf("unexpected new-file diff:\n%s", diffNew)
	}

	oldText := "line1\nline2\nline3\n"
	newText := "line1\nline2-edited\nline3\nline4\n"
	diffEdit := buildUnifiedDiff("demo/file.txt", oldText, newText, 2)
	if !strings.Contains(diffEdit, "--- a/demo/file.txt") || !strings.Contains(diffEdit, "-line2") || !strings.Contains(diffEdit, "+line2-edited") || !strings.Contains(diffEdit, "+line4") {
		t.Fatalf("unexpected edit diff:\n%s", diffEdit)
	}

	dir := t.TempDir()
	backupsDir := filepath.Join(dir, "backups")
	existingPath := filepath.Join(dir, "config.json")
	original := []byte("{\"existing\": true}\n")
	if err := os.WriteFile(existingPath, original, 0o640); err != nil {
		t.Fatal(err)
	}

	b1, firstRec, err := ensureBackup(backupsDir, "mcp", "claude", existingPath)
	if err != nil {
		t.Fatalf("first ensureBackup: %v", err)
	}
	if firstRec.OriginalSHA256 != fileSHA256Hex(original) {
		t.Fatalf("first backup original sha = %q, want %q", firstRec.OriginalSHA256, fileSHA256Hex(original))
	}
	installed := []byte("{\"existing\": false}\n")
	if err := os.WriteFile(existingPath, installed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recordBackupInstalled(backupsDir, backupTarget{Kind: "mcp", ID: "claude", Path: existingPath}, installed, 0o644); err != nil {
		t.Fatalf("record installed snapshot: %v", err)
	}
	// Second ensureBackup must preserve original backup contents, not overwrite it.
	b2, rec, err := ensureBackup(backupsDir, "mcp", "claude", existingPath)
	if err != nil {
		t.Fatalf("second ensureBackup: %v", err)
	}
	if b1 != b2 || string(rec.OriginalBytes) != string(original) {
		t.Fatalf("backup overwritten: b1=%s b2=%s rec=%q", b1, b2, rec.OriginalBytes)
	}
	if rec.InstalledSHA256 != fileSHA256Hex(installed) {
		t.Fatalf("installed sha = %q, want %q", rec.InstalledSHA256, fileSHA256Hex(installed))
	}

	restored, removed, err := restoreFromBackup(backupsDir, "mcp", "claude", existingPath)
	if err != nil || !restored || removed {
		t.Fatalf("restore existing: restored=%v removed=%v err=%v", restored, removed, err)
	}
	got, err := os.ReadFile(existingPath)
	if err != nil || string(got) != string(original) {
		t.Fatalf("restored content = %q, want %q (err=%v)", got, original, err)
	}

	// Newly created file should be deleted on restore.
	newSkillPath := filepath.Join(dir, ".claude", "skills", "slopguard", "SKILL.md")
	if _, _, err := ensureBackup(backupsDir, "skill", "project_claude_skill", newSkillPath); err != nil {
		t.Fatalf("ensureBackup new file: %v", err)
	}
	newSkillContent := []byte("# skill\n")
	if err := writeFileAtomic(newSkillPath, newSkillContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recordBackupInstalled(backupsDir, backupTarget{Kind: "skill", ID: "project_claude_skill", Path: newSkillPath}, newSkillContent, 0o644); err != nil {
		t.Fatalf("record installed skill snapshot: %v", err)
	}
	restored, removed, err = restoreFromBackup(backupsDir, "skill", "project_claude_skill", newSkillPath)
	if err != nil || !restored || !removed {
		t.Fatalf("restore new file: restored=%v removed=%v err=%v", restored, removed, err)
	}
	if _, err := os.Stat(newSkillPath); !os.IsNotExist(err) {
		t.Fatalf("expected new skill file to be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Dir(newSkillPath)); !os.IsNotExist(err) {
		t.Fatalf("expected empty slopguard dir to be cleaned up, stat err = %v", err)
	}
}

func TestMCPClientsPriorityPreviewInstallRestore(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "my-app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	globalClaudePath := filepath.Join(home, ".claude.json")
	originalClaude := `{"theme":"dark","mcpServers":{"other":{"command":"echo"}}}` + "\n"
	if err := os.WriteFile(globalClaudePath, []byte(originalClaude), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewServer(nil, nil)
	s.homeDir = home
	handler := authedTestHandler(s)

	// 1. Project .mcp.json should appear first and be marked recommended.
	status, body, _ := dashboardRequest(t, handler, http.MethodGet, "/api/mcp-clients?project="+url.QueryEscape(project))
	if status != http.StatusOK {
		t.Fatalf("GET /api/mcp-clients = %d: %s", status, body)
	}
	resp := decodeDashboardJSON(t, body)
	clients, _ := resp["clients"].([]any)
	if len(clients) < 2 {
		t.Fatalf("clients = %v", clients)
	}
	first, _ := clients[0].(map[string]any)
	if first["id"] != "project_mcp" || first["recommended"] != true {
		t.Fatalf("first client = %v, want project_mcp recommended", first)
	}
	if skillsList, _ := resp["skills"].([]any); len(skillsList) == 0 {
		t.Fatalf("expected skill targets in /api/mcp-clients response")
	}

	// 2. Preview global claude_code must not touch disk.
	code, prevBody := postDashboardJSON(t, handler, "/api/mcp-clients/preview", map[string]any{
		"client_id": "claude_code",
		"project":   project,
		"action":    "install",
	})
	if code != http.StatusOK {
		t.Fatalf("preview global = %d: %s", code, prevBody)
	}
	prevJSON := decodeDashboardJSON(t, prevBody)
	diffText, _ := prevJSON["diff"].(string)
	if prevJSON["has_changes"] != true || !strings.Contains(diffText, `"slopguard"`) {
		t.Fatalf("preview diff missing slopguard: %v", prevJSON)
	}
	previewID, _ := prevJSON["preview_id"].(string)
	if previewID == "" {
		t.Fatalf("preview missing preview_id: %v", prevJSON)
	}
	afterPreview, _ := os.ReadFile(globalClaudePath)
	if string(afterPreview) != originalClaude {
		t.Fatalf("preview mutated disk: %s", afterPreview)
	}

	// 3. Mutations require both confirmed=true and a matching preview_id.
	code, errBody := postDashboardJSON(t, handler, "/api/mcp-clients/install", map[string]any{
		"client_id":  "claude_code",
		"project":    project,
		"preview_id": previewID,
		"confirmed":  false,
	})
	if code != http.StatusConflict {
		t.Fatalf("unconfirmed global install = %d, want 409: %s", code, errBody)
	}
	code, errBody = postDashboardJSON(t, handler, "/api/mcp-clients/install", map[string]any{
		"client_id": "claude_code",
		"project":   project,
		"confirmed": true,
	})
	if code != http.StatusConflict {
		t.Fatalf("install without preview_id = %d, want 409: %s", code, errBody)
	}

	// 4. Global install with confirmed=true writes slopguard while preserving other servers + backup.
	code, instBody := postDashboardJSON(t, handler, "/api/mcp-clients/install", map[string]any{
		"client_id":       "claude_code",
		"project":         project,
		"preview_id":      previewID,
		"confirmed":       true,
		"expected_hashes": prevJSON["base_hashes"],
	})
	if code != http.StatusOK {
		t.Fatalf("confirmed global install = %d: %s", code, instBody)
	}
	updatedClaude, err := os.ReadFile(globalClaudePath)
	if err != nil || !strings.Contains(string(updatedClaude), `"slopguard"`) || !strings.Contains(string(updatedClaude), `"other"`) {
		t.Fatalf("updated claude.json = %q (err=%v)", updatedClaude, err)
	}

	// 5. Restore should revert exact original contents.
	_, restorePrevBody := postDashboardJSON(t, handler, "/api/mcp-clients/preview", map[string]any{
		"client_id": "claude_code",
		"project":   project,
		"action":    "restore",
	})
	restorePrev := decodeDashboardJSON(t, restorePrevBody)
	restoreID, _ := restorePrev["preview_id"].(string)
	if restoreID == "" {
		t.Fatalf("restore preview missing preview_id: %v", restorePrev)
	}
	code, restBody := postDashboardJSON(t, handler, "/api/mcp-clients/restore", map[string]any{
		"client_id":       "claude_code",
		"project":         project,
		"preview_id":      restoreID,
		"confirmed":       true,
		"expected_hashes": restorePrev["base_hashes"],
	})
	if code != http.StatusOK {
		t.Fatalf("restore = %d: %s", code, restBody)
	}
	restJSON := decodeDashboardJSON(t, restBody)
	if restJSON["restored_from_backup"] != true || restJSON["configured"] != false {
		t.Fatalf("restore resp = %v", restJSON)
	}
	restoredClaude, _ := os.ReadFile(globalClaudePath)
	if string(restoredClaude) != originalClaude {
		t.Fatalf("restored content = %q, want %q", restoredClaude, originalClaude)
	}
}

func TestSkillTargetsPreviewInstallRestore(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "skill-proj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewServer(nil, nil)
	s.homeDir = home
	handler := authedTestHandler(s)

	skillPath := filepath.Join(project, ".claude", "skills", "slopguard", "SKILL.md")

	// Preview project skill without modifying disk.
	code, prevBody := postDashboardJSON(t, handler, "/api/skills/preview", map[string]any{
		"target_id": "project_claude_skill",
		"project":   project,
		"action":    "install",
	})
	if code != http.StatusOK {
		t.Fatalf("skill preview = %d: %s", code, prevBody)
	}
	prevJSON := decodeDashboardJSON(t, prevBody)
	if prevJSON["has_changes"] != true || !strings.Contains(fmtStr(prevJSON["diff"]), "sg_verify") {
		t.Fatalf("unexpected skill preview = %v", prevJSON)
	}
	previewID, _ := prevJSON["preview_id"].(string)
	if previewID == "" {
		t.Fatalf("skill preview missing preview_id: %v", prevJSON)
	}
	if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
		t.Fatalf("preview created file unexpectedly")
	}

	// Install project skill.
	code, instBody := postDashboardJSON(t, handler, "/api/skills/install", map[string]any{
		"target_id":       "project_claude_skill",
		"project":         project,
		"preview_id":      previewID,
		"confirmed":       true,
		"expected_hashes": prevJSON["base_hashes"],
	})
	if code != http.StatusOK {
		t.Fatalf("skill install = %d: %s", code, instBody)
	}
	if !isSkillConfigured(skillPath) {
		t.Fatalf("expected installed skill at %s", skillPath)
	}

	// Restore removes newly created skill file cleanly.
	_, restorePrevBody := postDashboardJSON(t, handler, "/api/skills/preview", map[string]any{
		"target_id": "project_claude_skill",
		"project":   project,
		"action":    "restore",
	})
	restorePrev := decodeDashboardJSON(t, restorePrevBody)
	restoreID, _ := restorePrev["preview_id"].(string)
	if restoreID == "" {
		t.Fatalf("skill restore preview missing preview_id: %v", restorePrev)
	}
	code, restBody := postDashboardJSON(t, handler, "/api/skills/restore", map[string]any{
		"target_id":       "project_claude_skill",
		"project":         project,
		"preview_id":      restoreID,
		"confirmed":       true,
		"expected_hashes": restorePrev["base_hashes"],
	})
	if code != http.StatusOK {
		t.Fatalf("skill restore = %d: %s", code, restBody)
	}
	if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
		t.Fatalf("expected restored skill file to be removed, stat err = %v", err)
	}
}

func TestGuardToggleGitHookAndDebounceWatcher(t *testing.T) {
	home := t.TempDir()
	db := openDashboardStore(t)
	project := filepath.Join(t.TempDir(), "guarded-py")
	writeDashboardFile(t, filepath.Join(project, "pyproject.toml"), "[project]\nname = \"demo\"\n")
	writeDashboardFile(t, filepath.Join(project, "app.py"), "import os\n")
	writeDashboardFile(t, filepath.Join(project, ".slopguard", "baseline.json"), `{"metrics":{"noqa":0}}`)
	writeDashboardFile(t, filepath.Join(project, ".git", "HEAD"), "ref: refs/heads/main\n")
	hookPath := filepath.Join(project, ".git", "hooks", "post-commit")
	originalHook := "#!/bin/sh\necho custom-existing-hook\n"
	writeDashboardFile(t, hookPath, originalHook)

	runner := &fakeUIRunner{
		lookups: map[string]string{
			"ruff":    "/usr/local/bin/ruff",
			"pyright": "/usr/local/bin/pyright",
		},
		runFunc: func(_ context.Context, spec contract.RunSpec) contract.RunResult {
			if strings.Contains(spec.Name, "ruff") {
				return contract.RunResult{
					ExitCode: 1,
					Stdout:   `[{"code":"F401","message":"'os' imported but unused","filename":"app.py","location":{"row":1,"column":8}}]`,
				}
			}
			return contract.RunResult{ExitCode: 0}
		},
	}

	s := NewServer(db, runner)
	s.homeDir = home
	s.guard.pollInterval = 25 * time.Millisecond
	handler := authedTestHandler(s)

	// 1. Enable guard + hook and verify existing post-commit content is preserved.
	hookPreview := mustPreview(t, handler, "/api/guard/hook/preview", map[string]any{
		"project":      project,
		"hook_enabled": true,
	})
	code, body := postDashboardJSON(t, handler, "/api/guard/toggle", map[string]any{
		"project":      project,
		"enabled":      true,
		"hook_enabled": true,
		"debounce_sec": 1,
		"preview_id":   previewIDOf(t, hookPreview),
		"confirmed":    true,
	})
	if code != http.StatusOK {
		t.Fatalf("guard toggle on = %d: %s", code, body)
	}
	hookBytes, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	hookStr := string(hookBytes)
	if !strings.Contains(hookStr, "echo custom-existing-hook") || !strings.Contains(hookStr, gitHookStartMarker) || !strings.Contains(hookStr, "--last-commit") {
		t.Fatalf("post-commit hook content unexpected:\n%s", hookStr)
	}

	// 2. Start background watcher, modify a file, and wait for debounced auto-verify + mistake capture.
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	s.guard.Start(watchCtx)
	defer s.guard.Stop()

	// Let initial baseline snapshot settle.
	time.Sleep(60 * time.Millisecond)
	writeDashboardFile(t, filepath.Join(project, "app.py"), "import sys\n")

	deadline := time.Now().Add(4 * time.Second)
	captured := false
	for time.Now().Before(deadline) {
		records, qErr := db.QueryMistakes(context.Background(), store.MistakeQuery{ProjectPath: project})
		if qErr == nil && len(records) > 0 {
			if records[0].Source == "auto_watch" && records[0].ReviewStatus == "pending_ai" {
				captured = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !captured {
		records, _ := db.QueryMistakes(context.Background(), store.MistakeQuery{ProjectPath: project})
		st := s.guard.ProjectStatus(project)
		t.Fatalf("expected auto_watch mistake in pending_ai within deadline, got %+v (guardStatus=%+v)", records, st)
	}

	// 3. Uninstall hook and verify original hook restored verbatim.
	hookOffPreview := mustPreview(t, handler, "/api/guard/hook/preview", map[string]any{
		"project":      project,
		"hook_enabled": false,
	})
	code, body = postDashboardJSON(t, handler, "/api/guard/toggle", map[string]any{
		"project":      project,
		"hook_enabled": false,
		"preview_id":   previewIDOf(t, hookOffPreview),
		"confirmed":    true,
	})
	if code != http.StatusOK {
		t.Fatalf("guard toggle hook off = %d: %s", code, body)
	}
	restoredHook, _ := os.ReadFile(hookPath)
	if string(restoredHook) != originalHook {
		t.Fatalf("restored hook = %q, want %q", restoredHook, originalHook)
	}
}

func TestLegacySessionScanEndpointRemoved(t *testing.T) {
	s := NewServer(openDashboardStore(t), nil)
	handler := authedTestHandler(s)
	code, _ := postDashboardJSON(t, handler, "/api/sessions/scan", map[string]any{})
	if code != http.StatusNotFound {
		t.Fatalf("POST /api/sessions/scan = %d, want 404 (replaced by Agent prompt workflow)", code)
	}
}

func TestSummaryAndVerifyAutoCapture(t *testing.T) {
	home := t.TempDir()
	db := openDashboardStore(t)
	project := filepath.Join(t.TempDir(), "py-summary")
	writeDashboardFile(t, filepath.Join(project, "pyproject.toml"), "[project]\nname = \"demo\"\n")
	writeDashboardFile(t, filepath.Join(project, "app.py"), "x = 1\n")

	runner := &fakeUIRunner{
		lookups: map[string]string{
			"ruff":    "/usr/local/bin/ruff",
			"pyright": "/usr/local/bin/pyright",
		},
		runFunc: func(_ context.Context, spec contract.RunSpec) contract.RunResult {
			if strings.Contains(spec.Name, "ruff") {
				return contract.RunResult{
					ExitCode: 1,
					Stdout:   `[{"code":"F401","message":"unused import","filename":"app.py","location":{"row":1,"column":1}}]`,
				}
			}
			return contract.RunResult{ExitCode: 0}
		},
	}

	s := NewServer(db, runner)
	s.homeDir = home
	handler := authedTestHandler(s)

	// POST /api/verify with record=true should auto-capture findings into pending_ai.
	code, vBody := postDashboardJSON(t, handler, "/api/verify", map[string]any{
		"project": project,
		"all":     true,
		"record":  true,
	})
	if code != http.StatusOK {
		t.Fatalf("verify = %d: %s", code, vBody)
	}

	// Mark one mistake as analyzed so we verify both pending_ai and pending_user counts.
	if _, err := db.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: project,
		Fingerprint: "fp-ready-user",
		Category:    "type",
		Summary:     "missing type",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SubmitMistakeAnalysis(context.Background(), "fp-ready-user", "recommend", "建议补充类型注解", ""); err != nil {
		t.Fatal(err)
	}

	status, body, _ := dashboardRequest(t, handler, http.MethodGet, "/api/summary")
	if status != http.StatusOK {
		t.Fatalf("summary = %d: %s", status, body)
	}
	sumJSON := decodeDashboardJSON(t, body)
	if sumJSON["pending_ai"] != float64(1) || sumJSON["pending_user"] != float64(1) {
		t.Fatalf("summary counts = %v, want pending_ai=1 pending_user=1", sumJSON)
	}
	if sumJSON["has_scans"] != true || sumJSON["last_scan_passed"] != false || sumJSON["last_scan_project"] != "py-summary" {
		t.Fatalf("summary scan fields = %v", sumJSON)
	}
}

func fmtStr(v any) string {
	s, _ := v.(string)
	return s
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func mustPreview(t *testing.T, handler http.Handler, path string, payload map[string]any) map[string]any {
	t.Helper()
	code, body := postDashboardJSON(t, handler, path, payload)
	if code != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", path, code, body)
	}
	return decodeDashboardJSON(t, body)
}

func previewIDOf(t *testing.T, preview map[string]any) string {
	t.Helper()
	id, _ := preview["preview_id"].(string)
	if id == "" {
		t.Fatalf("preview missing preview_id: %v", preview)
	}
	return id
}

func TestSharedSkillAndUnifiedAgentsWorkflow(t *testing.T) {
	home := t.TempDir()
	db := openDashboardStore(t)
	project := filepath.Join(t.TempDir(), "agent-repo")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	agMCPPath := filepath.Join(home, ".gemini", "antigravity", "mcp_config.json")
	agSkillPath := filepath.Join(home, ".gemini", "antigravity", "skills", "slopguard", "SKILL.md")
	originalMCP := "{\n  \"mcpServers\": {\n    \"custom\": {\"command\": \"echo\"}\n  }\n}\n"
	writeDashboardFile(t, agMCPPath, originalMCP)

	s := NewServer(db, nil)
	s.homeDir = home
	handler := authedTestHandler(s)

	// 1. Default shared skill state.
	stCode, stBody, _ := dashboardRequest(t, handler, http.MethodGet, "/api/shared-skill")
	if stCode != http.StatusOK {
		t.Fatalf("GET /api/shared-skill = %d: %s", stCode, stBody)
	}
	stJSON := decodeDashboardJSON(t, stBody)
	if stJSON["is_custom"] != false || !strings.Contains(fmtStr(stJSON["content"]), "sg_verify") {
		t.Fatalf("unexpected default shared skill response: %v", stJSON)
	}

	// 2. Customize shared skill before enabling Antigravity.
	customDoc := "# SlopGuard Custom Guardrail\n\nAlways run sg_verify and sg_submit_analysis.\n"
	sharedPreview := mustPreview(t, handler, "/api/shared-skill/preview", map[string]any{
		"content":      customDoc,
		"sync_enabled": true,
		"project":      project,
	})
	cCode, cBody := postDashboardJSON(t, handler, "/api/shared-skill", map[string]any{
		"content":      customDoc,
		"sync_enabled": true,
		"project":      project,
		"preview_id":   previewIDOf(t, sharedPreview),
		"confirmed":    true,
	})
	if cCode != http.StatusOK {
		t.Fatalf("POST /api/shared-skill = %d: %s", cCode, cBody)
	}

	// 3. Global agent toggle rejects missing confirmation even with a valid preview.
	agentPreview := mustPreview(t, handler, "/api/agents/preview", map[string]any{
		"agent_id": "antigravity",
		"project":  project,
		"action":   "install",
	})
	unCode, _ := postDashboardJSON(t, handler, "/api/agents/toggle", map[string]any{
		"agent_id":   "antigravity",
		"project":    project,
		"enable":     true,
		"preview_id": previewIDOf(t, agentPreview),
		"confirmed":  false,
	})
	if unCode != http.StatusConflict {
		t.Fatalf("expected 409 without confirmation for global agent, got %d", unCode)
	}

	// 4. Preview returns combined diff without touching disk.
	pJSON := agentPreview
	if pJSON["has_changes"] != true || !strings.Contains(fmtStr(pJSON["diff"]), "SlopGuard Custom Guardrail") {
		t.Fatalf("unexpected agent preview: %v", pJSON)
	}
	if _, err := os.Stat(agSkillPath); !os.IsNotExist(err) {
		t.Fatalf("preview unexpectedly created skill file")
	}

	// 5. Confirmed toggle enables both MCP + custom shared Skill, preserving custom MCP.
	tCode, tBody := postDashboardJSON(t, handler, "/api/agents/toggle", map[string]any{
		"agent_id":        "antigravity",
		"project":         project,
		"enable":          true,
		"preview_id":      previewIDOf(t, agentPreview),
		"confirmed":       true,
		"expected_hashes": agentPreview["base_hashes"],
	})
	if tCode != http.StatusOK {
		t.Fatalf("POST /api/agents/toggle enable = %d: %s", tCode, tBody)
	}
	if !isSlopGuardConfigured(agMCPPath) || !isSkillConfigured(agSkillPath) {
		t.Fatalf("expected antigravity MCP + Skill configured")
	}
	skillBytes, _ := os.ReadFile(agSkillPath)
	if !strings.Contains(string(skillBytes), "SlopGuard Custom Guardrail") {
		t.Fatalf("expected custom shared skill synced, got:\n%s", string(skillBytes))
	}

	// 6. GET /api/agents reports antigravity enabled & project disabled until toggled.
	aCode, aBody, _ := dashboardRequest(t, handler, http.MethodGet, "/api/agents?project="+url.QueryEscape(project))
	if aCode != http.StatusOK {
		t.Fatalf("GET /api/agents = %d: %s", aCode, aBody)
	}
	aJSON := decodeDashboardJSON(t, aBody)
	agentsList, _ := aJSON["agents"].([]any)
	foundAGEnabled := false
	for _, raw := range agentsList {
		m, _ := raw.(map[string]any)
		if m["id"] == "antigravity" && m["enabled"] == true && m["has_backup"] == true {
			foundAGEnabled = true
		}
	}
	if !foundAGEnabled {
		t.Fatalf("expected antigravity enabled=true has_backup=true in %v", agentsList)
	}

	// 7. Resetting shared skill syncs back default text to already-enabled agent.
	resetPreview := mustPreview(t, handler, "/api/shared-skill/preview", map[string]any{
		"reset":        true,
		"sync_enabled": true,
		"project":      project,
	})
	rCode, rBody := postDashboardJSON(t, handler, "/api/shared-skill", map[string]any{
		"reset":        true,
		"sync_enabled": true,
		"project":      project,
		"preview_id":   previewIDOf(t, resetPreview),
		"confirmed":    true,
	})
	if rCode != http.StatusOK {
		t.Fatalf("reset shared skill = %d: %s", rCode, rBody)
	}
	rJSON := decodeDashboardJSON(t, rBody)
	if rJSON["is_custom"] != false {
		t.Fatalf("expected is_custom=false after reset: %v", rJSON)
	}
	skillAfterReset, _ := os.ReadFile(agSkillPath)
	if strings.Contains(string(skillAfterReset), "SlopGuard Custom Guardrail") || !strings.Contains(string(skillAfterReset), "sg_verify") {
		t.Fatalf("expected synced skill reset to default, got:\n%s", string(skillAfterReset))
	}

	// 8. Disabling antigravity restores original MCP config and removes created skill file.
	disablePreview := mustPreview(t, handler, "/api/agents/preview", map[string]any{
		"agent_id": "antigravity",
		"project":  project,
		"action":   "restore",
	})
	dCode, dBody := postDashboardJSON(t, handler, "/api/agents/toggle", map[string]any{
		"agent_id":        "antigravity",
		"project":         project,
		"enable":          false,
		"preview_id":      previewIDOf(t, disablePreview),
		"confirmed":       true,
		"expected_hashes": disablePreview["base_hashes"],
	})
	if dCode != http.StatusOK {
		t.Fatalf("POST /api/agents/toggle disable = %d: %s", dCode, dBody)
	}
	restoredMCP, _ := os.ReadFile(agMCPPath)
	if string(restoredMCP) != originalMCP {
		t.Fatalf("expected restored original MCP config, got:\n%s", string(restoredMCP))
	}
	if _, err := os.Stat(agSkillPath); !os.IsNotExist(err) {
		t.Fatalf("expected restored skill file removed, stat err=%v", err)
	}
}

func TestMistakeLocalHintCacheExclusionAndSetProjectConfig(t *testing.T) {
	home := t.TempDir()
	db := openDashboardStore(t)
	project := filepath.Join(t.TempDir(), "py-repo")
	writeDashboardFile(t, filepath.Join(project, "app.py"), "x = 1\n")
	writeDashboardFile(t, filepath.Join(project, ".cache", "uv", "pkg", "mod.py"), "y = 2 # noqa\n")
	writeDashboardFile(t, filepath.Join(project, ".git", "HEAD"), "ref: refs/heads/main\n")
	if err := os.MkdirAll(filepath.Join(project, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Verify ListFiles excludes .cache/uv third-party archives.
	files, err := engine.ListFiles(project, nil)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	for _, f := range files {
		if strings.Contains(f, ".cache") {
			t.Fatalf("expected .cache files to be excluded, found %s", f)
		}
	}

	// Verify Mistake API adds local_hint.
	if _, err := db.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: project,
		Fingerprint: "fp-none-member",
		Category:    "type",
		Summary:     `[pyright:reportOptionalMemberAccess] "auth" is not a known attribute of "None"`,
		Source:      "auto_verify",
	}); err != nil {
		t.Fatal(err)
	}

	s := NewServer(db, nil)
	s.homeDir = home
	handler := authedTestHandler(s)

	// SetProjectConfig enables watch + git hook cleanly.
	enTrue := true
	hkTrue := true
	st, err := s.guard.SetProjectConfig(project, &enTrue, &hkTrue, defaultGuardDebounceSec)
	if err != nil {
		t.Fatalf("SetProjectConfig: %v", err)
	}
	if !st.Enabled || !st.HookInstalled {
		t.Fatalf("expected guard watch + hook active, got %+v", st)
	}

	code, body, _ := dashboardRequest(t, handler, http.MethodGet, "/api/mistakes?project="+url.QueryEscape(project))
	if code != http.StatusOK {
		t.Fatalf("GET /api/mistakes = %d: %s", code, body)
	}
	mJSON := decodeDashboardJSON(t, body)
	items, _ := mJSON["mistakes"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 mistake, got %v", items)
	}
	first, _ := items[0].(map[string]any)
	hint := fmtStr(first["local_hint"])
	if !strings.Contains(hint, "None") {
		t.Fatalf("expected Chinese local_hint explaining None access, got %q", hint)
	}
}

func TestMCPAnalysisSubmitAndApplyRecommendations(t *testing.T) {
	home := t.TempDir()
	db := openDashboardStore(t)
	project := filepath.Join(t.TempDir(), "py-rules")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := db.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: project,
		Fingerprint: "fp-cache-noise",
		Category:    "lint",
		Summary:     `[ruff] warning: Invalid '# noqa' directive on .cache/uv/archive/pkg.py:1`,
		BadSnippet:  ".cache/uv/archive/pkg.py:1 - Invalid `# noqa`",
		Source:      "auto_verify",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: project,
		Fingerprint: "fp-pyright-none",
		Category:    "type",
		Summary:     `[pyright:reportOptionalMemberAccess] "auth" is not a known attribute of "None"`,
		Source:      "auto_verify",
	}); err != nil {
		t.Fatal(err)
	}

	s := NewServer(db, nil)
	s.homeDir = home
	handler := authedTestHandler(s)

	// 1. Simulate host Agent submitting analysis back via MCP (sg_submit_analysis).
	if err := db.SubmitMistakeAnalysisScoped(context.Background(), project, "fp-cache-noise", "skip", "属于缓存目录噪音，建议忽略。", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SubmitMistakeAnalysisScoped(context.Background(), project, "fp-pyright-none", "recommend", "模型判断属于真实类型安全风险，建议入库拦截。", `[pyright:reportOptionalMemberAccess]`); err != nil {
		t.Fatal(err)
	}

	// Verify both moved to pending_user with expected decisions.
	pendingUser, err := db.ListInboxMistakes(context.Background(), project, "pending_user")
	if err != nil || len(pendingUser) != 2 {
		t.Fatalf("pending_user = %+v (err=%v)", pendingUser, err)
	}
	byFP := map[string]store.MistakeRecord{}
	for _, m := range pendingUser {
		byFP[m.Fingerprint] = m
	}
	if byFP["fp-cache-noise"].AIDecision != "skip" || byFP["fp-pyright-none"].AIDecision != "recommend" {
		t.Fatalf("unexpected decisions: %+v", byFP)
	}

	// 2. Approving mistake creates a rule entry in rules ledger without duplicating on repeat approve.
	for i := 0; i < 2; i++ {
		apCode, apBody := postDashboardJSON(t, handler, "/api/mistakes/fp-pyright-none/approve", map[string]any{})
		if apCode != http.StatusOK {
			t.Fatalf("approve = %d: %s", apCode, apBody)
		}
	}

	rules, err := db.ListRuleLedger(context.Background(), store.RuleLedgerQuery{ProjectPath: project})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Engine != "pyright" || rules[0].RuleID != "reportOptionalMemberAccess" {
		t.Fatalf("rules ledger = %+v, want 1 pyright reportOptionalMemberAccess rule", rules)
	}

	// 3. Batch apply recommendations atomically promotes recommend items and ignores skip items.
	if _, err := db.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: project,
		Fingerprint: "fp-rec-eslint",
		Category:    "type",
		Summary:     `[eslint] @typescript-eslint/no-explicit-any`,
		Source:      "auto_verify",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: project,
		Fingerprint: "fp-skip-cache2",
		Category:    "lint",
		Summary:     `warning in /.venv/lib/python3.12/foo.py`,
		Source:      "auto_verify",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SubmitMistakeAnalysisScoped(context.Background(), project, "fp-rec-eslint", "recommend", "建议禁止 explicit any", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SubmitMistakeAnalysisScoped(context.Background(), project, "fp-skip-cache2", "skip", "虚拟环境误报", ""); err != nil {
		t.Fatal(err)
	}

	apAllCode, apAllBody := postDashboardJSON(t, handler, "/api/mistakes/apply-recommendations", map[string]any{
		"project": project,
	})
	if apAllCode != http.StatusOK {
		t.Fatalf("apply-recommendations = %d: %s", apAllCode, apAllBody)
	}
	apAllJSON := decodeDashboardJSON(t, apAllBody)
	if apAllJSON["applied_rules"] != float64(1) || apAllJSON["ignored"] != float64(2) {
		t.Fatalf("apply-recommendations = %v, want applied_rules=1 ignored=2", apAllJSON)
	}
	allRulesAfter, _ := db.ListRuleLedger(context.Background(), store.RuleLedgerQuery{ProjectPath: project})
	if len(allRulesAfter) != 2 {
		t.Fatalf("expected 2 rules after apply-recommendations, got %d", len(allRulesAfter))
	}
}

func TestCodexToggleAndVerifyBrokenCursor(t *testing.T) {
	home := t.TempDir()
	db := openDashboardStore(t)
	codexTOMLPath := filepath.Join(home, ".codex", "config.toml")
	codexSkillPath := filepath.Join(home, ".codex", "skills", "slopguard", "SKILL.md")
	origTOML := "model = \"gpt-5\"\n\n[mcp_servers.computer-use]\ncommand = \"echo\"\nargs = [\"mcp\"]\n"
	writeDashboardFile(t, codexTOMLPath, origTOML)

	// Broken cursor config pointing to missing binary.
	writeDashboardFile(t, filepath.Join(home, ".cursor", "mcp.json"), `{"mcpServers":{"slopguard":{"command":"/nonexistent/slopguard-bin","args":["mcp"]}}}`)

	s := NewServer(db, nil)
	s.homeDir = home
	handler := authedTestHandler(s)

	// Enable Codex (MCP TOML + Skill).
	codexPreview := mustPreview(t, handler, "/api/agents/preview", map[string]any{
		"agent_id": "codex",
		"action":   "install",
	})
	cCode, cBody := postDashboardJSON(t, handler, "/api/agents/toggle", map[string]any{
		"agent_id":        "codex",
		"enable":          true,
		"preview_id":      previewIDOf(t, codexPreview),
		"confirmed":       true,
		"expected_hashes": codexPreview["base_hashes"],
	})
	if cCode != http.StatusOK {
		t.Fatalf("toggle codex on = %d: %s", cCode, cBody)
	}
	tomlData, _ := os.ReadFile(codexTOMLPath)
	if !strings.Contains(string(tomlData), "[mcp_servers.computer-use]") || !strings.Contains(string(tomlData), "[mcp_servers.slopguard]") {
		t.Fatalf("codex toml unexpected:\n%s", string(tomlData))
	}
	if !isSkillConfigured(codexSkillPath) {
		t.Fatalf("expected codex SKILL.md created")
	}

	// Verify cursor reports broken binary & diagnosis.
	vCode, vBody := postDashboardJSON(t, handler, "/api/agents/verify", map[string]any{
		"agent_id": "cursor",
	})
	if vCode != http.StatusOK {
		t.Fatalf("POST /api/agents/verify = %d: %s", vCode, vBody)
	}
	vJSON := decodeDashboardJSON(t, vBody)
	reports, _ := vJSON["reports"].([]any)
	if len(reports) != 1 {
		t.Fatalf("expected 1 verify report, got %v", vJSON)
	}
	repMap, _ := reports[0].(map[string]any)
	if repMap["health_status"] != "broken" || repMap["binary_ok"] != false {
		t.Fatalf("expected broken status for missing binary, got %v", repMap)
	}

	// Restore Codex reverts TOML cleanly.
	codexOffPreview := mustPreview(t, handler, "/api/agents/preview", map[string]any{
		"agent_id": "codex",
		"action":   "restore",
	})
	rCode, rBody := postDashboardJSON(t, handler, "/api/agents/toggle", map[string]any{
		"agent_id":        "codex",
		"enable":          false,
		"preview_id":      previewIDOf(t, codexOffPreview),
		"confirmed":       true,
		"expected_hashes": codexOffPreview["base_hashes"],
	})
	if rCode != http.StatusOK {
		t.Fatalf("toggle codex off = %d: %s", rCode, rBody)
	}
	restoredTOML, _ := os.ReadFile(codexTOMLPath)
	if string(restoredTOML) != origTOML {
		t.Fatalf("expected restored TOML:\n%q\ngot:\n%q", origTOML, string(restoredTOML))
	}
}

func TestP0AgentTrustEnvWhitelistAndAISafety(t *testing.T) {
	home := t.TempDir()
	db := openDashboardStore(t)

	// P0-04: Parent secrets must NEVER be inherited into agent CLI env.
	t.Setenv("OPENAI_API_KEY", "sk-secret-parent-key")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	envList := augmentedCLIEnv(home)
	joinedEnv := strings.Join(envList, "\n")
	for _, secret := range []string{"sk-secret-parent-key", "sk-ant-secret-key", "aws-secret"} {
		if strings.Contains(joinedEnv, secret) {
			t.Fatalf("augmentedCLIEnv leaked parent secret %q", secret)
		}
	}
	if !strings.Contains(joinedEnv, "HOME="+home) {
		t.Fatalf("augmentedCLIEnv missing HOME=%s", home)
	}

	// P0-02: Untrusted MCP command or invalid args must fail verification without executing.
	markerPath := filepath.Join(home, "pwned.marker")
	writeDashboardFile(t, filepath.Join(home, ".cursor", "mcp.json"),
		`{"mcpServers":{"slopguard":{"command":"/bin/sh","args":["-c",`+jsonQuote("touch "+markerPath)+`]}}}`)
	writeDashboardFile(t, filepath.Join(home, ".codex", "config.toml"),
		"[mcp_servers.slopguard]\ncommand = \"/bin/echo\"\nargs = [\"mcp\"]\n")

	s := NewServer(db, nil)
	s.homeDir = home
	handler := authedTestHandler(s)

	for _, agentID := range []string{"cursor", "codex"} {
		vCode, vBody := postDashboardJSON(t, handler, "/api/agents/verify", map[string]any{"agent_id": agentID})
		if vCode != http.StatusOK {
			t.Fatalf("verify %s = %d: %s", agentID, vCode, vBody)
		}
		vJSON := decodeDashboardJSON(t, vBody)
		reports, _ := vJSON["reports"].([]any)
		rep, _ := reports[0].(map[string]any)
		if rep["health_status"] != "broken" || rep["config_matched"] != false {
			t.Fatalf("expected untrusted binary for %s to fail config_matched, got %v", agentID, rep)
		}
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("untrusted MCP command was executed!")
	}

	// Legacy headless CLI AI runner endpoints must remain completely removed (404).
	for _, removedEndpoint := range []string{"/api/ai/config", "/api/mistakes/analyze-all"} {
		code, _ := postDashboardJSON(t, handler, removedEndpoint, map[string]any{})
		if code != http.StatusNotFound {
			t.Fatalf("POST %s = %d, want 404 (headless AI runner removed)", removedEndpoint, code)
		}
	}

	// Invalid MCP analysis decision must be rejected and remain pending_ai.
	if _, err := db.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: home,
		Fingerprint: "fp-invalid-decision",
		Category:    "type",
		Summary:     "[pyright] none access",
		Source:      "auto_verify",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SubmitMistakeAnalysisScoped(context.Background(), home, "fp-invalid-decision", "auto_fix_everything", "bogus", ""); err == nil {
		t.Fatalf("expected error on illegal decision value")
	}
	stillPending, _ := db.ListInboxMistakes(context.Background(), home, "pending_ai")
	if len(stillPending) != 1 || stillPending[0].Fingerprint != "fp-invalid-decision" {
		t.Fatalf("illegal decision must remain pending_ai, got %+v", stillPending)
	}
}

func TestP0SafeUninstallBackupPermissionsConflictAndTOML(t *testing.T) {
	home := t.TempDir()
	db := openDashboardStore(t)

	claudePath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{"other":{"command":"echo"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewServer(db, nil)
	s.homeDir = home
	handler := authedTestHandler(s)

	// 1. Preview gives base_hashes; modifying target before apply yields 409 Conflict.
	pCode, pBody := postDashboardJSON(t, handler, "/api/agents/preview", map[string]any{
		"agent_id": "claude_code",
		"action":   "install",
	})
	if pCode != http.StatusOK {
		t.Fatalf("preview claude_code = %d: %s", pCode, pBody)
	}
	pJSON := decodeDashboardJSON(t, pBody)
	baseHashes, _ := pJSON["base_hashes"].(map[string]any)
	if len(baseHashes) == 0 {
		t.Fatalf("expected non-empty base_hashes in preview: %v", pJSON)
	}
	previewID, _ := pJSON["preview_id"].(string)
	if previewID == "" {
		t.Fatalf("preview missing preview_id: %v", pJSON)
	}
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{"other":{"command":"echo"},"racing":{}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conflictCode, _ := postDashboardJSON(t, handler, "/api/agents/toggle", map[string]any{
		"agent_id":        "claude_code",
		"enable":          true,
		"preview_id":      previewID,
		"confirmed":       true,
		"expected_hashes": baseHashes,
	})
	if conflictCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when file changed after preview, got %d", conflictCode)
	}

	// 2. Re-preview and apply cleanly; check 0700 backup dir, 0600 backup file, 0600 target file.
	_, pBody2 := postDashboardJSON(t, handler, "/api/agents/preview", map[string]any{
		"agent_id": "claude_code",
		"action":   "install",
	})
	pJSON2 := decodeDashboardJSON(t, pBody2)
	previewID2, _ := pJSON2["preview_id"].(string)
	if previewID2 == "" {
		t.Fatalf("second preview missing preview_id: %v", pJSON2)
	}
	okCode, okBody := postDashboardJSON(t, handler, "/api/agents/toggle", map[string]any{
		"agent_id":        "claude_code",
		"enable":          true,
		"preview_id":      previewID2,
		"confirmed":       true,
		"expected_hashes": pJSON2["base_hashes"],
	})
	if okCode != http.StatusOK {
		t.Fatalf("apply claude_code = %d: %s", okCode, okBody)
	}
	dirInfo, err := os.Stat(s.backupsDir())
	if err != nil {
		t.Fatalf("stat backupsDir: %v", err)
	}
	bPath := backupRecordPath(s.backupsDir(), "mcp", "claude_code", claudePath)
	bInfo, err := os.Stat(bPath)
	if err != nil {
		t.Fatalf("stat backup file %s: %v", bPath, err)
	}
	tgtInfo, err := os.Stat(claudePath)
	if err != nil {
		t.Fatalf("stat target %s: %v", claudePath, err)
	}
	if runtime.GOOS != "windows" {
		if dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("backupsDir perm = %v, want 0700", dirInfo.Mode().Perm())
		}
		if bInfo.Mode().Perm() != 0o600 {
			t.Fatalf("backup file perm = %v, want 0600", bInfo.Mode().Perm())
		}
		if tgtInfo.Mode().Perm() != 0o600 {
			t.Fatalf("target ~/.claude.json perm = %v, want preserved 0600", tgtInfo.Mode().Perm())
		}
	}

	// 3. Another tool adds "postgres" AFTER SlopGuard install; disabling SlopGuard must NOT wipe "postgres"!
	rawClaude, _ := os.ReadFile(claudePath)
	var claudeObj map[string]any
	if err := json.Unmarshal(rawClaude, &claudeObj); err != nil {
		t.Fatal(err)
	}
	serversObj := claudeObj["mcpServers"].(map[string]any)
	serversObj["postgres"] = map[string]any{"command": "pg-mcp"}
	editedClaude, _ := json.MarshalIndent(claudeObj, "", "  ")
	if err := os.WriteFile(claudePath, append(editedClaude, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	offPreview := mustPreview(t, handler, "/api/agents/preview", map[string]any{
		"agent_id": "claude_code",
		"action":   "restore",
	})
	offCode, offBody := postDashboardJSON(t, handler, "/api/agents/toggle", map[string]any{
		"agent_id":        "claude_code",
		"enable":          false,
		"preview_id":      previewIDOf(t, offPreview),
		"confirmed":       true,
		"expected_hashes": offPreview["base_hashes"],
	})
	if offCode != http.StatusOK {
		t.Fatalf("disable claude_code = %d: %s", offCode, offBody)
	}
	afterDisable, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(afterDisable), `"postgres"`) || !strings.Contains(string(afterDisable), `"other"`) {
		t.Fatalf("disabling SlopGuard wiped later additions:\n%s", string(afterDisable))
	}
	if strings.Contains(string(afterDisable), `"slopguard"`) {
		t.Fatalf("expected slopguard removed cleanly:\n%s", string(afterDisable))
	}

	// 4. Corrupt backup must fail loudly instead of silently degrading.
	corruptPath := filepath.Join(home, ".cursor", "mcp.json")
	if err := writeFileAtomic(corruptPath, []byte(`{"mcpServers":{"slopguard":{"command":"echo"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	corruptBak := backupRecordPath(s.backupsDir(), "mcp", "cursor", corruptPath)
	if err := writeFileAtomic(corruptBak, []byte(`{not valid json`), 0o600); err != nil {
		t.Fatal(err)
	}
	cOffCode, cOffBody := postDashboardJSON(t, handler, "/api/agents/preview", map[string]any{
		"agent_id": "cursor",
		"action":   "restore",
	})
	if cOffCode == http.StatusOK {
		t.Fatalf("expected error on corrupt backup record, got 200: %s", cOffBody)
	}

	// 5. Invalid non-object mcpServers JSON type must be rejected rather than overwritten.
	if err := writeFileAtomic(corruptPath, []byte(`{"mcpServers":["bad"]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := renderMCPUpsert(corruptPath, "/bin/echo", []string{"mcp"}); err == nil {
		t.Fatalf("expected renderMCPUpsert to reject array mcpServers")
	}

	// 6. Codex TOML validation + multiline string & later section preservation.
	codexTOML := filepath.Join(home, ".codex", "config.toml")
	mlTOML := "developer_instructions = \"\"\"\n[mcp_servers.slopguard]\nnot a real header\n\"\"\"\n\n[mcp_servers.existing]\ncommand = \"echo\"\n"
	if err := writeFileAtomic(codexTOML, []byte(mlTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	_, updatedTOML, _, err := renderMCPUpsert(codexTOML, "/usr/local/bin/slopguard", []string{"mcp"})
	if err != nil {
		t.Fatalf("renderMCPUpsert multiline TOML: %v", err)
	}
	if !strings.Contains(updatedTOML, "not a real header") || !strings.Contains(updatedTOML, "[mcp_servers.slopguard]") {
		t.Fatalf("multiline TOML broken:\n%s", updatedTOML)
	}
	if err := validateTOMLBasic("[broken\n"); err == nil {
		t.Fatalf("expected validateTOMLBasic to catch unclosed header")
	}
}

func TestP1GitHookPathResolution(t *testing.T) {
	proj := t.TempDir()

	// Real git repo with custom core.hooksPath.
	customHooksDir := filepath.Join(proj, ".custom-hooks")
	if err := os.MkdirAll(customHooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "core.hooksPath", ".custom-hooks"},
	} {
		res := engine.RunProcess(context.Background(), engine.ProcessSpec{
			Command: "git",
			Args:    args,
			Dir:     proj,
			Timeout: 5 * time.Second,
		})
		if res.Err != nil || res.ExitCode != 0 {
			t.Skipf("git unavailable or failed in test env: err=%v code=%d", res.Err, res.ExitCode)
		}
	}

	hookPath, ok := resolveGitHookPath(proj)
	wantHook := filepath.Join(proj, ".custom-hooks", "post-commit")
	wantReal, _ := filepath.EvalSymlinks(filepath.Dir(wantHook))
	gotReal, _ := filepath.EvalSymlinks(filepath.Dir(hookPath))
	if !ok || (hookPath != wantHook && gotReal != wantReal) {
		t.Fatalf("resolveGitHookPath with core.hooksPath = %q (ok=%v), want %q", hookPath, ok, wantHook)
	}

	block := buildGitHookBlock("/usr/local/bin/slopguard")
	if !strings.Contains(block, `--commit "$_sg_rev"`) {
		t.Fatalf("git hook block must pin commit SHA via --commit, got:\n%s", block)
	}
}
