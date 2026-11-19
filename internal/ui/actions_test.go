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
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/store"
)

func postDashboardJSON(t *testing.T, handler http.Handler, path string, payload any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
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

	b1, _, err := ensureBackup(backupsDir, "mcp", "claude", existingPath)
	if err != nil {
		t.Fatalf("first ensureBackup: %v", err)
	}
	if err := os.WriteFile(existingPath, []byte("{\"existing\": false}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Second ensureBackup must preserve original backup contents, not overwrite it.
	b2, rec, err := ensureBackup(backupsDir, "mcp", "claude", existingPath)
	if err != nil {
		t.Fatalf("second ensureBackup: %v", err)
	}
	if b1 != b2 || string(rec.OriginalBytes) != string(original) {
		t.Fatalf("backup overwritten: b1=%s b2=%s rec=%q", b1, b2, rec.OriginalBytes)
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
	if err := writeFileAtomic(newSkillPath, []byte("# skill\n"), 0o644); err != nil {
		t.Fatal(err)
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
	handler := s.Handler()

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
	afterPreview, _ := os.ReadFile(globalClaudePath)
	if string(afterPreview) != originalClaude {
		t.Fatalf("preview mutated disk: %s", afterPreview)
	}

	// 3. Global install without confirmed=true must fail with 400.
	code, errBody := postDashboardJSON(t, handler, "/api/mcp-clients/install", map[string]any{
		"client_id": "claude_code",
		"project":   project,
		"confirmed": false,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("unconfirmed global install = %d, want 400: %s", code, errBody)
	}

	// 4. Global install with confirmed=true writes slopguard while preserving other servers + backup.
	code, instBody := postDashboardJSON(t, handler, "/api/mcp-clients/install", map[string]any{
		"client_id": "claude_code",
		"project":   project,
		"confirmed": true,
	})
	if code != http.StatusOK {
		t.Fatalf("confirmed global install = %d: %s", code, instBody)
	}
	updatedClaude, err := os.ReadFile(globalClaudePath)
	if err != nil || !strings.Contains(string(updatedClaude), `"slopguard"`) || !strings.Contains(string(updatedClaude), `"other"`) {
		t.Fatalf("updated claude.json = %q (err=%v)", updatedClaude, err)
	}

	// 5. Restore should revert exact original contents.
	code, restBody := postDashboardJSON(t, handler, "/api/mcp-clients/restore", map[string]any{
		"client_id": "claude_code",
		"project":   project,
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
	handler := s.Handler()

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
	if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
		t.Fatalf("preview created file unexpectedly")
	}

	// Install project skill.
	code, instBody := postDashboardJSON(t, handler, "/api/skills/install", map[string]any{
		"target_id": "project_claude_skill",
		"project":   project,
		"confirmed": true,
	})
	if code != http.StatusOK {
		t.Fatalf("skill install = %d: %s", code, instBody)
	}
	if !isSkillConfigured(skillPath) {
		t.Fatalf("expected installed skill at %s", skillPath)
	}

	// Restore removes newly created skill file cleanly.
	code, restBody := postDashboardJSON(t, handler, "/api/skills/restore", map[string]any{
		"target_id": "project_claude_skill",
		"project":   project,
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
	handler := s.Handler()

	// 1. Enable guard + hook and verify existing post-commit content is preserved.
	code, body := postDashboardJSON(t, handler, "/api/guard/toggle", map[string]any{
		"project":      project,
		"enabled":      true,
		"hook_enabled": true,
		"debounce_sec": 1,
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
	code, body = postDashboardJSON(t, handler, "/api/guard/toggle", map[string]any{
		"project":      project,
		"hook_enabled": false,
	})
	if code != http.StatusOK {
		t.Fatalf("guard toggle hook off = %d: %s", code, body)
	}
	restoredHook, _ := os.ReadFile(hookPath)
	if string(restoredHook) != originalHook {
		t.Fatalf("restored hook = %q, want %q", restoredHook, originalHook)
	}
}

func TestSessionScannerReadOnlyAndGitFix(t *testing.T) {
	home := t.TempDir()
	db := openDashboardStore(t)
	project := filepath.Join(t.TempDir(), "web-app")
	writeDashboardFile(t, filepath.Join(project, ".git", "HEAD"), "ref: refs/heads/main\n")

	claudeLog := filepath.Join(home, ".claude", "projects", "web-app", "sess1.jsonl")
	claudeContent := `{"cwd":` + jsonQuote(project) + `,"timestamp":"2026-11-18T10:00:00Z","message":{"content":[{"type":"tool_result","is_error":true,"content":"src/index.ts(12,5): error TS2322: Type 'string' is not assignable to type 'number'."}]}}` + "\n"
	writeDashboardFile(t, claudeLog, claudeContent)

	codexLog := filepath.Join(home, ".codex", "sessions", "2026", "11", "18", "rollout-1.jsonl")
	codexContent := `{"type":"session_meta","timestamp":"2026-11-18T10:05:00Z","payload":{"cwd":` + jsonQuote(project) + `}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-11-18T10:06:00Z","payload":{"type":"custom_tool_call_output","output":"Command exited with code 1\ninternal/app.go:44:12: undefined: buildClient"}}` + "\n"
	writeDashboardFile(t, codexLog, codexContent)

	agyLog := filepath.Join(home, ".gemini", "antigravity", "brain", "conv-1", ".system_generated", "logs", "transcript.jsonl")
	agyContent := `{"status":"ERROR","created_at":"2026-11-18T10:09:00Z","content":"Encountered error in step execution: file does not exist\nthe error messages you got name them"}` + "\n" +
		`{"status":"ERROR","created_at":"2026-11-18T10:10:00Z","content":"Running pytest in ` + strings.ReplaceAll(project, `\`, `\\`) + `\nModuleNotFoundError: No module named 'httpx'"}` + "\n"
	writeDashboardFile(t, agyLog, agyContent)

	claudeStatBefore, _ := os.Stat(claudeLog)

	runner := &fakeUIRunner{
		runFunc: func(_ context.Context, spec contract.RunSpec) contract.RunResult {
			joined := strings.Join(spec.Args, " ")
			switch {
			case strings.Contains(joined, "rev-parse"):
				return contract.RunResult{ExitCode: 0, Stdout: "true\n"}
			case strings.Contains(joined, "log"):
				return contract.RunResult{
					ExitCode: 0,
					Stdout:   "1234567890abcdef1234567890abcdef12345678\t2026-11-18T09:00:00Z\tfix: resolve null token crash\nsrc/auth.ts\n",
				}
			default:
				return contract.RunResult{ExitCode: 0}
			}
		},
	}

	s := NewServer(db, runner)
	s.homeDir = home
	handler := s.Handler()

	code, body := postDashboardJSON(t, handler, "/api/sessions/scan", map[string]any{
		"project": project,
		"days":    14,
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/sessions/scan = %d: %s", code, body)
	}
	scanJSON := decodeDashboardJSON(t, body)
	if scanJSON["found"] != float64(4) || scanJSON["imported"] != float64(4) {
		t.Fatalf("scan response = %v, want found=4 imported=4", scanJSON)
	}

	// Verify read-only guarantee: original log files untouched.
	claudeAfter, _ := os.ReadFile(claudeLog)
	claudeStatAfter, _ := os.Stat(claudeLog)
	if string(claudeAfter) != claudeContent || !claudeStatAfter.ModTime().Equal(claudeStatBefore.ModTime()) {
		t.Fatalf("session file was modified by read-only scanner")
	}

	records, err := db.QueryMistakes(context.Background(), store.MistakeQuery{ProjectPath: project})
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]bool{}
	for _, r := range records {
		sources[r.Source] = true
		if r.ReviewStatus != "pending_ai" {
			t.Fatalf("expected pending_ai review_status, got %q", r.ReviewStatus)
		}
	}
	for _, wantSrc := range []string{"git_fix", "session_claude", "session_codex", "session_antigravity"} {
		if !sources[wantSrc] {
			t.Fatalf("missing source %q in records: %+v", wantSrc, records)
		}
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
	handler := s.Handler()

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
