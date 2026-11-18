package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/HunterLzap/slopguard/internal/contract"
)

const (
	driverName     = "sqlite"
	schemaVersion  = 2
	defaultDirName = ".slopguard"
	defaultDBName  = "slopguard.db"
)

// Store 是基于 modernc.org/sqlite 的 contract.Store 实现。
// SQLite 自身只允许单写者，因此写操作由 writeMu 串行化；读操作可并发执行。
type Store struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

var _ contract.Store = (*Store)(nil)

// DefaultPath 返回默认数据库路径 ~/.slopguard/slopguard.db。
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("store: locate user home: %w", err)
	}
	return filepath.Join(home, defaultDirName, defaultDBName), nil
}

// OpenDefault 打开默认数据库，目录不存在时自动创建。
func OpenDefault() (*Store, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return Open(path)
}

// Open 打开（必要时创建）path 处的数据库，并执行自动迁移。
// path 为 ":memory:" 时使用单连接内存数据库（测试场景）。
func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("store: empty database path")
	}

	if path != ":memory:" {
		dir := filepath.Dir(path)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("store: create database directory %s: %w", dir, err)
			}
		}
	}

	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
	}

	store := &Store{db: db, path: path}
	if err := store.configure(); err != nil {
		_ = db.Close()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Path 返回数据库文件路径。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *Store) configure() error {
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
	}
	for _, pragma := range pragmas {
		if _, err := s.db.Exec(pragma); err != nil {
			return fmt.Errorf("store: apply %s: %w", pragma, err)
		}
	}
	// journal_mode 需要用 Query 读取返回行，Exec 在某些驱动上不会消费结果。
	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("store: enable WAL: %w", err)
	}
	return nil
}

type migration struct {
	version    int
	statements []string
}

var migrations = []migration{
	{
		version: 1,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS mistakes (
				id            TEXT PRIMARY KEY,
				project_path  TEXT NOT NULL,
				fingerprint   TEXT NOT NULL,
				category      TEXT NOT NULL,
				summary       TEXT NOT NULL,
				bad_snippet   TEXT NOT NULL DEFAULT '',
				good_snippet  TEXT NOT NULL DEFAULT '',
				source        TEXT NOT NULL,
				status        TEXT NOT NULL DEFAULT 'pending',
				count         INTEGER NOT NULL DEFAULT 1,
				created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_mistakes_fingerprint
				ON mistakes(fingerprint)`,
			`CREATE INDEX IF NOT EXISTS idx_mistakes_count_status
				ON mistakes(count DESC, status)`,
			`CREATE TABLE IF NOT EXISTS rules (
				id            TEXT PRIMARY KEY,
				scope         TEXT NOT NULL,
				project_path  TEXT NOT NULL DEFAULT '',
				engine        TEXT NOT NULL,
				rule_key      TEXT NOT NULL,
				payload       TEXT NOT NULL,
				rationale     TEXT NOT NULL,
				hit_count     INTEGER NOT NULL DEFAULT 0,
				created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE INDEX IF NOT EXISTS idx_rules_engine_rule_key ON rules(engine, rule_key)`,
			`CREATE TABLE IF NOT EXISTS scan_runs (
				id            TEXT PRIMARY KEY,
				project_path  TEXT NOT NULL,
				duration_ms   INTEGER NOT NULL,
				escapes_cnt   INTEGER NOT NULL,
				passed        INTEGER NOT NULL,
				created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE INDEX IF NOT EXISTS idx_scan_runs_project_created
				ON scan_runs(project_path, created_at DESC)`,
		},
	},
	{
		version: 2,
		statements: []string{
			`ALTER TABLE rules ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE rules ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`,
			`ALTER TABLE rules ADD COLUMN applied_at DATETIME`,
			`CREATE INDEX IF NOT EXISTS idx_rules_status ON rules(status)`,
		},
	},
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("store: create schema_version: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_version`,
	).Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin migration %d: %w", m.version, err)
		}
		for _, statement := range m.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("store: apply migration %d: %w", m.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_version(version) VALUES(?)`, m.version,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

// RecordMistake 记录一条错题；同一 fingerprint 重复出现时累加 count。
// 返回该指纹当前累计次数。
func (s *Store) RecordMistake(ctx context.Context, m contract.Mistake) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.ProjectPath = strings.TrimSpace(m.ProjectPath)
	m.Fingerprint = strings.TrimSpace(m.Fingerprint)
	if m.ProjectPath == "" {
		return 0, errors.New("store: mistake project path is required")
	}
	if m.Fingerprint == "" {
		return 0, errors.New("store: mistake fingerprint is required")
	}
	if m.ID == "" {
		m.ID = newID("mistake")
	}
	if m.Status == "" {
		m.Status = "pending"
	}
	if m.Source == "" {
		m.Source = "manual"
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin record mistake: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `INSERT INTO mistakes
		(id, project_path, fingerprint, category, summary, bad_snippet, good_snippet, source, status, count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET count = count + 1`,
		m.ID, m.ProjectPath, m.Fingerprint, m.Category, m.Summary, m.BadSnippet, m.GoodSnippet,
		m.Source, m.Status, m.CreatedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return 0, fmt.Errorf("store: insert mistake: %w", err)
	}

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count FROM mistakes WHERE fingerprint = ?`,
		m.Fingerprint,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: read mistake count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit mistake: %w", err)
	}
	return count, nil
}

// ListCandidates 返回累计次数达到 minCount 的待处理错题簇（按次数降序）。
// minCount <= 0 时按架构默认值 2 处理。
func (s *Store) ListCandidates(ctx context.Context, minCount int) ([]contract.Mistake, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if minCount <= 0 {
		minCount = 2
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, project_path, fingerprint, category, summary,
		COALESCE(bad_snippet, ''), COALESCE(good_snippet, ''),
		source, status, created_at
		FROM mistakes
		WHERE count >= ? AND status = 'pending'
		ORDER BY count DESC, created_at ASC, id ASC`, minCount)
	if err != nil {
		return nil, fmt.Errorf("store: list candidates: %w", err)
	}
	defer rows.Close()

	var candidates []contract.Mistake
	for rows.Next() {
		var (
			m       contract.Mistake
			created any
		)
		if err := rows.Scan(
			&m.ID, &m.ProjectPath, &m.Fingerprint, &m.Category, &m.Summary,
			&m.BadSnippet, &m.GoodSnippet, &m.Source, &m.Status, &created,
		); err != nil {
			return nil, fmt.Errorf("store: scan candidate: %w", err)
		}
		m.CreatedAt = coerceTime(created)
		candidates = append(candidates, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate candidates: %w", err)
	}
	if candidates == nil {
		candidates = []contract.Mistake{}
	}
	return candidates, nil
}

// RecordScan 记录一次扫描运行。
func (s *Store) RecordScan(ctx context.Context, projectPath string, durationMs int64, escapes int, passed bool) error {
	if s == nil || s.db == nil {
		return errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if durationMs < 0 {
		durationMs = 0
	}
	if escapes < 0 {
		escapes = 0
	}
	passedValue := 0
	if passed {
		passedValue = 1
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO scan_runs
		(id, project_path, duration_ms, escapes_cnt, passed, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		newID("scan"), projectPath, durationMs, escapes, passedValue,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("store: record scan: %w", err)
	}
	return nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

func coerceTime(value any) time.Time {
	switch v := value.(type) {
	case time.Time:
		return v
	case string:
		return parseTime(v)
	case []byte:
		return parseTime(string(v))
	case int64:
		return time.Unix(v, 0).UTC()
	default:
		return time.Time{}
	}
}

func parseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
