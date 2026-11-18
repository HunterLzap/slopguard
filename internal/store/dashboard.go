package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// Overview 是看板效果总览的聚合数据。
type Overview struct {
	ProjectPath   string       `json:"project_path,omitempty"`
	TotalRuns     int64        `json:"total_runs"`
	FailedRuns    int64        `json:"failed_runs"`
	RuleCount     int64        `json:"rule_count"`
	AvgDurationMs float64      `json:"avg_duration_ms"`
	Trend         []TrendPoint `json:"trend"`
}

// TrendPoint 是某个时间桶内的 verify 趋势。
type TrendPoint struct {
	Bucket        string  `json:"bucket"`
	Runs          int64   `json:"runs"`
	Passed        int64   `json:"passed"`
	Failed        int64   `json:"failed"`
	Escapes       int64   `json:"escapes"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

// MistakeRecord 是看板错题本中的一条错题（含累计次数）。
type MistakeRecord struct {
	contract.Mistake
	Count int `json:"count"`
}

// RuleLedgerQuery 是规则台账查询条件。
type RuleLedgerQuery struct {
	ProjectPath string `json:"project_path,omitempty"`
	Engine      string `json:"engine,omitempty"`
	Scope       string `json:"scope,omitempty"`
	Status      string `json:"status,omitempty"` // 空 = active；all = 不过滤
}

// ProjectInfo 是看板项目选择器中的项目摘要。
type ProjectInfo struct {
	ProjectPath string    `json:"project_path"`
	ScanCount   int64     `json:"scan_count"`
	LastScanAt  time.Time `json:"last_scan_at,omitempty"`
}

// OverviewStats 返回看板效果总览。
// projectPath 为空时统计全部项目；granularity 支持 day/week（默认 day）。
func (s *Store) OverviewStats(ctx context.Context, projectPath, granularity string) (Overview, error) {
	if s == nil || s.db == nil {
		return Overview{}, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	projectPath = strings.TrimSpace(projectPath)
	bucketExpr, err := trendBucketExpr(granularity)
	if err != nil {
		return Overview{}, err
	}
	overview := Overview{ProjectPath: projectPath, Trend: []TrendPoint{}}

	where := ""
	var args []any
	if projectPath != "" {
		where = " WHERE project_path = ?"
		args = append(args, projectPath)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN passed = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(AVG(duration_ms), 0)
		FROM scan_runs`+where, args...,
	).Scan(&overview.TotalRuns, &overview.FailedRuns, &overview.AvgDurationMs); err != nil {
		return Overview{}, fmt.Errorf("store: overview totals: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, ruleCountSQL(projectPath), ruleCountArgs(projectPath)...).Scan(&overview.RuleCount); err != nil {
		return Overview{}, fmt.Errorf("store: overview rule count: %w", err)
	}

	trendSQL := `SELECT ` + bucketExpr + ` AS bucket,
		COUNT(*),
		COALESCE(SUM(CASE WHEN passed = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN passed = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(escapes_cnt), 0),
		COALESCE(AVG(duration_ms), 0)
		FROM scan_runs` + where + ` GROUP BY bucket ORDER BY bucket ASC`
	rows, err := s.db.QueryContext(ctx, trendSQL, args...)
	if err != nil {
		return Overview{}, fmt.Errorf("store: overview trend: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var point TrendPoint
		if err := rows.Scan(&point.Bucket, &point.Runs, &point.Passed, &point.Failed, &point.Escapes, &point.AvgDurationMs); err != nil {
			return Overview{}, fmt.Errorf("store: scan overview trend: %w", err)
		}
		overview.Trend = append(overview.Trend, point)
	}
	if err := rows.Err(); err != nil {
		return Overview{}, fmt.Errorf("store: iterate overview trend: %w", err)
	}
	return overview, nil
}

func trendBucketExpr(granularity string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(granularity)) {
	case "", "day", "daily":
		return `substr(created_at, 1, 10)`, nil
	case "week", "weekly":
		return `strftime('%Y-W%W', created_at)`, nil
	default:
		return "", fmt.Errorf("store: unsupported trend granularity %q (want day or week)", granularity)
	}
}

func ruleCountSQL(projectPath string) string {
	if strings.TrimSpace(projectPath) == "" {
		return `SELECT COUNT(*) FROM rules WHERE status != 'disabled'`
	}
	return `SELECT COUNT(*) FROM rules WHERE status != 'disabled' AND (scope = 'global' OR project_path = ?)`
}

func ruleCountArgs(projectPath string) []any {
	if strings.TrimSpace(projectPath) == "" {
		return nil
	}
	return []any{strings.TrimSpace(projectPath)}
}

// ListMistakes 返回看板错题本列表，按累计次数降序。
// status 为空时返回全部状态；projectPath 为空时返回全部项目。
func (s *Store) ListMistakes(ctx context.Context, status, projectPath string) ([]MistakeRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "", "pending", "promoted", "ignored":
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, status)
	}
	projectPath = strings.TrimSpace(projectPath)
	query := `SELECT id, project_path, fingerprint, category, summary,
		COALESCE(bad_snippet, ''), COALESCE(good_snippet, ''),
		source, status, count, created_at
		FROM mistakes`
	var conditions []string
	var args []any
	if status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, status)
	}
	if projectPath != "" {
		conditions = append(conditions, "project_path = ?")
		args = append(args, projectPath)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY count DESC, created_at ASC, id ASC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list mistakes: %w", err)
	}
	defer rows.Close()
	records := make([]MistakeRecord, 0)
	for rows.Next() {
		var (
			record  MistakeRecord
			created any
		)
		if err := rows.Scan(
			&record.ID, &record.ProjectPath, &record.Fingerprint, &record.Category, &record.Summary,
			&record.BadSnippet, &record.GoodSnippet, &record.Source, &record.Status,
			&record.Count, &created,
		); err != nil {
			return nil, fmt.Errorf("store: scan mistake: %w", err)
		}
		record.CreatedAt = coerceTime(created)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate mistakes: %w", err)
	}
	return records, nil
}

// ListRuleLedger 返回看板规则台账。
// status 为空时只返回 active；传 all 时返回全部状态。
func (s *Store) ListRuleLedger(ctx context.Context, query RuleLedgerQuery) ([]Rule, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	status := strings.ToLower(strings.TrimSpace(query.Status))
	if status == "" {
		status = "active"
	}
	var conditions []string
	var args []any
	if status != "all" {
		conditions = append(conditions, "status = ?")
		args = append(args, status)
	}
	if engine := strings.ToLower(strings.TrimSpace(query.Engine)); engine != "" {
		conditions = append(conditions, "engine = ?")
		args = append(args, engine)
	}
	scope := strings.ToLower(strings.TrimSpace(query.Scope))
	if scope != "" {
		conditions = append(conditions, "scope = ?")
		args = append(args, scope)
	}
	if projectPath := strings.TrimSpace(query.ProjectPath); projectPath != "" {
		conditions = append(conditions, "(scope = 'global' OR project_path = ?)")
		args = append(args, projectPath)
	}
	sql := `SELECT id, scope, project_path, engine, rule_key, payload, rationale, hit_count,
		source, status, applied_at, created_at FROM rules`
	if len(conditions) > 0 {
		sql += " WHERE " + strings.Join(conditions, " AND ")
	}
	sql += " ORDER BY created_at DESC, id ASC"
	rows, err := s.db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list rule ledger: %w", err)
	}
	defer rows.Close()
	rules := make([]Rule, 0)
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan rule ledger: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate rule ledger: %w", err)
	}
	return rules, nil
}

// ListProjects 返回已接入项目（有 scan_runs 或 mistakes 的项目），按最近活动时间降序。
func (s *Store) ListProjects(ctx context.Context) ([]ProjectInfo, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.project_path,
		COALESCE(s.scan_count, 0),
		s.last_scan_at
		FROM (
			SELECT project_path FROM scan_runs WHERE project_path <> ''
			UNION
			SELECT project_path FROM mistakes WHERE project_path <> ''
		) AS p
		LEFT JOIN (
			SELECT project_path, COUNT(*) AS scan_count, MAX(created_at) AS last_scan_at
			FROM scan_runs
			WHERE project_path <> ''
			GROUP BY project_path
		) AS s ON s.project_path = p.project_path
		ORDER BY (s.last_scan_at IS NULL) ASC, s.last_scan_at DESC, p.project_path ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list projects: %w", err)
	}
	defer rows.Close()
	projects := make([]ProjectInfo, 0)
	for rows.Next() {
		var (
			project ProjectInfo
			last    any
		)
		if err := rows.Scan(&project.ProjectPath, &project.ScanCount, &last); err != nil {
			return nil, fmt.Errorf("store: scan project: %w", err)
		}
		project.LastScanAt = coerceTime(last)
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate projects: %w", err)
	}
	return projects, nil
}
