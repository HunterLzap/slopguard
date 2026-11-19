package store

import (
	"context"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func TestOverviewStatsFiltersAndTrend(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	p1 := "/projects/one"
	p2 := "/projects/two"
	if err := s.RecordScan(ctx, p1, 100, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordScan(ctx, p1, 300, 2, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordScan(ctx, p2, 200, 3, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE scan_runs SET created_at = ? WHERE project_path = ? AND duration_ms = ?`,
		"2026-01-01T10:00:00Z", p1, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE scan_runs SET created_at = ? WHERE project_path = ? AND duration_ms = ?`,
		"2026-01-01T11:00:00Z", p1, 300); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE scan_runs SET created_at = ? WHERE project_path = ? AND duration_ms = ?`,
		"2026-01-09T09:00:00Z", p2, 200); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRule(ctx, Rule{ID: "active-rule", Engine: "eslint", RuleID: "no-console", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRule(ctx, Rule{ID: "disabled-rule", Engine: "ruff", RuleID: "E501", Status: "disabled"}); err != nil {
		t.Fatal(err)
	}

	overview, err := s.OverviewStats(ctx, "", "day")
	if err != nil {
		t.Fatalf("OverviewStats day: %v", err)
	}
	if overview.TotalRuns != 3 || overview.FailedRuns != 1 || overview.AvgDurationMs != 200 {
		t.Fatalf("overview totals = %+v, want runs=3 failed=1 avg=200", overview)
	}
	if overview.RuleCount != 1 {
		t.Fatalf("rule count = %d, want only active rule", overview.RuleCount)
	}
	if len(overview.Trend) != 2 {
		t.Fatalf("day trend = %+v, want two buckets", overview.Trend)
	}
	if overview.Trend[0].Bucket != "2026-01-01" || overview.Trend[0].Runs != 2 || overview.Trend[0].Failed != 1 {
		t.Fatalf("first day bucket = %+v", overview.Trend[0])
	}
	if overview.Trend[1].Bucket != "2026-01-09" || overview.Trend[1].Runs != 1 {
		t.Fatalf("second day bucket = %+v", overview.Trend[1])
	}

	weekly, err := s.OverviewStats(ctx, "", "week")
	if err != nil {
		t.Fatalf("OverviewStats week: %v", err)
	}
	if len(weekly.Trend) != 2 || !strings.Contains(weekly.Trend[0].Bucket, "2026-W") {
		t.Fatalf("week trend = %+v", weekly.Trend)
	}

	filtered, err := s.OverviewStats(ctx, p1, "day")
	if err != nil {
		t.Fatalf("OverviewStats project: %v", err)
	}
	if filtered.TotalRuns != 2 || filtered.FailedRuns != 1 || filtered.AvgDurationMs != 200 {
		t.Fatalf("filtered overview = %+v", filtered)
	}
	if _, err := s.OverviewStats(ctx, "", "hour"); err == nil {
		t.Fatal("OverviewStats invalid granularity = nil, want error")
	}
}

func TestListMistakesAndProjects(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	p1 := "/projects/one"
	p2 := "/projects/two"
	for i := 0; i < 2; i++ {
		if _, err := s.RecordMistake(ctx, contract.Mistake{ProjectPath: p1, Fingerprint: "fp-one", Category: "type", Summary: "one"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordMistake(ctx, contract.Mistake{ProjectPath: p2, Fingerprint: "fp-two", Category: "import", Summary: "two"}); err != nil {
		t.Fatal(err)
	}
	// ListProjects aggregates MAX(created_at) from scan_runs and mistakes; pin
	// mistake timestamps so the scan-run ordering is deterministic on any date.
	if _, err := s.db.Exec(`UPDATE mistakes SET created_at = ? WHERE project_path = ?`, "2025-12-01T00:00:00Z", p1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE mistakes SET created_at = ? WHERE project_path = ?`, "2025-12-02T00:00:00Z", p2); err != nil {
		t.Fatal(err)
	}
	records, err := s.ListMistakes(ctx, "", "")
	if err != nil {
		t.Fatalf("ListMistakes: %v", err)
	}
	if len(records) != 2 || records[0].Fingerprint != "fp-one" || records[0].Count != 2 {
		t.Fatalf("mistakes = %+v", records)
	}
	if filtered, err := s.ListMistakes(ctx, "pending", p2); err != nil || len(filtered) != 1 || filtered[0].Fingerprint != "fp-two" {
		t.Fatalf("filtered mistakes = %+v, err=%v", filtered, err)
	}
	if _, err := s.ListMistakes(ctx, "bogus", ""); err == nil {
		t.Fatal("ListMistakes invalid status = nil, want error")
	}

	if err := s.RecordScan(ctx, p1, 100, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordScan(ctx, p2, 100, 0, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE scan_runs SET created_at = ? WHERE project_path = ?`, "2026-01-01T00:00:00Z", p1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE scan_runs SET created_at = ? WHERE project_path = ?`, "2026-01-02T00:00:00Z", p2); err != nil {
		t.Fatal(err)
	}
	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 || projects[0].ProjectPath != p2 {
		t.Fatalf("projects = %+v, want p2 first", projects)
	}
}

func TestListRuleLedgerFilters(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	p1 := "/projects/one"
	p2 := "/projects/two"
	rules := []Rule{
		{ID: "global", Scope: "global", Engine: "eslint", RuleID: "no-console", Status: "active"},
		{ID: "p1", Scope: "project", ProjectPath: p1, Engine: "eslint", RuleID: "no-debugger", Status: "active"},
		{ID: "p2", Scope: "project", ProjectPath: p2, Engine: "ruff", RuleID: "E501", Status: "active"},
		{ID: "disabled", Scope: "project", ProjectPath: p1, Engine: "ruff", RuleID: "F401", Status: "disabled"},
	}
	for _, rule := range rules {
		if err := s.InsertRule(ctx, rule); err != nil {
			t.Fatalf("InsertRule %s: %v", rule.ID, err)
		}
	}
	got, err := s.ListRuleLedger(ctx, RuleLedgerQuery{ProjectPath: p1})
	if err != nil {
		t.Fatalf("ListRuleLedger project: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("project ledger = %+v, want global + project active", got)
	}
	active, err := s.ListRuleLedger(ctx, RuleLedgerQuery{ProjectPath: p1, Status: "all"})
	if err != nil || len(active) != 3 {
		t.Fatalf("status=all ledger = %+v, err=%v", active, err)
	}
	ruff, err := s.ListRuleLedger(ctx, RuleLedgerQuery{ProjectPath: p2, Engine: "ruff"})
	if err != nil || len(ruff) != 1 || ruff[0].ID != "p2" {
		t.Fatalf("engine ledger = %+v, err=%v", ruff, err)
	}
}

func TestDashboardEnhancedQueries(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	p1 := "/projects/alpha"

	for i := 0; i < 35; i++ {
		if err := s.RecordScan(ctx, p1, int64(100+i), i%4, i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordMistake(ctx, contract.Mistake{
		ProjectPath: p1, Fingerprint: "fp-cand", Category: "type", Summary: "avoid any",
		BadSnippet: "let a: any", GoodSnippet: "let a: unknown", Source: "agent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMistake(ctx, contract.Mistake{
		ProjectPath: p1, Fingerprint: "fp-cand", Category: "type", Summary: "avoid any",
		Source: "agent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMistake(ctx, contract.Mistake{
		ProjectPath: p1, Fingerprint: "fp-single", Category: "import", Summary: "bad import",
		Source: "manual",
	}); err != nil {
		t.Fatal(err)
	}

	overview, err := s.OverviewStats(ctx, p1, "day")
	if err != nil {
		t.Fatalf("OverviewStats: %v", err)
	}
	if len(overview.RecentRuns) != 30 {
		t.Fatalf("recent_runs len = %d, want 30", len(overview.RecentRuns))
	}
	if overview.Totals.TotalRuns != 35 || overview.Totals.PassedRuns != 18 || overview.Totals.FailedRuns != 17 {
		t.Fatalf("totals = %+v", overview.Totals)
	}
	if overview.Totals.PendingMistakes != 2 || overview.Totals.CandidateMistakes != 1 {
		t.Fatalf("mistake totals = %+v", overview.Totals)
	}

	filtered, err := s.QueryMistakes(ctx, MistakeQuery{
		ProjectPath: p1,
		Category:    "type",
		Source:      "agent",
		Q:           "unknown",
	})
	if err != nil {
		t.Fatalf("QueryMistakes: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Fingerprint != "fp-cand" || !filtered[0].IsCandidate || filtered[0].UpdatedAt.IsZero() {
		t.Fatalf("filtered mistakes = %+v", filtered)
	}

	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].ScanCount != 35 || projects[0].PassedCount != 18 || projects[0].PendingMistakes != 2 {
		t.Fatalf("projects = %+v", projects)
	}
	if projects[0].PassRate <= 50 || projects[0].PassRate >= 52 {
		t.Fatalf("pass_rate = %v, want ~51.43", projects[0].PassRate)
	}
}

func TestMCPEventsAndInboxWorkflow(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	p1 := "/projects/alpha"
	p2 := "/projects/beta"

	// Record MCP events
	if err := s.RecordMCPEvent(ctx, MCPEvent{
		ProjectPath:   p1,
		ToolName:      "sg_verify",
		ArgsSummary:   `{"all":true}`,
		Passed:        false,
		FindingsCount: 3,
		DurationMs:    120,
	}); err != nil {
		t.Fatalf("RecordMCPEvent 1: %v", err)
	}
	if err := s.RecordMCPEvent(ctx, MCPEvent{
		ProjectPath:   p2,
		ToolName:      "sg_radar",
		ArgsSummary:   `{"top":5}`,
		Passed:        true,
		FindingsCount: 0,
		DurationMs:    45,
	}); err != nil {
		t.Fatalf("RecordMCPEvent 2: %v", err)
	}
	allEvents, err := s.ListMCPEvents(ctx, "", "", 10)
	if err != nil {
		t.Fatalf("ListMCPEvents all: %v", err)
	}
	if len(allEvents) != 2 {
		t.Fatalf("ListMCPEvents len = %d, want 2", len(allEvents))
	}
	filteredEvents, err := s.ListMCPEvents(ctx, p1, "sg_verify", 10)
	if err != nil {
		t.Fatalf("ListMCPEvents filtered: %v", err)
	}
	if len(filteredEvents) != 1 || filteredEvents[0].Passed || filteredEvents[0].FindingsCount != 3 {
		t.Fatalf("filteredEvents = %+v", filteredEvents)
	}

	// Record mistakes and test inbox lifecycle
	if _, err := s.RecordMistake(ctx, contract.Mistake{
		ProjectPath: p1,
		Fingerprint: "fp-auto-1",
		Category:    "type",
		Summary:     "avoid any",
		Source:      "auto_verify",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMistake(ctx, contract.Mistake{
		ProjectPath: p1,
		Fingerprint: "fp-auto-2",
		Category:    "import",
		Summary:     "unused import",
		Source:      "auto_verify",
	}); err != nil {
		t.Fatal(err)
	}

	inbox, err := s.ListInboxMistakes(ctx, p1, "")
	if err != nil {
		t.Fatalf("ListInboxMistakes: %v", err)
	}
	if len(inbox) != 2 || inbox[0].ReviewStatus != "pending_ai" {
		t.Fatalf("inbox = %+v", inbox)
	}

	// Submit AI analysis on fp-auto-1
	if err := s.SubmitMistakeAnalysis(ctx, "fp-auto-1", "recommend", "多次出现 any 类型逃生舱", `"@typescript-eslint/no-explicit-any": "error"`); err != nil {
		t.Fatalf("SubmitMistakeAnalysis: %v", err)
	}
	pendingUser, err := s.ListInboxMistakes(ctx, p1, "pending_user")
	if err != nil {
		t.Fatalf("ListInboxMistakes pending_user: %v", err)
	}
	if len(pendingUser) != 1 || pendingUser[0].Fingerprint != "fp-auto-1" ||
		pendingUser[0].AIDecision != "recommend" || pendingUser[0].AIAnalyzedAt.IsZero() {
		t.Fatalf("pending_user = %+v", pendingUser)
	}

	// Ignore and restore analyzed mistake -> restores to pending_user
	if err := s.IgnoreMistake(ctx, "fp-auto-1"); err != nil {
		t.Fatalf("IgnoreMistake: %v", err)
	}
	ignored, err := s.QueryMistakes(ctx, MistakeQuery{ReviewStatus: "ignored"})
	if err != nil || len(ignored) != 1 || ignored[0].Status != "ignored" {
		t.Fatalf("ignored = %+v, err=%v", ignored, err)
	}
	if err := s.RestoreMistake(ctx, "fp-auto-1"); err != nil {
		t.Fatalf("RestoreMistake analyzed: %v", err)
	}
	restored, err := s.QueryMistakes(ctx, MistakeQuery{ProjectPath: p1, ReviewStatus: "pending_user"})
	if err != nil || len(restored) != 1 || restored[0].Status != "pending" {
		t.Fatalf("restored analyzed = %+v, err=%v", restored, err)
	}

	// Ignore and restore unanalyzed mistake -> restores to pending_ai
	if err := s.IgnoreMistake(ctx, "fp-auto-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreMistake(ctx, "fp-auto-2"); err != nil {
		t.Fatal(err)
	}
	restoredAI, err := s.QueryMistakes(ctx, MistakeQuery{ProjectPath: p1, ReviewStatus: "pending_ai"})
	if err != nil || len(restoredAI) != 1 || restoredAI[0].Fingerprint != "fp-auto-2" {
		t.Fatalf("restored unanalyzed = %+v, err=%v", restoredAI, err)
	}

	// Approve mistake -> status='promoted', review_status='approved'
	if err := s.ApproveMistake(ctx, "fp-auto-1"); err != nil {
		t.Fatalf("ApproveMistake: %v", err)
	}
	approved, err := s.QueryMistakes(ctx, MistakeQuery{ReviewStatus: "approved"})
	if err != nil || len(approved) != 1 || approved[0].Status != "promoted" {
		t.Fatalf("approved = %+v, err=%v", approved, err)
	}
}
