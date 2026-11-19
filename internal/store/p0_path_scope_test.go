package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func TestP0ListCandidatesScopedProjectIsolation(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	repoA := t.TempDir()
	repoB := t.TempDir()

	record := func(project, fingerprint, summary string) {
		for i := 0; i < 2; i++ {
			if _, err := s.RecordMistake(ctx, contract.Mistake{
				ProjectPath: project,
				Fingerprint: fingerprint,
				Category:    "type",
				Summary:     summary,
				Source:      "auto_verify",
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	record(repoA, "fp-scoped-a", "issue A")
	record(repoB, "fp-scoped-b", "issue B")

	candidates, err := s.ListCandidatesScoped(ctx, repoA, 2)
	if err != nil {
		t.Fatalf("ListCandidatesScoped(repoA): %v", err)
	}
	if len(candidates) != 1 || candidates[0].Fingerprint != "fp-scoped-a" {
		t.Fatalf("repoA candidates = %+v", candidates)
	}

	if _, err := s.ListCandidatesScoped(ctx, "", 2); !errors.Is(err, ErrProjectScopeRequired) {
		t.Fatalf("empty project err = %v, want ErrProjectScopeRequired", err)
	}
	empty, err := s.ListCandidatesScoped(ctx, repoB, 3)
	if err != nil {
		t.Fatalf("ListCandidatesScoped(repoB,min=3): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("min count filter returned %+v", empty)
	}
}

func TestP0NormalizeHistoricalProjectPathsMigration(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	root := t.TempDir()
	canonical := filepath.Join(root, "proj")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	nonCanonical := canonical + string(os.PathSeparator) + "."
	unparseable := "\x00bad"

	insertMistake := func(id, fingerprint, projectPath string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO mistakes
			(id, project_path, fingerprint, category, summary, source)
			VALUES (?, ?, ?, 'type', 'migration test', 'manual')`,
			id, projectPath, fingerprint); err != nil {
			t.Fatalf("insert mistake %s: %v", id, err)
		}
	}
	insertMistake("m1", "fp-m1", nonCanonical)
	insertMistake("m2", "fp-m2", canonical)
	insertMistake("m3", "fp-m3", unparseable)

	insertRule := func(id, projectPath string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO rules
			(id, scope, project_path, engine, rule_key, payload, rationale)
			VALUES (?, 'project', ?, 'eslint', 'no-console', '{}', 'test')`,
			id, projectPath); err != nil {
			t.Fatalf("insert rule %s: %v", id, err)
		}
	}
	insertRule("r1", nonCanonical)
	insertRule("r2", canonical)

	runMigration := func() {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := normalizeHistoricalProjectPathsTx(ctx, tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("normalize paths: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	runMigration()

	var gotM1, gotM3 string
	if err := s.db.QueryRow(`SELECT project_path FROM mistakes WHERE id='m1'`).Scan(&gotM1); err != nil {
		t.Fatal(err)
	}
	if gotM1 != canonical {
		t.Fatalf("m1 path = %q, want %q", gotM1, canonical)
	}
	if err := s.db.QueryRow(`SELECT project_path FROM mistakes WHERE id='m3'`).Scan(&gotM3); err != nil {
		t.Fatal(err)
	}
	if gotM3 != unparseable {
		t.Fatalf("unparseable path changed: %q", gotM3)
	}
	var mistakeCount, ruleCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mistakes`).Scan(&mistakeCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM rules`).Scan(&ruleCount); err != nil {
		t.Fatal(err)
	}
	if mistakeCount != 3 || ruleCount != 2 {
		t.Fatalf("rows after migration: mistakes=%d rules=%d, want 3/2", mistakeCount, ruleCount)
	}

	var conflictPath string
	if err := s.db.QueryRow(`SELECT project_path FROM rules WHERE id='r1'`).Scan(&conflictPath); err != nil {
		t.Fatal(err)
	}
	if conflictPath != nonCanonical {
		t.Fatalf("conflicting rule path changed: %q", conflictPath)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM project_path_migrations
		WHERE table_name='mistakes' AND old_path=? ORDER BY rowid DESC LIMIT 1`, nonCanonical).Scan(&status); err != nil {
		t.Fatalf("migration record m1: %v", err)
	}
	if status != "normalized" {
		t.Fatalf("m1 migration status = %q, want normalized", status)
	}
	if err := s.db.QueryRow(`SELECT status FROM project_path_migrations
		WHERE table_name='rules' AND old_path=? ORDER BY rowid DESC LIMIT 1`, nonCanonical).Scan(&status); err != nil {
		t.Fatalf("migration record r1: %v", err)
	}
	if status != "conflict" {
		t.Fatalf("r1 migration status = %q, want conflict", status)
	}
	if err := s.db.QueryRow(`SELECT status FROM project_path_migrations
		WHERE table_name='mistakes' AND old_path=? ORDER BY rowid DESC LIMIT 1`, unparseable).Scan(&status); err != nil {
		t.Fatalf("migration record m3: %v", err)
	}
	if status != "unparseable" {
		t.Fatalf("m3 migration status = %q, want unparseable", status)
	}
	var backupCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_path_migration_backup`).Scan(&backupCount); err != nil {
		t.Fatal(err)
	}
	if backupCount < 5 {
		t.Fatalf("migration backup rows = %d, want >=5", backupCount)
	}

	// Re-running the migration must not change normalized data or lose rows.
	runMigration()
	if err := s.db.QueryRow(`SELECT project_path FROM mistakes WHERE id='m1'`).Scan(&gotM1); err != nil {
		t.Fatal(err)
	}
	if gotM1 != canonical {
		t.Fatalf("m1 path after second migration = %q", gotM1)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mistakes`).Scan(&mistakeCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM rules`).Scan(&ruleCount); err != nil {
		t.Fatal(err)
	}
	if mistakeCount != 3 || ruleCount != 2 {
		t.Fatalf("rows after second migration: mistakes=%d rules=%d", mistakeCount, ruleCount)
	}
}

func TestP0ReopenMigratesLegacyProjectPaths(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "slopguard.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	canonical := filepath.Join(root, "proj")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	nonCanonical := canonical + string(os.PathSeparator) + "."
	if _, err := s.db.Exec(`INSERT INTO mistakes
		(id, project_path, fingerprint, category, summary, source)
		VALUES ('reopen-m1', ?, 'fp-reopen', 'type', 'reopen migration', 'manual')`,
		nonCanonical); err != nil {
		t.Fatal(err)
	}
	// Simulate a v4 database: removing the v5 marker forces the v5 post-migration
	// to run again on reopen, including the historical path normalization step.
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version = 5`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var got string
	if err := s2.db.QueryRow(`SELECT project_path FROM mistakes WHERE id='reopen-m1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != canonical {
		t.Fatalf("path after reopen = %q, want %q", got, canonical)
	}
	var status string
	if err := s2.db.QueryRow(`SELECT status FROM project_path_migrations
		WHERE table_name='mistakes' AND old_path=? ORDER BY rowid DESC LIMIT 1`, nonCanonical).Scan(&status); err != nil {
		t.Fatalf("migration audit: %v", err)
	}
	if status != "normalized" {
		t.Fatalf("reopen migration status = %q, want normalized", status)
	}
}
