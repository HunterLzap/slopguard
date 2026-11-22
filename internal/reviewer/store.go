package reviewer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
	_ "modernc.org/sqlite"
)

const schema = `
	CREATE TABLE IF NOT EXISTS projects (
		path TEXT PRIMARY KEY, name TEXT NOT NULL, languages_json TEXT NOT NULL,
		last_status TEXT NOT NULL DEFAULT '', last_scan_at TEXT NOT NULL DEFAULT '',
		hook_enabled INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS mistake_fingerprints (
		id TEXT PRIMARY KEY, project_path TEXT NOT NULL REFERENCES projects(path),
		fingerprint TEXT NOT NULL, language TEXT NOT NULL, checker TEXT NOT NULL, rule_id TEXT NOT NULL,
		sample_message TEXT NOT NULL, snippets_json TEXT NOT NULL, occurrence_count INTEGER NOT NULL,
		consecutive_failures INTEGER NOT NULL, status TEXT NOT NULL, snoozed_at_count INTEGER NOT NULL DEFAULT 0,
		last_seen_at TEXT NOT NULL, UNIQUE(project_path, fingerprint)
	);
	CREATE TABLE IF NOT EXISTS rule_proposals (
		id TEXT PRIMARY KEY, project_path TEXT NOT NULL REFERENCES projects(path), fingerprint TEXT NOT NULL,
		decision TEXT NOT NULL, summary_zh TEXT NOT NULL, rule_id TEXT NOT NULL, semgrep_yaml TEXT NOT NULL,
		hint_line TEXT NOT NULL, sandbox_proof_hash TEXT NOT NULL, sandbox_positive_hits INTEGER NOT NULL,
		sandbox_project_hits INTEGER NOT NULL, sandbox_duration_ms INTEGER NOT NULL, status TEXT NOT NULL,
		applied_path TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		FOREIGN KEY(project_path, fingerprint) REFERENCES mistake_fingerprints(project_path, fingerprint)
	);
	CREATE TABLE IF NOT EXISTS app_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE INDEX IF NOT EXISTS mistake_queue ON mistake_fingerprints(status, last_seen_at);
	CREATE INDEX IF NOT EXISTS proposal_status ON rule_proposals(project_path, status);
	INSERT OR IGNORE INTO app_settings (key, value) VALUES
		('api_base_url', 'https://api.deepseek.com/v1'), ('api_key', ''), ('model', 'deepseek-chat');
`

type Store struct{ db *sql.DB }

type Settings struct {
	BaseURL string
	APIKey  string
	Model   string
}

type Project struct {
	Path        string                `json:"path"`
	Name        string                `json:"name"`
	Languages   []string              `json:"languages"`
	LastStatus  contract.VerifyStatus `json:"last_status"`
	LastScanAt  *time.Time            `json:"last_scan_at"`
	HookEnabled bool                  `json:"hook_enabled"`
}

func OpenDefault(ctx context.Context) (*Store, error) {
	dir, err := core.GlobalConfigDir()
	if err != nil {
		return nil, err
	}
	return OpenStore(ctx, filepath.Join(dir, "slopguard.db"))
}

func OpenStore(ctx context.Context, path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, err
	}
	// Reserve new databases with private permissions before SQLite creates its sidecars.
	file, err := os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		if err := file.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	uri := filepath.ToSlash(absolute)
	if !strings.HasPrefix(uri, "/") {
		uri = "/" + uri
	}
	dsn := url.URL{Scheme: "file", Path: uri}
	options := url.Values{"_pragma": {"busy_timeout(10000)", "foreign_keys(1)", "journal_mode(WAL)"}, "_txlock": {"immediate"}}
	dsn.RawQuery = options.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, schema)
		return err
	}); err != nil {
		return nil, errors.Join(fmt.Errorf("initialize SQLite: %w", err), db.Close())
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) transaction(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
			err = errors.Join(err, e)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Settings(ctx context.Context) (Settings, error) {
	var out Settings
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT value FROM app_settings WHERE key = 'api_base_url'),
		(SELECT value FROM app_settings WHERE key = 'api_key'),
		(SELECT value FROM app_settings WHERE key = 'model')`).Scan(&out.BaseURL, &out.APIKey, &out.Model)
	return out, err
}

func (s *Store) UpdateSettings(ctx context.Context, settings Settings) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO app_settings(key, value) VALUES ('api_base_url', ?), ('api_key', ?), ('model', ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, settings.BaseURL, settings.APIKey, settings.Model)
		return err
	})
}

func (s *Store) RegisterProject(ctx context.Context, project string, languages []string) error {
	data, err := encodeArray(languages)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO projects(path, name, languages_json, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET languages_json=excluded.languages_json`, project, filepath.Base(project), data, timestamp(time.Now()))
	return err
}

func (s *Store) Project(ctx context.Context, project string) (Project, error) {
	out := Project{Path: project, Name: filepath.Base(project), Languages: []string{}}
	var languages, lastScan string
	err := s.db.QueryRowContext(ctx, `SELECT languages_json, last_status, last_scan_at, hook_enabled FROM projects WHERE path=?`, project).
		Scan(&languages, &out.LastStatus, &lastScan, &out.HookEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(languages), &out.Languages); err != nil {
		return out, err
	}
	if lastScan != "" {
		t, err := time.Parse(time.RFC3339Nano, lastScan)
		if err != nil {
			return out, err
		}
		out.LastScanAt = &t
	}
	return out, nil
}

func (s *Store) SetHookEnabled(ctx context.Context, project string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO projects(path, name, languages_json, hook_enabled, created_at) VALUES (?, ?, '[]', ?, ?)
		ON CONFLICT(path) DO UPDATE SET hook_enabled=excluded.hook_enabled`, project, filepath.Base(project), enabled, timestamp(time.Now()))
	return err
}

const proposalFields = `id, project_path, fingerprint, decision, summary_zh, rule_id, semgrep_yaml, hint_line,
	sandbox_proof_hash, sandbox_positive_hits, sandbox_project_hits, sandbox_duration_ms, status, applied_path, created_at, updated_at`

type rowScanner interface{ Scan(...any) error }

func readProposal(row rowScanner) (contract.RuleProposal, error) {
	var p contract.RuleProposal
	var created, updated string
	err := row.Scan(&p.ID, &p.ProjectPath, &p.Fingerprint, &p.Decision, &p.SummaryZH, &p.RuleID, &p.SemgrepYAML, &p.HintLine,
		&p.SandboxProofHash, &p.SandboxPositiveHits, &p.SandboxProjectHits, &p.SandboxDurationMs, &p.Status, &p.AppliedPath, &created, &updated)
	if err != nil {
		return p, err
	}
	p.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return p, err
	}
	p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return p, err
}

func (s *Store) ListProposals(ctx context.Context, project string) (out []contract.RuleProposal, err error) {
	out = []contract.RuleProposal{}
	rows, err := s.db.QueryContext(ctx, `SELECT `+proposalFields+` FROM rule_proposals WHERE project_path=? AND status=? ORDER BY created_at DESC, id`,
		project, contract.ProposalProposed)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		p, err := readProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func encodeArray[T any](values []T) (string, error) {
	if values == nil {
		values = []T{}
	}
	b, err := json.Marshal(values)
	return string(b), err
}

func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
