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
	ProjectPath   string         `json:"project_path,omitempty"`
	TotalRuns     int64          `json:"total_runs"`
	FailedRuns    int64          `json:"failed_runs"`
	RuleCount     int64          `json:"rule_count"`
	AvgDurationMs float64        `json:"avg_duration_ms"`
	Totals        OverviewTotals `json:"totals"`
	Trend         []TrendPoint   `json:"trend"`
	RecentRuns    []ScanRun      `json:"recent_runs"`
}

// OverviewTotals 是看板效果总览的汇总指标。
type OverviewTotals struct {
	TotalRuns         int64   `json:"total_runs"`
	PassedRuns        int64   `json:"passed_runs"`
	FailedRuns        int64   `json:"failed_runs"`
	PassRate          float64 `json:"pass_rate"`
	RuleCount         int64   `json:"rule_count"`
	AvgDurationMs     float64 `json:"avg_duration_ms"`
	LatestEscapes     int64   `json:"latest_escapes"`
	PendingMistakes   int64   `json:"pending_mistakes"`
	CandidateMistakes int64   `json:"candidate_mistakes"`
}

// ScanRun 是单次 verify 扫描运行记录。
type ScanRun struct {
	ID          string    `json:"id"`
	ProjectPath string    `json:"project_path"`
	DurationMs  int64     `json:"duration_ms"`
	EscapesCnt  int64     `json:"escapes_cnt"`
	Passed      bool      `json:"passed"`
	CreatedAt   time.Time `json:"created_at"`
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

// MistakeRecord 是看板错题本中的一条错题（含累计次数与 AI 分析状态）。
type MistakeRecord struct {
	contract.Mistake
	Count        int       `json:"count"`
	IsCandidate  bool      `json:"is_candidate"`
	UpdatedAt    time.Time `json:"updated_at"`
	AIDecision   string    `json:"ai_decision"`
	AIReason     string    `json:"ai_reason"`
	AIRuleDraft  string    `json:"ai_rule_draft"`
	AIAnalyzedAt time.Time `json:"ai_analyzed_at,omitempty"`
	ReviewStatus string    `json:"review_status"`
}

// MistakeQuery 是看板错题本查询条件。
type MistakeQuery struct {
	ProjectPath  string `json:"project_path,omitempty"`
	Status       string `json:"status,omitempty"`
	ReviewStatus string `json:"review_status,omitempty"`
	Category     string `json:"category,omitempty"`
	Source       string `json:"source,omitempty"`
	Q            string `json:"q,omitempty"`
}

// MCPEvent 是 MCP 工具调用审计事件。
type MCPEvent struct {
	ID            string    `json:"id"`
	ProjectPath   string    `json:"project_path"`
	ToolName      string    `json:"tool_name"`
	ArgsSummary   string    `json:"args_summary"`
	Passed        bool      `json:"passed"`
	FindingsCount int       `json:"findings_count"`
	DurationMs    int64     `json:"duration_ms"`
	ErrorText     string    `json:"error_text,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
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
	ProjectPath     string    `json:"project_path"`
	ScanCount       int64     `json:"scan_count"`
	PassedCount     int64     `json:"passed_count"`
	PassRate        float64   `json:"pass_rate"`
	LatestEscapes   int64     `json:"latest_escapes"`
	PendingMistakes int64     `json:"pending_mistakes"`
	LastScanAt      time.Time `json:"last_scan_at,omitempty"`
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
	overview := Overview{
		ProjectPath: projectPath,
		Trend:       []TrendPoint{},
		RecentRuns:  []ScanRun{},
	}

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

	runsSQL := `SELECT id, project_path, duration_ms, escapes_cnt, passed, created_at
		FROM scan_runs` + where + ` ORDER BY created_at DESC, id DESC LIMIT 30`
	runRows, err := s.db.QueryContext(ctx, runsSQL, args...)
	if err != nil {
		return Overview{}, fmt.Errorf("store: overview recent runs: %w", err)
	}
	defer runRows.Close()
	for runRows.Next() {
		var (
			run       ScanRun
			passedInt int
			created   any
		)
		if err := runRows.Scan(&run.ID, &run.ProjectPath, &run.DurationMs, &run.EscapesCnt, &passedInt, &created); err != nil {
			return Overview{}, fmt.Errorf("store: scan recent run: %w", err)
		}
		run.Passed = passedInt != 0
		run.CreatedAt = coerceTime(created)
		overview.RecentRuns = append(overview.RecentRuns, run)
	}
	if err := runRows.Err(); err != nil {
		return Overview{}, fmt.Errorf("store: iterate recent runs: %w", err)
	}

	var pendingMistakes, candidateMistakes int64
	mistakeWhere := " WHERE status = 'pending'"
	var mistakeArgs []any
	if projectPath != "" {
		mistakeWhere += " AND project_path = ?"
		mistakeArgs = append(mistakeArgs, projectPath)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN count >= 2 THEN 1 ELSE 0 END), 0)
		FROM mistakes`+mistakeWhere, mistakeArgs...,
	).Scan(&pendingMistakes, &candidateMistakes); err != nil {
		return Overview{}, fmt.Errorf("store: overview mistakes: %w", err)
	}

	passedRuns := overview.TotalRuns - overview.FailedRuns
	passRate := 0.0
	if overview.TotalRuns > 0 {
		passRate = float64(passedRuns) * 100.0 / float64(overview.TotalRuns)
	}
	var latestEscapes int64
	if len(overview.RecentRuns) > 0 {
		latestEscapes = overview.RecentRuns[0].EscapesCnt
	}
	overview.Totals = OverviewTotals{
		TotalRuns:         overview.TotalRuns,
		PassedRuns:        passedRuns,
		FailedRuns:        overview.FailedRuns,
		PassRate:          passRate,
		RuleCount:         overview.RuleCount,
		AvgDurationMs:     overview.AvgDurationMs,
		LatestEscapes:     latestEscapes,
		PendingMistakes:   pendingMistakes,
		CandidateMistakes: candidateMistakes,
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
	return s.QueryMistakes(ctx, MistakeQuery{
		Status:      status,
		ProjectPath: projectPath,
	})
}

// QueryMistakes 按多维条件查询看板错题本，按累计次数降序。
func (s *Store) QueryMistakes(ctx context.Context, query MistakeQuery) ([]MistakeRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	status := strings.ToLower(strings.TrimSpace(query.Status))
	switch status {
	case "", "pending", "promoted", "ignored":
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, status)
	}
	reviewStatus := strings.ToLower(strings.TrimSpace(query.ReviewStatus))
	switch reviewStatus {
	case "", "pending_ai", "pending_user", "approved", "ignored":
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, reviewStatus)
	}
	projectPath := strings.TrimSpace(query.ProjectPath)
	category := strings.ToLower(strings.TrimSpace(query.Category))
	source := strings.ToLower(strings.TrimSpace(query.Source))
	q := strings.TrimSpace(query.Q)

	sqlQuery := `SELECT id, project_path, fingerprint, category, summary,
		COALESCE(bad_snippet, ''), COALESCE(good_snippet, ''),
		source, status, count, created_at,
		COALESCE(ai_decision, ''), COALESCE(ai_reason, ''), COALESCE(ai_rule_draft, ''),
		ai_analyzed_at, COALESCE(review_status, 'pending_ai')
		FROM mistakes`
	var conditions []string
	var args []any
	if status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, status)
	}
	if reviewStatus != "" {
		conditions = append(conditions, "review_status = ?")
		args = append(args, reviewStatus)
	}
	if projectPath != "" {
		conditions = append(conditions, "project_path = ?")
		args = append(args, projectPath)
	}
	if category != "" {
		conditions = append(conditions, "LOWER(category) = ?")
		args = append(args, category)
	}
	if source != "" {
		conditions = append(conditions, "LOWER(source) = ?")
		args = append(args, source)
	}
	if q != "" {
		pattern := "%" + strings.ToLower(q) + "%"
		conditions = append(conditions, `(LOWER(fingerprint) LIKE ? OR LOWER(summary) LIKE ? OR LOWER(category) LIKE ? OR LOWER(COALESCE(bad_snippet, '')) LIKE ? OR LOWER(COALESCE(good_snippet, '')) LIKE ?)`)
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}
	if len(conditions) > 0 {
		sqlQuery += " WHERE " + strings.Join(conditions, " AND ")
	}
	sqlQuery += " ORDER BY count DESC, created_at ASC, id ASC"
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list mistakes: %w", err)
	}
	defer rows.Close()
	records := make([]MistakeRecord, 0)
	for rows.Next() {
		var (
			record     MistakeRecord
			created    any
			aiAnalyzed any
		)
		if err := rows.Scan(
			&record.ID, &record.ProjectPath, &record.Fingerprint, &record.Category, &record.Summary,
			&record.BadSnippet, &record.GoodSnippet, &record.Source, &record.Status,
			&record.Count, &created,
			&record.AIDecision, &record.AIReason, &record.AIRuleDraft,
			&aiAnalyzed, &record.ReviewStatus,
		); err != nil {
			return nil, fmt.Errorf("store: scan mistake: %w", err)
		}
		record.CreatedAt = coerceTime(created)
		record.AIAnalyzedAt = coerceTime(aiAnalyzed)
		record.UpdatedAt = record.CreatedAt
		record.IsCandidate = record.Count >= 2 && record.Status == "pending"
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
		COALESCE(s.passed_count, 0),
		COALESCE((
			SELECT sr.escapes_cnt FROM scan_runs sr
			WHERE sr.project_path = p.project_path
			ORDER BY sr.created_at DESC, sr.id DESC
			LIMIT 1
		), 0),
		COALESCE(m.pending_mistakes, 0),
		s.last_scan_at
		FROM (
			SELECT project_path FROM scan_runs WHERE project_path <> ''
			UNION
			SELECT project_path FROM mistakes WHERE project_path <> ''
		) AS p
		LEFT JOIN (
			SELECT project_path,
				COUNT(*) AS scan_count,
				COALESCE(SUM(CASE WHEN passed = 1 THEN 1 ELSE 0 END), 0) AS passed_count,
				MAX(created_at) AS last_scan_at
			FROM scan_runs
			WHERE project_path <> ''
			GROUP BY project_path
		) AS s ON s.project_path = p.project_path
		LEFT JOIN (
			SELECT project_path, COUNT(*) AS pending_mistakes
			FROM mistakes
			WHERE project_path <> '' AND status = 'pending'
			GROUP BY project_path
		) AS m ON m.project_path = p.project_path
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
		if err := rows.Scan(
			&project.ProjectPath,
			&project.ScanCount,
			&project.PassedCount,
			&project.LatestEscapes,
			&project.PendingMistakes,
			&last,
		); err != nil {
			return nil, fmt.Errorf("store: scan project: %w", err)
		}
		if project.ScanCount > 0 {
			project.PassRate = float64(project.PassedCount) * 100.0 / float64(project.ScanCount)
		}
		project.LastScanAt = coerceTime(last)
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate projects: %w", err)
	}
	return projects, nil
}

// RecordMCPEvent 记录一次 MCP 工具调用审计事件。
func (s *Store) RecordMCPEvent(ctx context.Context, event MCPEvent) error {
	if s == nil || s.db == nil {
		return errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	event.ProjectPath = strings.TrimSpace(event.ProjectPath)
	event.ToolName = strings.TrimSpace(event.ToolName)
	if event.ToolName == "" {
		return errors.New("store: mcp event tool name is required")
	}
	if event.ID == "" {
		event.ID = newID("evt")
	}
	if event.DurationMs < 0 {
		event.DurationMs = 0
	}
	if event.FindingsCount < 0 {
		event.FindingsCount = 0
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	passedInt := 0
	if event.Passed {
		passedInt = 1
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO mcp_events
		(id, project_path, tool_name, args_summary, passed, findings_count, duration_ms, error_text, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.ProjectPath, event.ToolName, event.ArgsSummary, passedInt,
		event.FindingsCount, event.DurationMs, event.ErrorText,
		event.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("store: record mcp event: %w", err)
	}
	return nil
}

// ListMCPEvents 返回最近的 MCP 工具调用审计事件，按时间倒序。
func (s *Store) ListMCPEvents(ctx context.Context, projectPath, toolName string, limit int) ([]MCPEvent, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 {
		limit = 50
	}
	projectPath = strings.TrimSpace(projectPath)
	toolName = strings.TrimSpace(toolName)

	sqlQuery := `SELECT id, project_path, tool_name, COALESCE(args_summary, ''),
		passed, findings_count, duration_ms, COALESCE(error_text, ''), created_at
		FROM mcp_events`
	var conditions []string
	var args []any
	if projectPath != "" {
		conditions = append(conditions, "project_path = ?")
		args = append(args, projectPath)
	}
	if toolName != "" {
		conditions = append(conditions, "tool_name = ?")
		args = append(args, toolName)
	}
	if len(conditions) > 0 {
		sqlQuery += " WHERE " + strings.Join(conditions, " AND ")
	}
	sqlQuery += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list mcp events: %w", err)
	}
	defer rows.Close()

	events := make([]MCPEvent, 0)
	for rows.Next() {
		var (
			ev        MCPEvent
			passedInt int
			created   any
		)
		if err := rows.Scan(
			&ev.ID, &ev.ProjectPath, &ev.ToolName, &ev.ArgsSummary,
			&passedInt, &ev.FindingsCount, &ev.DurationMs, &ev.ErrorText, &created,
		); err != nil {
			return nil, fmt.Errorf("store: scan mcp event: %w", err)
		}
		ev.Passed = passedInt != 0
		ev.CreatedAt = coerceTime(created)
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate mcp events: %w", err)
	}
	return events, nil
}

// ListInboxMistakes 返回待审查错题箱（review_status 为 pending_ai / pending_user，或指定值）。
func (s *Store) ListInboxMistakes(ctx context.Context, projectPath, reviewStatus string) ([]MistakeRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reviewStatus = strings.ToLower(strings.TrimSpace(reviewStatus))
	if reviewStatus != "" {
		return s.QueryMistakes(ctx, MistakeQuery{
			ProjectPath:  projectPath,
			ReviewStatus: reviewStatus,
		})
	}
	all, err := s.QueryMistakes(ctx, MistakeQuery{
		ProjectPath: projectPath,
	})
	if err != nil {
		return nil, err
	}
	inbox := make([]MistakeRecord, 0, len(all))
	for _, rec := range all {
		if rec.ReviewStatus == "pending_ai" || rec.ReviewStatus == "pending_user" {
			inbox = append(inbox, rec)
		}
	}
	return inbox, nil
}

// SubmitMistakeAnalysis 保存宿主 Agent 对错题的中文分析与入库建议，并将 review_status 置为 pending_user。
func (s *Store) SubmitMistakeAnalysis(ctx context.Context, fingerprint, decision, reason, ruleDraft string) error {
	if s == nil || s.db == nil {
		return errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return errors.New("store: fingerprint is required")
	}
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision != "recommend" && decision != "skip" {
		return fmt.Errorf("store: invalid ai decision %q (want recommend or skip)", decision)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("store: ai reason is required")
	}
	ruleDraft = strings.TrimSpace(ruleDraft)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE mistakes SET
		ai_decision = ?,
		ai_reason = ?,
		ai_rule_draft = ?,
		ai_analyzed_at = ?,
		review_status = 'pending_user'
		WHERE fingerprint = ?`,
		decision, reason, ruleDraft, time.Now().UTC().Format(time.RFC3339Nano), fingerprint,
	)
	if err != nil {
		return fmt.Errorf("store: submit mistake analysis: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("store: submit mistake analysis rows: %w", err)
	} else if affected == 0 {
		return ErrMistakeNotFound
	}
	return nil
}

// ApproveMistake 确认错题入库，将 review_status 置为 approved、status 置为 promoted。
func (s *Store) ApproveMistake(ctx context.Context, fingerprint string) error {
	return s.UpdateMistakeStatus(ctx, fingerprint, "promoted")
}
