package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	"github.com/HunterLzap/slopguard/internal/store"
)

func openDashboardStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func writeDashboardFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func authedTestHandler(s *Server) http.Handler {
	base := s.Handler()
	tok := s.AuthToken()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "127.0.0.1"
		r.Header.Set(uiTokenHeaderName, tok)
		base.ServeHTTP(w, r)
	})
}

func dashboardRequest(t *testing.T, handler http.Handler, method, path string) (int, []byte, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Host = "127.0.0.1"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder.Code, recorder.Body.Bytes(), recorder.Header()
}

func decodeDashboardJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, body)
	}
	return value
}

func TestDashboardOverviewMistakesAndRulesAPI(t *testing.T) {
	db := openDashboardStore(t)
	ctx := context.Background()
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordScan(ctx, project, 100, 2, true); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordScan(ctx, project, 200, 4, false); err != nil {
		t.Fatal(err)
	}
	mistake := contract.Mistake{
		ProjectPath: project,
		Fingerprint: "fp-dashboard",
		Category:    "type",
		Summary:     "avoid any",
		BadSnippet:  "let x: any",
		GoodSnippet: "let x: unknown",
	}
	if _, err := db.RecordMistake(ctx, mistake); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordMistake(ctx, mistake); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertRule(ctx, store.Rule{
		ID: "rule-dashboard", Scope: "project", ProjectPath: project,
		Engine: "eslint", RuleID: "no-console", Payload: `"error"`,
		Rationale: "dashboard test", Status: "active", HitCount: 3,
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(db, nil)
	handler := authedTestHandler(server)

	status, body, header := dashboardRequest(t, handler, http.MethodGet, "/api/overview?project="+url.QueryEscape(project))
	if status != http.StatusOK {
		t.Fatalf("overview status = %d, body = %s", status, body)
	}
	if !strings.Contains(header.Get("Content-Type"), "application/json") {
		t.Fatalf("overview content-type = %q", header.Get("Content-Type"))
	}
	overviewJSON := decodeDashboardJSON(t, body)
	if overviewJSON["total_runs"] != float64(2) || overviewJSON["failed_runs"] != float64(1) {
		t.Fatalf("overview = %v, want total_runs=2 failed_runs=1", overviewJSON)
	}
	if overviewJSON["avg_duration_ms"] != float64(150) || overviewJSON["rule_count"] != float64(1) {
		t.Fatalf("overview = %v, want avg=150 rule_count=1", overviewJSON)
	}
	if trend, ok := overviewJSON["trend"].([]any); !ok || len(trend) == 0 {
		t.Fatalf("overview trend = %v", overviewJSON["trend"])
	}

	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/mistakes?project="+url.QueryEscape(project))
	if status != http.StatusOK {
		t.Fatalf("mistakes status = %d, body = %s", status, body)
	}
	mistakesJSON := decodeDashboardJSON(t, body)
	records, _ := mistakesJSON["mistakes"].([]any)
	if len(records) != 1 {
		t.Fatalf("mistakes = %v, want one record", mistakesJSON)
	}
	record, _ := records[0].(map[string]any)
	if record["count"] != float64(2) || record["bad_snippet"] != "let x: any" || record["good_snippet"] != "let x: unknown" {
		t.Fatalf("mistake record = %v", record)
	}

	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/rules?project="+url.QueryEscape(project))
	if status != http.StatusOK {
		t.Fatalf("rules status = %d, body = %s", status, body)
	}
	rulesJSON := decodeDashboardJSON(t, body)
	rules, _ := rulesJSON["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("rules = %v, want one rule", rulesJSON)
	}
	rule, _ := rules[0].(map[string]any)
	if rule["rule_id"] != "no-console" || rule["rationale"] != "dashboard test" || rule["hit_count"] != float64(3) {
		t.Fatalf("rule = %v", rule)
	}

	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/projects")
	if status != http.StatusOK {
		t.Fatalf("projects status = %d, body = %s", status, body)
	}
	projectsJSON := decodeDashboardJSON(t, body)
	projects, _ := projectsJSON["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("projects = %v, want one", projectsJSON)
	}
	projectInfo, _ := projects[0].(map[string]any)
	if projectInfo["project_path"] != project {
		t.Fatalf("project info = %v", projectInfo)
	}
}

func TestDashboardMistakeIgnoreStatusFlow(t *testing.T) {
	db := openDashboardStore(t)
	ctx := context.Background()
	project := t.TempDir()
	mistake := contract.Mistake{
		ProjectPath: project,
		Fingerprint: "fp-ignore",
		Category:    "syntax",
		Summary:     "ignore me",
	}
	for i := 0; i < 2; i++ {
		if _, err := db.RecordMistake(ctx, mistake); err != nil {
			t.Fatal(err)
		}
	}
	handler := authedTestHandler(NewServer(db, nil))

	status, body, _ := dashboardRequest(t, handler, http.MethodGet, "/api/mistakes?status=pending")
	if status != http.StatusOK {
		t.Fatalf("pending status = %d: %s", status, body)
	}
	pending := decodeDashboardJSON(t, body)
	if records, _ := pending["mistakes"].([]any); len(records) != 1 {
		t.Fatalf("pending mistakes = %v", pending)
	}

	status, body, _ = dashboardRequest(t, handler, http.MethodPost, "/api/mistakes/fp-ignore/ignore")
	if status != http.StatusOK {
		t.Fatalf("ignore status = %d: %s", status, body)
	}
	ignored := decodeDashboardJSON(t, body)
	if ignored["ok"] != true || ignored["status"] != "ignored" {
		t.Fatalf("ignore response = %v", ignored)
	}
	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/mistakes?status=ignored")
	if status != http.StatusOK {
		t.Fatalf("ignored list status = %d: %s", status, body)
	}
	ignoredList := decodeDashboardJSON(t, body)
	if records, _ := ignoredList["mistakes"].([]any); len(records) != 1 {
		t.Fatalf("ignored mistakes = %v", ignoredList)
	}
	status, _, _ = dashboardRequest(t, handler, http.MethodGet, "/api/mistakes?status=pending")
	if status != http.StatusOK {
		t.Fatalf("pending after ignore status = %d", status)
	}
	status, body, _ = dashboardRequest(t, handler, http.MethodPost, "/api/mistakes/missing/ignore")
	if status != http.StatusNotFound {
		t.Fatalf("missing ignore status = %d, want 404: %s", status, body)
	}
	status, _, _ = dashboardRequest(t, handler, http.MethodGet, "/api/mistakes/fp-ignore/ignore")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("GET ignore status = %d, want 405", status)
	}
}

func TestDashboardRadarAndScreenshotAPI(t *testing.T) {
	db := openDashboardStore(t)
	project := t.TempDir()
	writeDashboardFile(t, filepath.Join(project, "package.json"), `{"name":"dashboard-project","version":"1.0.0"}`)
	writeDashboardFile(t, filepath.Join(project, "src", "app.ts"), "// @ts-ignore\nexport const value: number = 1;\n")
	writeDashboardFile(t, filepath.Join(project, ".slopguard", "baseline.json"),
		`{"version":1,"updated_at":"2026-01-01T00:00:00Z","metrics":{"ts_ignore":0}}`)
	writeDashboardFile(t, filepath.Join(project, ".slopguard", "ui.yaml"),
		"base_url: http://127.0.0.1:3000\nroutes: [\"/\"]\nscreenshot_dir: .slopguard/ui-screenshots\n")
	screenshotDir := filepath.Join(project, ".slopguard", "ui-screenshots")
	writeDashboardFile(t, filepath.Join(screenshotDir, "home.png"), "fake-png")

	handler := authedTestHandler(NewServer(db, nil))
	status, body, _ := dashboardRequest(t, handler, http.MethodGet, "/api/radar?project="+url.QueryEscape(project)+"&top=3")
	if status != http.StatusOK {
		t.Fatalf("radar status = %d, body = %s", status, body)
	}
	radarJSON := decodeDashboardJSON(t, body)
	if radarJSON["project"] != project {
		t.Fatalf("radar project = %v", radarJSON["project"])
	}
	baseline, _ := radarJSON["baseline"].(map[string]any)
	if baseline["available"] != true {
		t.Fatalf("baseline = %v, want available", baseline)
	}
	hotspots, ok := radarJSON["hotspots"].([]any)
	if !ok {
		t.Fatalf("hotspots = %v, want array", radarJSON["hotspots"])
	}
	screenshots, ok := radarJSON["screenshots"].([]any)
	if !ok || len(screenshots) != 1 {
		t.Fatalf("screenshots = %v, want one", radarJSON["screenshots"])
	}
	shot, _ := screenshots[0].(map[string]any)
	if shot["path"] != ".slopguard/ui-screenshots/home.png" {
		t.Fatalf("screenshot = %v", shot)
	}
	if len(hotspots) == 0 {
		t.Fatalf("hotspots empty for project with a source file")
	}

	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/screenshot?project="+url.QueryEscape(project)+"&path="+url.QueryEscape(".slopguard/ui-screenshots/home.png"))
	if status != http.StatusOK || string(body) != "fake-png" {
		t.Fatalf("screenshot status = %d body = %q", status, body)
	}
	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/screenshot?project="+url.QueryEscape(project)+"&path="+url.QueryEscape("../outside.png"))
	if status != http.StatusForbidden {
		t.Fatalf("path traversal status = %d, want 403: %s", status, body)
	}
}

func TestDashboardRadarMissingProjectIsGraceful(t *testing.T) {
	handler := authedTestHandler(NewServer(nil, nil))
	missing := filepath.Join(t.TempDir(), "missing-project")
	status, body, _ := dashboardRequest(t, handler, http.MethodGet, "/api/radar?project="+url.QueryEscape(missing))
	if status != http.StatusOK {
		t.Fatalf("missing project radar status = %d, want 200 graceful empty: %s", status, body)
	}
	payload := decodeDashboardJSON(t, body)
	if payload["project"] != missing {
		t.Fatalf("project = %v, want %s", payload["project"], missing)
	}
	baseline, _ := payload["baseline"].(map[string]any)
	if baseline["available"] != false {
		t.Fatalf("baseline = %v, want unavailable", baseline)
	}
	if hotspots, _ := payload["hotspots"].([]any); len(hotspots) != 0 {
		t.Fatalf("hotspots = %v, want empty", payload["hotspots"])
	}
	if screenshots, _ := payload["screenshots"].([]any); len(screenshots) != 0 {
		t.Fatalf("screenshots = %v, want empty", payload["screenshots"])
	}
}

func TestDashboardEmbeddedAssets(t *testing.T) {
	if _, err := assets.ReadFile("assets/index.html"); err != nil {
		t.Fatalf("embedded index missing: %v", err)
	}
	if _, err := assets.ReadFile("assets/app.js"); err != nil {
		t.Fatalf("embedded app.js missing: %v", err)
	}
	if _, err := assets.ReadFile("assets/styles.css"); err != nil {
		t.Fatalf("embedded styles.css missing: %v", err)
	}
	handler := authedTestHandler(NewServer(nil, nil))
	status, body, header := dashboardRequest(t, handler, http.MethodGet, "/")
	if status != http.StatusOK {
		t.Fatalf("index status = %d", status)
	}
	if !strings.Contains(header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "SlopGuard") {
		t.Fatalf("index content-type=%q body=%q", header.Get("Content-Type"), body)
	}
	for _, section := range []string{"总览", "错题与规则", "接入与守护", "热点文件与跳过注释基线", "MCP 调用日志"} {
		if !strings.Contains(string(body), section) {
			t.Fatalf("index missing dashboard section %q", section)
		}
	}
	status, body, header = dashboardRequest(t, handler, http.MethodGet, "/static/styles.css")
	if status != http.StatusOK || !strings.Contains(header.Get("Content-Type"), "text/css") || len(body) == 0 {
		t.Fatalf("styles status=%d content-type=%q body-len=%d", status, header.Get("Content-Type"), len(body))
	}
	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/static/app.js")
	if status != http.StatusOK || len(body) == 0 {
		t.Fatalf("app.js status=%d body-len=%d", status, len(body))
	}
	status, _, _ = dashboardRequest(t, handler, http.MethodGet, "/static/missing.js")
	if status != http.StatusNotFound {
		t.Fatalf("missing static status = %d, want 404", status)
	}
}

func TestDashboardRunLoopbackAndGracefulShutdown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	db := openDashboardStore(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, db, nil, Options{Port: port, NoOpen: true}, &output)
	}()

	client := &http.Client{Timeout: 300 * time.Millisecond}
	var response *http.Response
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err = client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/identity", port))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("dashboard did not start: %v\noutput: %s", err, output.String())
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("identity status = %d", response.StatusCode)
	}

	// Unauthenticated root / API must fail with 401.
	unauthResp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, unauthResp.Body)
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		cancel()
		t.Fatalf("unauthenticated root status = %d, want 401", unauthResp.StatusCode)
	}

	// Read 0600 ~/.slopguard/ui-token and authenticate cleanly.
	tokRaw, err := os.ReadFile(filepath.Join(home, ".slopguard", "ui-token"))
	if err != nil {
		cancel()
		t.Fatalf("read ui-token: %v", err)
	}
	tok := strings.TrimSpace(string(tokRaw))
	if len(tok) < 32 {
		cancel()
		t.Fatalf("unexpected short token %q", tok)
	}
	authReq, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	authReq.Header.Set(uiTokenHeaderName, tok)
	authResp, err := client.Do(authReq)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, authResp.Body)
	_ = authResp.Body.Close()
	if authResp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("authenticated root status = %d, want 200", authResp.StatusCode)
	}

	if !strings.Contains(output.String(), fmt.Sprintf("http://127.0.0.1:%d/", port)) {
		t.Fatalf("startup output = %q", output.String())
	}
	if strings.Contains(output.String(), tok) {
		t.Fatalf("startup output must never log session token")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run exit error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

type fakeUIRunner struct {
	lookups map[string]string
	runFunc func(ctx context.Context, spec contract.RunSpec) contract.RunResult
}

func (f *fakeUIRunner) LookPath(name string) (string, bool) {
	if f.lookups != nil {
		if p, ok := f.lookups[name]; ok {
			return p, true
		}
	}
	return "", false
}

func (f *fakeUIRunner) Run(ctx context.Context, spec contract.RunSpec) contract.RunResult {
	if f.runFunc != nil {
		return f.runFunc(ctx, spec)
	}
	return contract.RunResult{ExitCode: 0}
}

func TestOpenBrowserCommandStandaloneAndFallback(t *testing.T) {
	url := "http://127.0.0.1:7788/"
	noFile := func(string) bool { return false }
	noLook := func(string) (string, error) { return "", fmt.Errorf("not found") }

	name, args := openBrowserCommand("darwin", url, func(p string) bool {
		return strings.Contains(p, "SlopGuard.app")
	}, noLook)
	if name != "open" || len(args) != 1 || args[0] != "/Applications/SlopGuard.app" {
		t.Fatalf("darwin slopguard.app = %q %v", name, args)
	}

	name, args = openBrowserCommand("darwin", url, func(p string) bool {
		return strings.Contains(p, "Google Chrome.app")
	}, noLook)
	if !strings.Contains(name, "Google Chrome") || len(args) != 2 || args[0] != "--app="+url || args[1] != "--window-size=1180,780" {
		t.Fatalf("darwin chrome = %q %v", name, args)
	}

	name, args = openBrowserCommand("darwin", url, noFile, noLook)
	if name != "open" || len(args) != 1 || args[0] != url {
		t.Fatalf("darwin fallback = %q %v", name, args)
	}

	name, args = openBrowserCommand("linux", url, noFile, func(bin string) (string, error) {
		if bin == "chromium" {
			return "/usr/bin/chromium", nil
		}
		return "", fmt.Errorf("not found")
	})
	if name != "/usr/bin/chromium" || args[0] != "--app="+url {
		t.Fatalf("linux chromium = %q %v", name, args)
	}

	name, args = openBrowserCommand("linux", url, noFile, noLook)
	if name != "xdg-open" || args[0] != url {
		t.Fatalf("linux fallback = %q %v", name, args)
	}

	name, args = openBrowserCommand("windows", url, noFile, noLook)
	if name != "rundll32" || len(args) != 2 || args[1] != url {
		t.Fatalf("windows fallback = %q %v", name, args)
	}
}

func TestDashboardMistakeRestoreFiltersProjectMetaAndVerify(t *testing.T) {
	db := openDashboardStore(t)
	ctx := context.Background()
	project := filepath.Join(t.TempDir(), "py-proj")
	writeDashboardFile(t, filepath.Join(project, "pyproject.toml"), "[project]\nname = \"demo\"\n")
	writeDashboardFile(t, filepath.Join(project, "app.py"), "x = 1  # noqa\n")
	writeDashboardFile(t, filepath.Join(project, ".slopguard", "config.yaml"), "version: 1\nadapters:\n  - python\nverify:\n  jobs: 3\n")
	writeDashboardFile(t, filepath.Join(project, ".slopguard", "ui.yaml"), "base_url: http://127.0.0.1:3000\nroutes:\n  - /\nscreenshot_dir: .slopguard/screenshots\ntimeout_sec: 30\n")
	writeDashboardFile(t, filepath.Join(project, ".slopguard", "baseline.json"), `{"metrics":{"noqa":0},"updated_at":"2026-01-01T00:00:00Z"}`)

	if _, err := db.RecordMistake(ctx, contract.Mistake{
		ProjectPath: project, Fingerprint: "fp-restore", Category: "type",
		Summary: "missing annotation", BadSnippet: "def f(x):", GoodSnippet: "def f(x: int):", Source: "agent",
	}); err != nil {
		t.Fatal(err)
	}

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

	server := NewServer(db, runner)
	handler := authedTestHandler(server)

	// Ignore then restore
	status, body, _ := dashboardRequest(t, handler, http.MethodPost, "/api/mistakes/fp-restore/ignore")
	if status != http.StatusOK {
		t.Fatalf("ignore status = %d, body = %s", status, body)
	}
	status, body, _ = dashboardRequest(t, handler, http.MethodPost, "/api/mistakes/fp-restore/restore")
	if status != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", status, body)
	}
	restoreJSON := decodeDashboardJSON(t, body)
	if restoreJSON["status"] != "pending" {
		t.Fatalf("restore = %v", restoreJSON)
	}

	// Filter mistakes by category/source/q
	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/mistakes?category=type&source=agent&q=annotation")
	if status != http.StatusOK {
		t.Fatalf("mistakes filter status = %d, body = %s", status, body)
	}
	mJSON := decodeDashboardJSON(t, body)
	if items, _ := mJSON["mistakes"].([]any); len(items) != 1 {
		t.Fatalf("filtered mistakes = %v", mJSON)
	}

	// Project meta
	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/project-meta?project="+url.QueryEscape(project))
	if status != http.StatusOK {
		t.Fatalf("project-meta status = %d, body = %s", status, body)
	}
	metaJSON := decodeDashboardJSON(t, body)
	if metaJSON["exists"] != true {
		t.Fatalf("meta exists = %v", metaJSON)
	}
	adapters, _ := metaJSON["adapters"].([]any)
	if len(adapters) != 1 {
		t.Fatalf("meta adapters = %v", metaJSON["adapters"])
	}

	// POST /api/verify with record=true
	reqBody := fmt.Sprintf(`{"project":%q,"all":true,"ui":false,"record":true}`, project)
	req := httptest.NewRequest(http.MethodPost, "/api/verify", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify status = %d, body = %s", rec.Code, rec.Body.String())
	}
	verifyJSON := decodeDashboardJSON(t, rec.Body.Bytes())
	if verifyJSON["passed"] != false {
		t.Fatalf("verify passed = %v, want false (ruff + ratchet)", verifyJSON)
	}
	findings, _ := verifyJSON["findings"].([]any)
	if len(findings) < 2 {
		t.Fatalf("verify findings = %v, want ruff + ratchet findings", findings)
	}

	// Verify scan_runs was recorded in overview
	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/overview?project="+url.QueryEscape(project))
	if status != http.StatusOK {
		t.Fatalf("overview status = %d", status)
	}
	ov := decodeDashboardJSON(t, body)
	if runs, _ := ov["recent_runs"].([]any); len(runs) != 1 {
		t.Fatalf("recent_runs = %v, want 1", ov["recent_runs"])
	}
}

func TestDashboardEventsApproveAndServiceStatusAPI(t *testing.T) {
	db := openDashboardStore(t)
	ctx := context.Background()
	projectA := filepath.Join(t.TempDir(), "project-a")
	projectB := filepath.Join(t.TempDir(), "project-b")
	for _, dir := range []string{projectA, projectB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Seed MCP events
	if err := db.RecordMCPEvent(ctx, store.MCPEvent{
		ID:            "evt-1",
		ProjectPath:   projectA,
		ToolName:      "sg_verify",
		ArgsSummary:   "all=true",
		Passed:        true,
		FindingsCount: 0,
		DurationMs:    42,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMCPEvent(ctx, store.MCPEvent{
		ID:            "evt-2",
		ProjectPath:   projectB,
		ToolName:      "sg_radar",
		ArgsSummary:   "top=5",
		Passed:        false,
		FindingsCount: 3,
		DurationMs:    120,
		ErrorText:     "hotspots found",
	}); err != nil {
		t.Fatal(err)
	}

	// Seed mistake, submit AI analysis, then test review_status filter + approve
	if _, err := db.RecordMistake(ctx, contract.Mistake{
		ProjectPath: projectA,
		Fingerprint: "fp-approve-1",
		Category:    "type",
		Summary:     "avoid any",
		BadSnippet:  "const x: any = 1",
		GoodSnippet: "const x: number = 1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SubmitMistakeAnalysis(ctx, "fp-approve-1", "recommend", "滥用 any 会绕过类型检查", "no-explicit-any"); err != nil {
		t.Fatal(err)
	}

	server := NewServer(db, nil)
	handler := authedTestHandler(server)

	// 1. GET /api/events
	status, body, _ := dashboardRequest(t, handler, http.MethodGet, "/api/events?project="+url.QueryEscape(projectA)+"&tool=sg_verify&limit=10")
	if status != http.StatusOK {
		t.Fatalf("events status = %d, body = %s", status, body)
	}
	eventsJSON := decodeDashboardJSON(t, body)
	events, _ := eventsJSON["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %v, want 1 event", eventsJSON)
	}
	ev, _ := events[0].(map[string]any)
	if ev["tool_name"] != "sg_verify" || ev["passed"] != true || ev["duration_ms"] != float64(42) {
		t.Fatalf("event = %v", ev)
	}

	// Invalid limit & method check
	status, _, _ = dashboardRequest(t, handler, http.MethodGet, "/api/events?limit=-1")
	if status != http.StatusBadRequest {
		t.Fatalf("negative limit status = %d, want 400", status)
	}
	status, _, _ = dashboardRequest(t, handler, http.MethodPost, "/api/events")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/events status = %d, want 405", status)
	}

	// 2. GET /api/mistakes?review_status=pending_user + AI fields
	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/mistakes?review_status=pending_user")
	if status != http.StatusOK {
		t.Fatalf("mistakes review_status filter = %d, body = %s", status, body)
	}
	mistakesJSON := decodeDashboardJSON(t, body)
	records, _ := mistakesJSON["mistakes"].([]any)
	if len(records) != 1 {
		t.Fatalf("pending_user mistakes = %v, want 1", mistakesJSON)
	}
	rec, _ := records[0].(map[string]any)
	if rec["review_status"] != "pending_user" || rec["ai_decision"] != "recommend" || rec["ai_reason"] != "滥用 any 会绕过类型检查" || rec["ai_rule_draft"] != "no-explicit-any" {
		t.Fatalf("mistake AI fields = %v", rec)
	}
	if _, ok := rec["ai_analyzed_at"]; !ok {
		t.Fatalf("expected ai_analyzed_at in mistake record: %v", rec)
	}

	// 3. POST /api/mistakes/{fp}/approve
	status, body, _ = dashboardRequest(t, handler, http.MethodPost, "/api/mistakes/fp-approve-1/approve")
	if status != http.StatusOK {
		t.Fatalf("approve status = %d, body = %s", status, body)
	}
	approveJSON := decodeDashboardJSON(t, body)
	if approveJSON["ok"] != true || approveJSON["fingerprint"] != "fp-approve-1" || approveJSON["status"] != "promoted" || approveJSON["review_status"] != "approved" {
		t.Fatalf("approve response = %v", approveJSON)
	}

	// Verify review_status=approved filter after approve
	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/mistakes?review_status=approved")
	if status != http.StatusOK {
		t.Fatalf("approved mistakes status = %d", status)
	}
	approvedJSON := decodeDashboardJSON(t, body)
	if items, _ := approvedJSON["mistakes"].([]any); len(items) != 1 {
		t.Fatalf("approved mistakes = %v, want 1", approvedJSON)
	}

	// Missing fingerprint & wrong method
	status, _, _ = dashboardRequest(t, handler, http.MethodPost, "/api/mistakes/nonexistent/approve")
	if status != http.StatusNotFound {
		t.Fatalf("missing approve status = %d, want 404", status)
	}
	status, _, _ = dashboardRequest(t, handler, http.MethodGet, "/api/mistakes/fp-approve-1/approve")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("GET approve status = %d, want 405", status)
	}

	// 4. GET /api/service-status
	status, body, _ = dashboardRequest(t, handler, http.MethodGet, "/api/service-status")
	if status != http.StatusOK {
		t.Fatalf("service-status = %d, body = %s", status, body)
	}
	svcJSON := decodeDashboardJSON(t, body)
	if svcJSON["port"] != float64(7788) || svcJSON["platform"] == "" || svcJSON["pid_hint"] == nil {
		t.Fatalf("service-status = %v", svcJSON)
	}
	status, _, _ = dashboardRequest(t, handler, http.MethodPost, "/api/service-status")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("POST service-status = %d, want 405", status)
	}

	// Unit test detectServiceStatus helper directly
	plist := filepath.Join("/Users/tester", "Library", "LaunchAgents", "com.slopguard.ui.plist")
	installed := detectServiceStatus("darwin", "/Users/tester", 7788, func(p string) bool { return p == plist })
	if !installed.Installed || installed.Platform != "darwin" || installed.Port != 7788 || installed.PIDHint <= 0 {
		t.Fatalf("detectServiceStatus(installed) = %+v", installed)
	}
	notInstalled := detectServiceStatus("darwin", "/Users/tester", 8899, func(string) bool { return false })
	if notInstalled.Installed || notInstalled.Port != 8899 {
		t.Fatalf("detectServiceStatus(not installed) = %+v", notInstalled)
	}
}

func TestDashboardSecurityMiddlewareAndTrustBoundary(t *testing.T) {
	db := openDashboardStore(t)
	s := NewServer(db, nil)
	s.homeDir = t.TempDir()
	s.port = 7788
	s.instanceID = "test-instance"
	s.startedAt = time.Now().UTC()
	handler := s.Handler()

	// 1. Public discovery endpoint (/api/identity) works on loopback host without token.
	idReq := httptest.NewRequest(http.MethodGet, "/api/identity", nil)
	idReq.Host = "127.0.0.1:7788"
	idRec := httptest.NewRecorder()
	handler.ServeHTTP(idRec, idReq)
	if idRec.Code != http.StatusOK {
		t.Fatalf("GET /api/identity = %d: %s", idRec.Code, idRec.Body.String())
	}
	idJSON := decodeDashboardJSON(t, idRec.Body.Bytes())
	if idJSON["service"] != "slopguard" || idJSON["auth_required"] != true || idJSON["instance_id"] != "test-instance" {
		t.Fatalf("unexpected identity payload: %v", idJSON)
	}
	for _, secretKey := range []string{"token", "auth_token", "ui_token"} {
		if _, leaked := idJSON[secretKey]; leaked {
			t.Fatalf("identity leaked %q: %v", secretKey, idJSON)
		}
	}

	// 2. Protected /api/* rejects missing or invalid session token with 401.
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"missing", ""},
		{"wrong", "0000000000000000"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
		req.Host = "127.0.0.1:7788"
		if tc.token != "" {
			req.Header.Set(uiTokenHeaderName, tc.token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s token: GET /api/overview = %d, want 401", tc.name, rec.Code)
		}
	}

	// 3. Rejects DNS rebinding / non-loopback Host and cross-site Origin / Sec-Fetch-Site.
	badHostReq := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	badHostReq.Host = "evil.example.com:7788"
	badHostReq.Header.Set(uiTokenHeaderName, s.AuthToken())
	badHostRec := httptest.NewRecorder()
	handler.ServeHTTP(badHostRec, badHostReq)
	if badHostRec.Code != http.StatusForbidden {
		t.Fatalf("bad host = %d, want 403", badHostRec.Code)
	}

	badOriginReq := httptest.NewRequest(http.MethodPost, "/api/ai/config", strings.NewReader(`{}`))
	badOriginReq.Host = "127.0.0.1:7788"
	badOriginReq.Header.Set("Origin", "https://evil.com")
	badOriginReq.Header.Set("Content-Type", "application/json")
	badOriginReq.Header.Set(uiTokenHeaderName, s.AuthToken())
	badOriginRec := httptest.NewRecorder()
	handler.ServeHTTP(badOriginRec, badOriginReq)
	if badOriginRec.Code != http.StatusForbidden {
		t.Fatalf("bad origin = %d, want 403", badOriginRec.Code)
	}

	crossSiteReq := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	crossSiteReq.Host = "127.0.0.1:7788"
	crossSiteReq.Header.Set("Sec-Fetch-Site", "cross-site")
	crossSiteReq.Header.Set(uiTokenHeaderName, s.AuthToken())
	crossSiteRec := httptest.NewRecorder()
	handler.ServeHTTP(crossSiteRec, crossSiteReq)
	if crossSiteRec.Code != http.StatusForbidden {
		t.Fatalf("cross-site = %d, want 403", crossSiteRec.Code)
	}

	// 4. Browser bootstrap uses a short-lived one-time ticket; a long-term token in the URL is rejected.
	if bootCode, _, _ := dashboardRequest(t, handler, http.MethodGet, "/?token="+url.QueryEscape(s.AuthToken())); bootCode != http.StatusUnauthorized {
		t.Fatalf("GET /?token=... = %d, want 401", bootCode)
	}
	ticketReq := httptest.NewRequest(http.MethodPost, "/api/auth/ticket", nil)
	ticketReq.Host = "127.0.0.1:7788"
	ticketReq.Header.Set(uiTokenHeaderName, s.AuthToken())
	ticketRec := httptest.NewRecorder()
	handler.ServeHTTP(ticketRec, ticketReq)
	if ticketRec.Code != http.StatusOK {
		t.Fatalf("POST /api/auth/ticket = %d: %s", ticketRec.Code, ticketRec.Body.String())
	}
	var ticketPayload struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(ticketRec.Body.Bytes(), &ticketPayload); err != nil || ticketPayload.Ticket == "" {
		t.Fatalf("ticket payload invalid: %v err=%v", ticketRec.Body.String(), err)
	}
	bootReq := httptest.NewRequest(http.MethodGet, "/?ticket="+url.QueryEscape(ticketPayload.Ticket), nil)
	bootReq.Host = "127.0.0.1:7788"
	bootRec := httptest.NewRecorder()
	handler.ServeHTTP(bootRec, bootReq)
	if bootRec.Code != http.StatusFound {
		t.Fatalf("GET /?ticket=... = %d, want 302", bootRec.Code)
	}
	cookies := bootRec.Result().Cookies()
	var sessCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == uiSessionCookieName {
			sessCookie = c
			break
		}
	}
	if sessCookie == nil || !sessCookie.HttpOnly || sessCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("expected HttpOnly SameSite=Strict cookie, got %+v", sessCookie)
	}
	replayReq := httptest.NewRequest(http.MethodGet, "/?ticket="+url.QueryEscape(ticketPayload.Ticket), nil)
	replayReq.Host = "127.0.0.1:7788"
	replayRec := httptest.NewRecorder()
	handler.ServeHTTP(replayRec, replayReq)
	if replayRec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed ticket = %d, want 401", replayRec.Code)
	}

	// Cookie POST without X-Requested-With must be rejected with 403.
	noCSRFReq := httptest.NewRequest(http.MethodPost, "/api/ai/config", strings.NewReader(`{"auto_analyze":false}`))
	noCSRFReq.Host = "127.0.0.1:7788"
	noCSRFReq.AddCookie(sessCookie)
	noCSRFReq.Header.Set("Content-Type", "application/json")
	noCSRFRec := httptest.NewRecorder()
	handler.ServeHTTP(noCSRFRec, noCSRFReq)
	if noCSRFRec.Code != http.StatusForbidden {
		t.Fatalf("cookie POST without X-Requested-With = %d, want 403", noCSRFRec.Code)
	}

	// Cookie POST with X-Requested-With but non-JSON Content-Type rejected with 415.
	badCTReq := httptest.NewRequest(http.MethodPost, "/api/ai/config", strings.NewReader(`auto_analyze=false`))
	badCTReq.Host = "127.0.0.1:7788"
	badCTReq.AddCookie(sessCookie)
	badCTReq.Header.Set("X-Requested-With", "SlopGuard")
	badCTReq.Header.Set("Content-Type", "text/plain")
	badCTRec := httptest.NewRecorder()
	handler.ServeHTTP(badCTRec, badCTReq)
	if badCTRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bad content-type = %d, want 415", badCTRec.Code)
	}

	// Valid cookie POST with X-Requested-With + application/json succeeds.
	okCookieReq := httptest.NewRequest(http.MethodPost, "/api/ai/config", strings.NewReader(`{"auto_analyze":false}`))
	okCookieReq.Host = "127.0.0.1:7788"
	okCookieReq.AddCookie(sessCookie)
	okCookieReq.Header.Set("X-Requested-With", "SlopGuard")
	okCookieReq.Header.Set("Content-Type", "application/json")
	okCookieRec := httptest.NewRecorder()
	handler.ServeHTTP(okCookieRec, okCookieReq)
	if okCookieRec.Code != http.StatusOK {
		t.Fatalf("valid cookie POST = %d: %s", okCookieRec.Code, okCookieRec.Body.String())
	}

	// 5. Oversized request body (> 2 MiB) returns 413 Request Entity Too Large.
	bigBody := `{"project":"` + strings.Repeat("a", (2<<20)+1024) + `"}`
	bigReq := httptest.NewRequest(http.MethodPost, "/api/mistakes/analyze-all", strings.NewReader(bigBody))
	bigReq.Host = "127.0.0.1:7788"
	bigReq.Header.Set(uiTokenHeaderName, s.AuthToken())
	bigReq.Header.Set("Content-Type", "application/json")
	bigRec := httptest.NewRecorder()
	handler.ServeHTTP(bigRec, bigReq)
	if bigRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413: %s", bigRec.Code, bigRec.Body.String())
	}
}
