package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func projectPathVariants(projectPath string) ([]string, error) {
	trimmed := strings.TrimSpace(projectPath)
	if trimmed == "" {
		return nil, nil
	}
	seen := make(map[string]struct{}, 5)
	out := make([]string, 0, 5)
	add := func(v string) {
		if v == "" {
			return
		}
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	canonical, err := canonicalProjectPathStrict(trimmed)
	if err != nil {
		return nil, err
	}
	add(canonical)
	if abs, absErr := filepath.Abs(trimmed); absErr == nil {
		add(filepath.Clean(abs))
	}
	add(filepath.Clean(trimmed))
	if runtime.GOOS == "darwin" {
		for _, item := range append([]string(nil), out...) {
			if strings.HasPrefix(item, "/private/var/") {
				add("/var/" + strings.TrimPrefix(item, "/private/var/"))
			} else if strings.HasPrefix(item, "/var/") {
				add("/private/var/" + strings.TrimPrefix(item, "/var/"))
			} else if strings.HasPrefix(item, "/private/tmp/") {
				add("/tmp/" + strings.TrimPrefix(item, "/private/tmp/"))
			} else if strings.HasPrefix(item, "/tmp/") {
				add("/private/tmp/" + strings.TrimPrefix(item, "/tmp/"))
			}
		}
	}
	return out, nil
}

func projectPathScopeClause(projectPath string) (clause string, args []any, err error) {
	variants, err := projectPathVariants(projectPath)
	if err != nil || len(variants) == 0 {
		return "", nil, err
	}
	if len(variants) == 1 {
		return "project_path = ?", []any{variants[0]}, nil
	}
	placeholders := make([]string, len(variants))
	args = make([]any, len(variants))
	for i, v := range variants {
		placeholders[i] = "?"
		args[i] = v
	}
	return "project_path IN (" + strings.Join(placeholders, ", ") + ")", args, nil
}

// ErrProjectScopeRequired 表示查询必须携带明确的项目作用域，禁止退化成全局查询。
var ErrProjectScopeRequired = errors.New("store: project scope is required")

// ListCandidatesScoped 返回指定项目内累计次数达到 minCount 的待处理错题簇。
// projectPath 为空时返回 ErrProjectScopeRequired，绝不回退到全局 ListCandidates。
func (s *Store) ListCandidatesScoped(ctx context.Context, projectPath string, minCount int) ([]contract.Mistake, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	projectPath = strings.TrimSpace(projectPath)
	if projectPath == "" {
		return nil, ErrProjectScopeRequired
	}
	clause, scopeArgs, err := projectPathScopeClause(projectPath)
	if err != nil {
		return nil, fmt.Errorf("store: normalize project scope: %w", err)
	}
	if minCount <= 0 {
		minCount = 2
	}
	queryArgs := append(scopeArgs, minCount)
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, project_path, fingerprint, category, summary,
		COALESCE(bad_snippet, ''), COALESCE(good_snippet, ''),
		source, status, created_at
		FROM mistakes
		WHERE `+clause+` AND count >= ? AND status = 'pending'
		ORDER BY count DESC, created_at ASC, id ASC`, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: list scoped candidates: %w", err)
	}
	defer rows.Close()

	candidates := make([]contract.Mistake, 0)
	for rows.Next() {
		var (
			m       contract.Mistake
			created any
		)
		if err := rows.Scan(
			&m.ID, &m.ProjectPath, &m.Fingerprint, &m.Category, &m.Summary,
			&m.BadSnippet, &m.GoodSnippet, &m.Source, &m.Status, &created,
		); err != nil {
			return nil, fmt.Errorf("store: scan scoped candidate: %w", err)
		}
		m.CreatedAt = coerceTime(created)
		candidates = append(candidates, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate scoped candidates: %w", err)
	}
	return candidates, nil
}

func canonicalProjectPathStrict(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("invalid NUL byte in project path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil && resolved != "" {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// pathMigrationRow 是历史 project_path 规范化的处理单元。
type pathMigrationRow struct {
	Table   string
	RowID   int64
	OldPath string
	NewPath string
	Scope   string
	Engine  string
	RuleKey string
}

func normalizeHistoricalProjectPathsTx(ctx context.Context, tx *sql.Tx) error {
	tables := []struct {
		name  string
		query string
	}{
		{"mistakes", `SELECT rowid, project_path FROM mistakes WHERE project_path <> ''`},
		{"rules", `SELECT rowid, project_path, scope, engine, LOWER(rule_key) FROM rules WHERE project_path <> ''`},
		{"scan_runs", `SELECT rowid, project_path FROM scan_runs WHERE project_path <> ''`},
		{"mcp_events", `SELECT rowid, project_path FROM mcp_events WHERE project_path <> ''`},
	}
	for _, table := range tables {
		exists, err := tableExistsTx(ctx, tx, table.name)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		rows, err := tx.QueryContext(ctx, table.query)
		if err != nil {
			return fmt.Errorf("store: scan %s for path migration: %w", table.name, err)
		}
		var items []pathMigrationRow
		for rows.Next() {
			item := pathMigrationRow{Table: table.name}
			if table.name == "rules" {
				if err := rows.Scan(&item.RowID, &item.OldPath, &item.Scope, &item.Engine, &item.RuleKey); err != nil {
					_ = rows.Close()
					return fmt.Errorf("store: scan %s row: %w", table.name, err)
				}
			} else {
				if err := rows.Scan(&item.RowID, &item.OldPath); err != nil {
					_ = rows.Close()
					return fmt.Errorf("store: scan %s row: %w", table.name, err)
				}
			}
			items = append(items, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := migratePathRowsTx(ctx, tx, items); err != nil {
			return err
		}
	}
	return nil
}

func tableExistsTx(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var name string
	err := tx.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return name != "", nil
}

func migratePathRowsTx(ctx context.Context, tx *sql.Tx, items []pathMigrationRow) error {
	type keyState struct {
		rows []int
	}
	keyGroups := map[string]*keyState{}
	for i := range items {
		canonical, err := canonicalProjectPathStrict(items[i].OldPath)
		if err != nil {
			items[i].NewPath = ""
			continue
		}
		items[i].NewPath = canonical
	}
	// 重新构造 rules 的最终唯一键：scope|engine|canonical project|lower(rule_key)。
	for i := range items {
		if items[i].Table != "rules" || items[i].NewPath == "" {
			continue
		}
		key := items[i].Scope + "\x00" + items[i].Engine + "\x00" +
			items[i].NewPath + "\x00" + strings.ToLower(items[i].RuleKey)
		group := keyGroups[key]
		if group == nil {
			group = &keyState{}
			keyGroups[key] = group
		}
		group.rows = append(group.rows, i)
	}
	conflict := make(map[int]bool)
	for _, group := range keyGroups {
		if len(group.rows) > 1 {
			for _, idx := range group.rows {
				if items[idx].OldPath != items[idx].NewPath {
					conflict[idx] = true
				}
			}
		}
	}
	for i := range items {
		item := items[i]
		status := "normalized"
		detail := ""
		if item.NewPath == "" {
			status = "unparseable"
			detail = "无法解析项目路径"
		} else if conflict[i] {
			status = "conflict"
			detail = "规范化后与现有规则唯一键冲突，保留原记录"
		} else if item.NewPath == item.OldPath {
			status = "unchanged"
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_path_migration_backup
			(table_name, row_id, old_path, backed_up_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)`,
			item.Table, fmt.Sprint(item.RowID), item.OldPath); err != nil {
			return fmt.Errorf("store: backup path row %s/%d: %w", item.Table, item.RowID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_path_migrations
			(table_name, row_id, old_path, new_path, status, detail) VALUES (?, ?, ?, ?, ?, ?)`,
			item.Table, fmt.Sprint(item.RowID), item.OldPath, item.NewPath, status, detail); err != nil {
			return fmt.Errorf("store: record path migration %s/%d: %w", item.Table, item.RowID, err)
		}
		if status != "normalized" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+item.Table+` SET project_path = ? WHERE rowid = ?`, item.NewPath, item.RowID); err != nil {
			return fmt.Errorf("store: update %s path row %d: %w", item.Table, item.RowID, err)
		}
	}
	return nil
}
