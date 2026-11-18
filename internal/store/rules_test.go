package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func TestUpdateMistakeStatusTransitions(t *testing.T) {
	ctx := context.Background()
	record := func(t *testing.T, s *Store, fingerprint string) {
		t.Helper()
		for i := 0; i < 2; i++ {
			if _, err := s.RecordMistake(ctx, contract.Mistake{
				ProjectPath: "/project/a",
				Fingerprint: fingerprint,
				Category:    "type",
				Summary:     "same mistake",
			}); err != nil {
				t.Fatalf("RecordMistake %s: %v", fingerprint, err)
			}
		}
	}

	t.Run("pending to promoted", func(t *testing.T) {
		s := openMemoryStore(t)
		record(t, s, "fp-promoted")
		if candidates, err := s.ListCandidates(ctx, 2); err != nil || len(candidates) != 1 {
			t.Fatalf("candidates before promote = %v, err=%v", candidates, err)
		}
		if err := s.UpdateMistakeStatus(ctx, "fp-promoted", "promoted"); err != nil {
			t.Fatalf("UpdateMistakeStatus: %v", err)
		}
		candidates, err := s.ListCandidates(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 0 {
			t.Fatalf("candidates after promote = %v, want empty", candidates)
		}
		var status string
		if err := s.db.QueryRow(`SELECT status FROM mistakes WHERE fingerprint = ?`, "fp-promoted").Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "promoted" {
			t.Fatalf("stored status = %q, want promoted", status)
		}
	})

	t.Run("pending to ignored", func(t *testing.T) {
		s := openMemoryStore(t)
		record(t, s, "fp-ignored")
		if err := s.UpdateMistakeStatus(ctx, "fp-ignored", "ignored"); err != nil {
			t.Fatalf("UpdateMistakeStatus: %v", err)
		}
		candidates, err := s.ListCandidates(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 0 {
			t.Fatalf("candidates after ignore = %v, want empty", candidates)
		}
		var status string
		if err := s.db.QueryRow(`SELECT status FROM mistakes WHERE fingerprint = ?`, "fp-ignored").Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "ignored" {
			t.Fatalf("stored status = %q, want ignored", status)
		}
	})

	t.Run("invalid status rejected", func(t *testing.T) {
		s := openMemoryStore(t)
		record(t, s, "fp-invalid")
		err := s.UpdateMistakeStatus(ctx, "fp-invalid", "bogus")
		if !errors.Is(err, ErrInvalidStatus) {
			t.Fatalf("error = %v, want ErrInvalidStatus", err)
		}
		var status string
		if err := s.db.QueryRow(`SELECT status FROM mistakes WHERE fingerprint = ?`, "fp-invalid").Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "pending" {
			t.Fatalf("status changed to %q on invalid input", status)
		}
	})

	t.Run("unknown fingerprint", func(t *testing.T) {
		s := openMemoryStore(t)
		err := s.UpdateMistakeStatus(ctx, "missing", "promoted")
		if !errors.Is(err, ErrMistakeNotFound) {
			t.Fatalf("error = %v, want ErrMistakeNotFound", err)
		}
	})

	t.Run("empty fingerprint", func(t *testing.T) {
		s := openMemoryStore(t)
		if err := s.UpdateMistakeStatus(ctx, "  ", "promoted"); err == nil {
			t.Fatal("UpdateMistakeStatus empty fingerprint = nil, want error")
		}
	})
}

func TestRulesDAOCRUD(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	applied := time.Now().UTC().Truncate(time.Second)
	rule := Rule{
		ID:          "rule-1",
		Scope:       "project",
		ProjectPath: "/project/a",
		Engine:      "ESLint",
		RuleID:      "no-console",
		Payload:     `"error"`,
		Rationale:   "AI keeps adding console.log",
		Source:      "agent",
		Status:      "active",
		HitCount:    2,
		AppliedAt:   applied,
	}
	if err := s.InsertRule(ctx, rule); err != nil {
		t.Fatalf("InsertRule: %v", err)
	}

	got, err := s.GetRule(ctx, "rule-1")
	if err != nil {
		t.Fatalf("GetRule: %v", err)
	}
	if got.Engine != "eslint" || got.RuleID != "no-console" || got.Status != "active" || got.Source != "agent" {
		t.Fatalf("GetRule = %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("CreatedAt was not persisted")
	}
	if delta := got.AppliedAt.Sub(applied); delta < -time.Second || delta > time.Second {
		t.Fatalf("AppliedAt = %v, want %v", got.AppliedAt, applied)
	}

	all, err := s.ListRules(ctx, "")
	if err != nil || len(all) != 1 {
		t.Fatalf("ListRules(all) = %v, err=%v", all, err)
	}
	eslintRules, err := s.ListRules(ctx, "ESLINT")
	if err != nil || len(eslintRules) != 1 {
		t.Fatalf("ListRules(ESLINT) = %v, err=%v", eslintRules, err)
	}
	ruffRules, err := s.ListRules(ctx, "ruff")
	if err != nil || len(ruffRules) != 0 {
		t.Fatalf("ListRules(ruff) = %v, err=%v", ruffRules, err)
	}

	rule.Status = "disabled"
	rule.HitCount = 9
	rule.AppliedAt = time.Time{}
	if err := s.UpdateRule(ctx, rule); err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	got, err = s.GetRule(ctx, "rule-1")
	if err != nil {
		t.Fatalf("GetRule after update: %v", err)
	}
	if got.Status != "disabled" || got.HitCount != 9 {
		t.Fatalf("updated rule = %+v", got)
	}
	if !got.AppliedAt.IsZero() {
		t.Fatalf("AppliedAt = %v, want zero after clearing", got.AppliedAt)
	}

	if err := s.UpdateRule(ctx, Rule{ID: "missing", Engine: "eslint", RuleID: "x"}); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("UpdateRule missing = %v, want ErrRuleNotFound", err)
	}
	if _, err := s.GetRule(ctx, "missing"); !errors.Is(err, ErrRuleNotFound) {
		t.Fatalf("GetRule missing = %v, want ErrRuleNotFound", err)
	}
	if err := s.InsertRule(ctx, Rule{Engine: "", RuleID: "x"}); err == nil {
		t.Fatal("InsertRule empty engine = nil, want error")
	}
	if err := s.InsertRule(ctx, Rule{Engine: "eslint"}); err == nil {
		t.Fatal("InsertRule empty rule id = nil, want error")
	}
	if err := s.InsertRule(ctx, Rule{ID: "rule-1", Engine: "eslint", RuleID: "dup"}); err == nil {
		t.Fatal("InsertRule duplicate id = nil, want error")
	}
}

func TestMigrationFromV1AddsRuleColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("open v1 db: %v", err)
	}
	statements := []string{
		`CREATE TABLE schema_version (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO schema_version(version) VALUES(1)`,
		`CREATE TABLE rules (
			id TEXT PRIMARY KEY,
			scope TEXT NOT NULL,
			project_path TEXT NOT NULL DEFAULT '',
			engine TEXT NOT NULL,
			rule_key TEXT NOT NULL,
			payload TEXT NOT NULL,
			rationale TEXT NOT NULL,
			hit_count INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO rules (id, scope, engine, rule_key, payload, rationale)
			VALUES ('old-rule', 'project', 'eslint', 'old-rule', '"warn"', 'legacy row')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("prepare v1 schema: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1 database: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	columns := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(rules)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			defaultV   any
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &defaultV, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"source", "status", "applied_at"} {
		if !columns[column] {
			t.Fatalf("rules column %q missing after migration; columns = %v", column, columns)
		}
	}
	got, err := s.GetRule(context.Background(), "old-rule")
	if err != nil {
		t.Fatalf("GetRule legacy row: %v", err)
	}
	if got.Payload != `"warn"` || got.Status != "active" {
		t.Fatalf("legacy row = %+v", got)
	}
}
