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
