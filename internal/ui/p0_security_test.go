package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func p0FreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func p0WaitForIdentity(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/identity", port))
		if err == nil {
			status := resp.StatusCode
			_, _ = resp.Body.Read(make([]byte, 0))
			_ = resp.Body.Close()
			if status == http.StatusOK {
				return
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("dashboard on port %d did not become ready", port)
}

func TestP0AuthTicketLifecycleAndIdentity(t *testing.T) {
	if authTicketTTL != 60*time.Second {
		t.Fatalf("authTicketTTL = %v, want 60s", authTicketTTL)
	}
	s := NewServer(nil, nil)
	token := generateRandomToken()
	s.SetAuthToken(token)
	s.instanceID = "inst-p0"
	s.port = 43170

	handler := s.Handler()

	identityReq := httptest.NewRequest(http.MethodGet, "/api/identity", nil)
	identityReq.Host = "127.0.0.1:43170"
	identityRec := httptest.NewRecorder()
	handler.ServeHTTP(identityRec, identityReq)
	if identityRec.Code != http.StatusOK {
		t.Fatalf("identity status = %d", identityRec.Code)
	}
	identity := decodeDashboardJSON(t, identityRec.Body.Bytes())
	if identity["service"] != "slopguard" || identity["instance_id"] != "inst-p0" {
		t.Fatalf("identity payload = %v", identity)
	}
	if strings.Contains(identityRec.Body.String(), token) {
		t.Fatalf("identity response leaked long-term token")
	}

	ticketPost := func(header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/ticket", nil)
		req.Host = "127.0.0.1:43170"
		if header != "" {
			req.Header.Set(uiTokenHeaderName, header)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := ticketPost(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ticket without token = %d, want 401", rec.Code)
	}
	if rec := ticketPost("wrong-token"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ticket with wrong token = %d, want 401", rec.Code)
	}
	ticketRec := ticketPost(token)
	if ticketRec.Code != http.StatusOK {
		t.Fatalf("ticket with token = %d: %s", ticketRec.Code, ticketRec.Body.String())
	}
	if strings.Contains(ticketRec.Body.String(), token) {
		t.Fatalf("ticket response leaked long-term token: %s", ticketRec.Body.String())
	}
	var ticketPayload struct {
		Ticket string `json:"ticket"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(ticketRec.Body.Bytes(), &ticketPayload); err != nil || ticketPayload.Ticket == "" {
		t.Fatalf("ticket payload = %s err=%v", ticketRec.Body.String(), err)
	}
	if !strings.Contains(ticketPayload.URL, "?ticket=") || strings.Contains(ticketPayload.URL, "token=") {
		t.Fatalf("ticket URL = %q, want one-time ticket URL", ticketPayload.URL)
	}

	bootReq := httptest.NewRequest(http.MethodGet, "/?ticket="+ticketPayload.Ticket, nil)
	bootReq.Host = "127.0.0.1:43170"
	bootRec := httptest.NewRecorder()
	handler.ServeHTTP(bootRec, bootReq)
	if bootRec.Code != http.StatusFound {
		t.Fatalf("ticket boot = %d, want 302", bootRec.Code)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range bootRec.Result().Cookies() {
		if cookie.Name == uiSessionCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("ticket cookie = %+v", sessionCookie)
	}

	replayReq := httptest.NewRequest(http.MethodGet, "/?ticket="+ticketPayload.Ticket, nil)
	replayReq.Host = "127.0.0.1:43170"
	replayRec := httptest.NewRecorder()
	handler.ServeHTTP(replayRec, replayReq)
	if replayRec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed ticket = %d, want 401", replayRec.Code)
	}

	expired, _, err := s.IssueAuthTicket(time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	expiredReq := httptest.NewRequest(http.MethodGet, "/?ticket="+expired, nil)
	expiredReq.Host = "127.0.0.1:43170"
	expiredRec := httptest.NewRecorder()
	handler.ServeHTTP(expiredRec, expiredReq)
	if expiredRec.Code != http.StatusUnauthorized {
		t.Fatalf("expired ticket = %d, want 401", expiredRec.Code)
	}
}

func TestP0LongTermTokenIsStable(t *testing.T) {
	home := t.TempDir()
	first, err := loadOrCreateAuthToken(home)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateAuthToken(home)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("long-term token rotated: %q != %q", first, second)
	}
	if len(first) != 64 {
		t.Fatalf("token length = %d, want 64", len(first))
	}
	raw, err := os.ReadFile(filepath.Join(home, ".slopguard", "ui-token"))
	if err != nil || strings.TrimSpace(string(raw)) != first {
		t.Fatalf("ui-token file = %q err=%v", raw, err)
	}
}

func TestP0SingleInstanceAndTokenSafety(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	db := openDashboardStore(t)
	firstPort := p0FreePort(t)
	secondPort := p0FreePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, db, nil, Options{Port: firstPort, NoOpen: true}, &output)
	}()
	p0WaitForIdentity(t, firstPort)

	tokenPath := filepath.Join(home, ".slopguard", "ui-token")
	tokenRaw, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	token := strings.TrimSpace(string(tokenRaw))
	if len(token) != 64 {
		t.Fatalf("token length = %d", len(token))
	}
	if strings.Contains(output.String(), token) {
		t.Fatalf("dashboard output leaked token: %q", output.String())
	}

	var secondOutput bytes.Buffer
	secondErr := Run(context.Background(), db, nil, Options{Port: secondPort, NoOpen: true}, &secondOutput)
	var already *AlreadyRunningError
	if !errors.As(secondErr, &already) {
		t.Fatalf("second Run error = %v, want AlreadyRunningError", secondErr)
	}
	after, err := os.ReadFile(tokenPath)
	if err != nil || string(after) != string(tokenRaw) {
		t.Fatalf("second Run changed ui-token: %q err=%v", after, err)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("first Run exit: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("first Run did not stop")
	}
	if _, err := os.Stat(instanceLockPath(home)); !os.IsNotExist(err) {
		t.Fatalf("ui.lock not cleaned up: %v", err)
	}
}

func TestP0StartupFailureDoesNotRotateToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	stateDir := filepath.Join(home, ".slopguard")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(stateDir, "ui-token")
	token := strings.Repeat("a", 64) + "\n"
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port
	db := openDashboardStore(t)
	var output bytes.Buffer
	if runErr := Run(context.Background(), db, nil, Options{Port: port, NoOpen: true}, &output); runErr == nil {
		t.Fatal("expected listen failure")
	}
	after, err := os.ReadFile(tokenPath)
	if err != nil || string(after) != token {
		t.Fatalf("failed startup changed ui-token: %q err=%v", after, err)
	}
	if _, err := os.Stat(instanceLockPath(home)); !os.IsNotExist(err) {
		t.Fatalf("failed startup left ui.lock: %v", err)
	}
}

func TestP0StaleLockRecovery(t *testing.T) {
	home := t.TempDir()
	lockPath := instanceLockPath(home)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := instanceLockRecord{
		Version:    1,
		PID:        99999999,
		Port:       p0FreePort(t),
		InstanceID: "stale-instance",
		StartedAt:  time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireInstanceLock(home, p0FreePort(t))
	if err != nil {
		t.Fatalf("stale lock not recovered: %v", err)
	}
	if lock.record.InstanceID == stale.InstanceID {
		t.Fatal("acquired the stale lock record")
	}
	lock.release()
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock release failed: %v", err)
	}
}

func TestP0LegacyBackupFailClosedAndSafeMigration(t *testing.T) {
	backupsDir := filepath.Join(t.TempDir(), "backups")
	target := filepath.Join(t.TempDir(), "claude.json")
	original := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"}}}\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, rec, err := ensureBackup(backupsDir, "mcp", "legacy", target)
	if err != nil {
		t.Fatal(err)
	}
	legacy := rec
	legacy.SchemaVersion = 0
	legacy.InstalledSHA256 = ""
	legacy.InstalledBytes = nil
	legacy.InstalledMode = 0
	legacy.InstalledAt = ""
	recordPath := backupRecordPath(backupsDir, "mcp", "legacy", target)
	encoded, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.WriteFile(recordPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("{\"changed\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(target)
	plan, err := planManagedUninstall(backupsDir, backupTarget{Kind: "mcp", ID: "legacy", Path: target}, legacy)
	if !errors.Is(err, ErrLegacyBackupNeedsReview) {
		t.Fatalf("legacy backup err = %v, want ErrLegacyBackupNeedsReview (plan=%+v)", err, plan)
	}
	after, _ := os.ReadFile(target)
	if string(before) != string(after) {
		t.Fatalf("legacy fail-closed changed target: %q -> %q", before, after)
	}

	// A provable legacy MCP snapshot can be upgraded to v2.
	migrateTarget := filepath.Join(t.TempDir(), "claude.json")
	migrateOriginal := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"}}}\n")
	if err := os.WriteFile(migrateTarget, migrateOriginal, 0o600); err != nil {
		t.Fatal(err)
	}
	_, migrateRec, err := ensureBackup(backupsDir, "mcp", "migrate", migrateTarget)
	if err != nil {
		t.Fatal(err)
	}
	installed := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"},\"slopguard\":{\"command\":\"slopguard\",\"args\":[\"mcp\"]}}}\n")
	if err := os.WriteFile(migrateTarget, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	legacyMigrate := migrateRec
	legacyMigrate.SchemaVersion = 0
	legacyMigrate.InstalledSHA256 = ""
	legacyMigrate.InstalledBytes = nil
	legacyMigrate.InstalledMode = 0
	legacyMigrate.InstalledAt = ""
	upgraded, err := migrateLegacyBackup(backupTarget{Kind: "mcp", ID: "migrate", Path: migrateTarget}, legacyMigrate)
	if err != nil {
		t.Fatalf("provable legacy backup migration: %v", err)
	}
	if upgraded.InstalledSHA256 != fileSHA256Hex(installed) || string(upgraded.InstalledBytes) != string(installed) {
		t.Fatalf("upgraded record = %+v", upgraded)
	}
	plan, err = planManagedUninstall(backupsDir, backupTarget{Kind: "mcp", ID: "migrate", Path: migrateTarget}, upgraded)
	if err != nil {
		t.Fatalf("plan upgraded uninstall: %v", err)
	}
	if err := applyManagedUninstall(backupsDir, backupTarget{Kind: "mcp", ID: "migrate", Path: migrateTarget}, plan); err != nil {
		t.Fatalf("apply upgraded uninstall: %v", err)
	}
	if got, _ := os.ReadFile(migrateTarget); string(got) != string(migrateOriginal) {
		t.Fatalf("upgraded restore = %q, want %q", got, migrateOriginal)
	}

	// Legacy skill without a provable installed snapshot must fail closed.
	skillTarget := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skillTarget, []byte("# user skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, skillRec, err := ensureBackup(backupsDir, "skill", "skill-legacy", skillTarget)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillTarget, []byte("# user skill\n\nchanged after backup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillRec.SchemaVersion = 0
	skillRec.InstalledSHA256 = ""
	skillRec.InstalledBytes = nil
	if _, err := planManagedUninstall(backupsDir, backupTarget{Kind: "skill", ID: "skill-legacy", Path: skillTarget}, skillRec); !errors.Is(err, ErrLegacyBackupNeedsReview) {
		t.Fatalf("legacy skill err = %v, want ErrLegacyBackupNeedsReview", err)
	}
}

func TestP0ThreeWayUninstallMCPAndSkill(t *testing.T) {
	backupsDir := filepath.Join(t.TempDir(), "backups")
	original := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"}}}\n")
	installed := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"},\"slopguard\":{\"command\":\"slopguard\",\"args\":[\"mcp\"]}}}\n")

	target := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureBackup(backupsDir, "mcp", "claude", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recordBackupInstalled(backupsDir, backupTarget{Kind: "mcp", ID: "claude", Path: target}, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	withPostgres := []byte("{\"mcpServers\":{\"other\":{\"command\":\"echo\"},\"slopguard\":{\"command\":\"slopguard\",\"args\":[\"mcp\"]},\"postgres\":{\"command\":\"pg-mcp\"}}}\n")
	if err := os.WriteFile(target, withPostgres, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := planManagedUninstall(backupsDir, backupTarget{Kind: "mcp", ID: "claude", Path: target}, mustBackupRecord(t, backupsDir, "mcp", "claude", target))
	if err != nil {
		t.Fatalf("plan mcp merge: %v", err)
	}
	if err := applyManagedUninstall(backupsDir, backupTarget{Kind: "mcp", ID: "claude", Path: target}, plan); err != nil {
		t.Fatalf("apply mcp merge: %v", err)
	}
	merged, _ := os.ReadFile(target)
	if !strings.Contains(string(merged), `"postgres"`) || strings.Contains(string(merged), `"slopguard"`) {
		t.Fatalf("mcp merge = %s", merged)
	}

	conflictTarget := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(conflictTarget, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureBackup(backupsDir, "mcp", "conflict", conflictTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflictTarget, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recordBackupInstalled(backupsDir, backupTarget{Kind: "mcp", ID: "conflict", Path: conflictTarget}, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	userEdited := []byte("{\"mcpServers\":{\"slopguard\":{\"command\":\"user-changed\",\"args\":[\"mcp\"]}}}\n")
	if err := os.WriteFile(conflictTarget, userEdited, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := planManagedUninstall(backupsDir, backupTarget{Kind: "mcp", ID: "conflict", Path: conflictTarget}, mustBackupRecord(t, backupsDir, "mcp", "conflict", conflictTarget)); !errors.Is(err, ErrManagedConfigModified) {
		t.Fatalf("user-modified mcp entry err = %v, want ErrManagedConfigModified", err)
	}
	if got, _ := os.ReadFile(conflictTarget); string(got) != string(userEdited) {
		t.Fatalf("user-modified mcp file changed: %q", got)
	}

	skillTarget := filepath.Join(t.TempDir(), "SKILL.md")
	if _, _, err := ensureBackup(backupsDir, "skill", "skill", skillTarget); err != nil {
		t.Fatal(err)
	}
	skillInstalled := []byte(wrapSkillManagedBlock("---\nname: slopguard\n---\n\n# skill\n"))
	if err := os.WriteFile(skillTarget, skillInstalled, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recordBackupInstalled(backupsDir, backupTarget{Kind: "skill", ID: "skill", Path: skillTarget}, skillInstalled, 0o600); err != nil {
		t.Fatal(err)
	}
	skillWithUser := append(append([]byte(nil), skillInstalled...), []byte("\nuser note\n")...)
	if err := os.WriteFile(skillTarget, skillWithUser, 0o600); err != nil {
		t.Fatal(err)
	}
	skillPlan, err := planManagedUninstall(backupsDir, backupTarget{Kind: "skill", ID: "skill", Path: skillTarget}, mustBackupRecord(t, backupsDir, "skill", "skill", skillTarget))
	if err != nil {
		t.Fatalf("plan skill merge: %v", err)
	}
	if err := applyManagedUninstall(backupsDir, backupTarget{Kind: "skill", ID: "skill", Path: skillTarget}, skillPlan); err != nil {
		t.Fatalf("apply skill merge: %v", err)
	}
	skillMerged, _ := os.ReadFile(skillTarget)
	if !strings.Contains(string(skillMerged), "user note") || strings.Contains(string(skillMerged), skillManagedStartMarker) {
		t.Fatalf("skill merge = %s", skillMerged)
	}
}

func mustBackupRecord(t *testing.T, backupsDir, kind, id, target string) configBackupRecord {
	t.Helper()
	_, rec, ok, err := lookupBackupChecked(backupsDir, kind, id, target)
	if err != nil || !ok {
		t.Fatalf("lookup backup %s/%s: ok=%v err=%v", kind, id, ok, err)
	}
	return rec
}

func TestP0TOMLUninstallPreservesOtherSections(t *testing.T) {
	backupsDir := filepath.Join(t.TempDir(), "backups")
	target := filepath.Join(t.TempDir(), "config.toml")
	original := "model = \"gpt-5\"\n\n[mcp_servers.existing]\ncommand = \"echo\"\nargs = [\"mcp\"]\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureBackup(backupsDir, "mcp", "codex", target); err != nil {
		t.Fatal(err)
	}
	installed := "model = \"gpt-5\"\n\n[mcp_servers.existing]\ncommand = \"echo\"\nargs = [\"mcp\"]\n\n[mcp_servers.slopguard]\ncommand = \"slopguard\"\nargs = [\"mcp\"]\n"
	if err := os.WriteFile(target, []byte(installed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recordBackupInstalled(backupsDir, backupTarget{Kind: "mcp", ID: "codex", Path: target}, []byte(installed), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := planManagedUninstall(backupsDir, backupTarget{Kind: "mcp", ID: "codex", Path: target}, mustBackupRecord(t, backupsDir, "mcp", "codex", target))
	if err != nil {
		t.Fatalf("plan TOML uninstall: %v", err)
	}
	if err := applyManagedUninstall(backupsDir, backupTarget{Kind: "mcp", ID: "codex", Path: target}, plan); err != nil {
		t.Fatalf("apply TOML uninstall: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != original {
		t.Fatalf("TOML restore = %q err=%v, want %q", got, err, original)
	}
}

func TestP0PreviewBindingLifecycle(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config.json")
	original := []byte("{\"a\":1}\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, nil)
	baseHash := fileSHA256Hex(original)
	newHash := fileSHA256Hex([]byte("{\"a\":2}\n"))
	expect := previewExpectation{
		Kind: "mcp", TargetID: "target", Project: t.TempDir(), Scope: "global", Action: "install",
		Paths: []string{target},
	}
	newRecord := func() previewRecord {
		return previewRecord{
			Kind: "mcp", TargetID: "target", Project: expect.Project, Scope: "global", Action: "install",
			Paths:     map[string]string{target: baseHash},
			NewHashes: map[string]string{target: newHash},
		}
	}

	if _, err := s.beginApply("", expect); !errors.Is(err, ErrPreviewRequired) {
		t.Fatalf("missing preview err = %v, want ErrPreviewRequired", err)
	}

	rec, err := s.createPreview(newRecord())
	if err != nil {
		t.Fatal(err)
	}
	mismatch := expect
	mismatch.TargetID = "other"
	if _, err := s.beginApply(rec.ID, mismatch); !errors.Is(err, ErrPreviewScopeMismatch) {
		t.Fatalf("scope mismatch err = %v, want ErrPreviewScopeMismatch", err)
	}
	mismatchProject := expect
	mismatchProject.Project = filepath.Join(t.TempDir(), "other")
	if _, err := s.beginApply(rec.ID, mismatchProject); !errors.Is(err, ErrPreviewScopeMismatch) {
		t.Fatalf("project mismatch err = %v, want ErrPreviewScopeMismatch", err)
	}
	mismatchAction := expect
	mismatchAction.Action = "restore"
	if _, err := s.beginApply(rec.ID, mismatchAction); !errors.Is(err, ErrPreviewScopeMismatch) {
		t.Fatalf("action mismatch err = %v, want ErrPreviewScopeMismatch", err)
	}
	lease, err := s.beginApply(rec.ID, expect)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := lease.checkNewContent(target, []byte("{\"a\":2}\n")); err != nil {
		t.Fatalf("new content check: %v", err)
	}
	lease.Close()
	if _, err := s.beginApply(rec.ID, expect); !errors.Is(err, ErrPreviewUsed) {
		t.Fatalf("replay err = %v, want ErrPreviewUsed", err)
	}

	conflictRec, err := s.createPreview(newRecord())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("{\"external\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.beginApply(conflictRec.ID, expect); !errors.Is(err, ErrPreviewConflict) {
		t.Fatalf("conflict err = %v, want ErrPreviewConflict", err)
	}
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}

	expiredRec, err := s.createPreview(newRecord())
	if err != nil {
		t.Fatal(err)
	}
	s.previewMu.Lock()
	s.previews[expiredRec.ID].ExpiresAt = time.Now().Add(-time.Second)
	s.previewMu.Unlock()
	if _, err := s.beginApply(expiredRec.ID, expect); !errors.Is(err, ErrPreviewExpired) {
		t.Fatalf("expired err = %v, want ErrPreviewExpired", err)
	}

	contentRec, err := s.createPreview(newRecord())
	if err != nil {
		t.Fatal(err)
	}
	contentLease, err := s.beginApply(contentRec.ID, expect)
	if err != nil {
		t.Fatalf("content apply: %v", err)
	}
	if err := contentLease.checkNewContent(target, []byte("{\"wrong\":true}\n")); !errors.Is(err, ErrPreviewContentChanged) {
		t.Fatalf("content changed err = %v, want ErrPreviewContentChanged", err)
	}
	contentLease.Close()

	concurrentRec, err := s.createPreview(newRecord())
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := s.beginApply(concurrentRec.ID, expect)
			if err == nil && lease != nil {
				lease.Close()
			}
			results <- result{err: err}
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	used := 0
	for res := range results {
		if res.err == nil {
			success++
		} else if errors.Is(res.err, ErrPreviewUsed) {
			used++
		} else {
			t.Fatalf("concurrent err = %v", res.err)
		}
	}
	if success != 1 || used != 1 {
		t.Fatalf("concurrent results success=%d used=%d, want 1/1", success, used)
	}
}

func TestP0ConcurrentApplySerializedByPreview(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	s := NewServer(nil, nil)
	s.homeDir = home
	handler := authedTestHandler(s)

	preview := mustPreview(t, handler, "/api/mcp-clients/preview", map[string]any{
		"client_id": "claude_code",
		"project":   project,
		"action":    "install",
	})
	payload := map[string]any{
		"client_id":       "claude_code",
		"project":         project,
		"preview_id":      previewIDOf(t, preview),
		"confirmed":       true,
		"expected_hashes": preview["base_hashes"],
	}
	type result struct {
		code int
		body []byte
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := postDashboardJSON(t, handler, "/api/mcp-clients/install", payload)
			results <- result{code: code, body: body}
		}()
	}
	wg.Wait()
	close(results)
	okCount, conflictCount := 0, 0
	for res := range results {
		switch res.code {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("concurrent apply = %d: %s", res.code, res.body)
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Fatalf("concurrent apply codes ok=%d conflict=%d, want 1/1", okCount, conflictCount)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil || !isSlopGuardConfigured(filepath.Join(home, ".claude.json")) {
		t.Fatalf("concurrent apply config = %q err=%v", raw, err)
	}
	if _, err := os.Stat(backupRecordPath(s.backupsDir(), "mcp", "claude_code", filepath.Join(home, ".claude.json"))); err != nil {
		t.Fatalf("concurrent apply backup missing: %v", err)
	}
}

func TestP0MutationEndpointsRequirePreview(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(project, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, nil)
	s.homeDir = home
	handler := authedTestHandler(s)

	cases := []struct {
		name string
		path string
		body map[string]any
	}{
		{"mcp install", "/api/mcp-clients/install", map[string]any{"client_id": "claude_code", "confirmed": true}},
		{"skill install", "/api/skills/install", map[string]any{"target_id": "project_claude_skill", "project": project, "confirmed": true}},
		{"agent toggle", "/api/agents/toggle", map[string]any{"agent_id": "antigravity", "enable": true, "confirmed": true}},
		{"shared skill", "/api/shared-skill", map[string]any{"content": "# x\n", "confirmed": true}},
		{"guard hook", "/api/guard/toggle", map[string]any{"project": project, "hook_enabled": true, "confirmed": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postDashboardJSON(t, handler, tc.path, tc.body)
			if code != http.StatusConflict {
				t.Fatalf("%s = %d, want 409: %s", tc.name, code, body)
			}
		})
	}
}
