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

func dashboardRequest(t *testing.T, handler http.Handler, method, path string) (int, []byte, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
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
	handler := server.Handler()

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
	handler := NewServer(db, nil).Handler()

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

	handler := NewServer(db, nil).Handler()
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
	handler := NewServer(nil, nil).Handler()
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
	handler := NewServer(nil, nil).Handler()
	status, body, header := dashboardRequest(t, handler, http.MethodGet, "/")
	if status != http.StatusOK {
		t.Fatalf("index status = %d", status)
	}
	if !strings.Contains(header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "SlopGuard Dashboard") {
		t.Fatalf("index content-type=%q body=%q", header.Get("Content-Type"), body)
	}
	for _, section := range []string{"效果总览", "错题本与规则进化", "规则台账", "项目雷达与 UI 截图"} {
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
		response, err = client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
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
		t.Fatalf("dashboard status = %d", response.StatusCode)
	}
	if !strings.Contains(output.String(), fmt.Sprintf("http://127.0.0.1:%d/", port)) {
		t.Fatalf("startup output = %q", output.String())
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
