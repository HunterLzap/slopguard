package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func openMemoryStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:) error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenMigratesSchemaAndWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "slopguard.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}

	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	tables := map[string]bool{}
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables[name] = true
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mistakes", "rules", "scan_runs", "schema_version"} {
		if !tables[want] {
			t.Fatalf("table %q missing; tables = %v", want, tables)
		}
	}
	var version int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening the same file must not re-run migrations or fail on existing tables.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen error: %v", err)
	}
	defer s2.Close()
	if err := s2.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("reopened schema version = %d, want %d", version, schemaVersion)
	}
}

func TestRecordMistakeFingerprintDedup(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	m := contract.Mistake{
		ProjectPath: "/project/a",
		Fingerprint: "same-fingerprint",
		Category:    "type",
		Summary:     "example mistake",
		Source:      "manual",
	}
	first, err := s.RecordMistake(ctx, m)
	if err != nil {
		t.Fatalf("first RecordMistake: %v", err)
	}
	if first != 1 {
		t.Fatalf("first count = %d, want 1", first)
	}
	second, err := s.RecordMistake(ctx, m)
	if err != nil {
		t.Fatalf("second RecordMistake: %v", err)
	}
	if second != 2 {
		t.Fatalf("second count = %d, want 2", second)
	}

	var rows, count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mistakes`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("mistakes rows = %d, want 1", rows)
	}
	if err := s.db.QueryRow(`SELECT count FROM mistakes WHERE fingerprint = ?`, m.Fingerprint).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("stored count = %d, want 2", count)
	}

	if candidates, err := s.ListCandidates(ctx, 2); err != nil || len(candidates) != 1 {
		t.Fatalf("ListCandidates(2) = %v, %v; want 1", candidates, err)
	}
	if candidates, err := s.ListCandidates(ctx, 3); err != nil || len(candidates) != 0 {
		t.Fatalf("ListCandidates(3) = %v, %v; want 0", candidates, err)
	}
	if candidates, err := s.ListCandidates(ctx, 0); err != nil || len(candidates) != 1 {
		t.Fatalf("ListCandidates(0 default 2) = %v, %v; want 1", candidates, err)
	}
}

func TestRecordMistakeGlobalFingerprintAcrossProjects(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	first, err := s.RecordMistake(ctx, contract.Mistake{ProjectPath: "/p1", Fingerprint: "global"})
	if err != nil || first != 1 {
		t.Fatalf("first = %d, %v", first, err)
	}
	second, err := s.RecordMistake(ctx, contract.Mistake{ProjectPath: "/p2", Fingerprint: "global"})
	if err != nil || second != 2 {
		t.Fatalf("second = %d, %v", second, err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mistakes`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want one global fingerprint row", rows)
	}
}

func TestRecordMistakeValidation(t *testing.T) {
	s := openMemoryStore(t)
	if _, err := s.RecordMistake(context.Background(), contract.Mistake{Fingerprint: "x"}); err == nil {
		t.Fatal("missing project path should error")
	}
	if _, err := s.RecordMistake(context.Background(), contract.Mistake{ProjectPath: "/p"}); err == nil {
		t.Fatal("missing fingerprint should error")
	}
	var nilStore *Store
	if _, err := nilStore.RecordMistake(context.Background(), contract.Mistake{ProjectPath: "/p", Fingerprint: "x"}); err == nil {
		t.Fatal("nil store should error")
	}
}

func TestRecordScan(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	if err := s.RecordScan(ctx, "/project/a", 1234, 5, true); err != nil {
		t.Fatalf("RecordScan: %v", err)
	}
	if err := s.RecordScan(ctx, "/project/a", -10, -2, false); err != nil {
		t.Fatalf("RecordScan negative values: %v", err)
	}
	var (
		count, duration, escapes, passed int
	)
	if err := s.db.QueryRow(`SELECT COUNT(*), MAX(duration_ms), MAX(escapes_cnt), MAX(passed) FROM scan_runs`).Scan(&count, &duration, &escapes, &passed); err != nil {
		t.Fatal(err)
	}
	if count != 2 || duration != 1234 || escapes != 5 || passed != 1 {
		t.Fatalf("scan_runs aggregates = count=%d duration=%d escapes=%d passed=%d", count, duration, escapes, passed)
	}
	var negativeDuration, negativeEscapes int
	if err := s.db.QueryRow(`SELECT duration_ms, escapes_cnt FROM scan_runs WHERE passed = 0`).Scan(&negativeDuration, &negativeEscapes); err != nil {
		t.Fatal(err)
	}
	if negativeDuration != 0 || negativeEscapes != 0 {
		t.Fatalf("negative values not clamped: duration=%d escapes=%d", negativeDuration, negativeEscapes)
	}
}

func TestConcurrentRecordMistake(t *testing.T) {
	s := openMemoryStore(t)
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	counts := make(chan int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			count, err := s.RecordMistake(context.Background(), contract.Mistake{
				ProjectPath: "/concurrent",
				Fingerprint: "concurrent-fingerprint",
				Category:    "syntax",
				Summary:     "concurrent",
			})
			if err != nil {
				errs <- err
				return
			}
			counts <- count
		}()
	}
	wg.Wait()
	close(errs)
	close(counts)
	for err := range errs {
		t.Fatalf("concurrent RecordMistake: %v", err)
	}
	seen := map[int]bool{}
	for count := range counts {
		seen[count] = true
	}
	if len(seen) != workers {
		t.Fatalf("returned counts = %v, want %d distinct values", seen, workers)
	}
	var final int
	if err := s.db.QueryRow(`SELECT count FROM mistakes WHERE fingerprint = 'concurrent-fingerprint'`).Scan(&final); err != nil {
		t.Fatal(err)
	}
	if final != workers {
		t.Fatalf("final count = %d, want %d", final, workers)
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	const writers = 16
	const readers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers+readers)
	for i := 0; i < writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.RecordMistake(ctx, contract.Mistake{
				ProjectPath: "/rw",
				Fingerprint: fmt.Sprintf("rw-%d", i),
				Category:    "type",
				Summary:     "concurrent rw",
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if _, err := s.ListCandidates(ctx, 1); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read/write: %v", err)
	}
}

func TestListCandidatesOrdering(t *testing.T) {
	s := openMemoryStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.RecordMistake(ctx, contract.Mistake{Fingerprint: fmt.Sprintf("f%d", i), ProjectPath: "/p", Category: "type", Summary: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	// Bump f0 twice more so it leads the ordering.
	for i := 0; i < 2; i++ {
		if _, err := s.RecordMistake(ctx, contract.Mistake{Fingerprint: "f0", ProjectPath: "/p"}); err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := s.ListCandidates(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Fingerprint != "f0" {
		t.Fatalf("candidates = %+v, want f0 only", candidates)
	}
	if candidates[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt was not populated")
	}
}

func TestDefaultPath(t *testing.T) {
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath error: %v", err)
	}
	if !strings.HasSuffix(path, filepath.Join(".slopguard", "slopguard.db")) {
		t.Fatalf("DefaultPath = %q", path)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("empty path should error")
	}
	var nilStore *Store
	if err := nilStore.Close(); err != nil {
		t.Fatalf("nil Close = %v, want nil", err)
	}
	if _, err := nilStore.ListCandidates(context.Background(), 2); err == nil {
		t.Fatal("nil ListCandidates should error")
	}
}

// TestStoreContractCompile ensures *Store remains assignable to contract.Store.
func TestStoreContractCompile(t *testing.T) {
	var s *Store
	var _ contract.Store = s
}
