package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 规则/错题状态相关的可识别错误。
var (
	ErrRuleNotFound    = errors.New("store: rule not found")
	ErrMistakeNotFound = errors.New("store: mistake not found")
	ErrInvalidStatus   = errors.New("store: invalid mistake status")
)

// Rule 是已沉淀规则的台账记录。
// RuleID 对应数据库 rules.rule_key，与 architecture 文档的规则标识一致。
type Rule struct {
	ID          string    `json:"id"`
	Scope       string    `json:"scope"` // global | project
	ProjectPath string    `json:"project_path,omitempty"`
	Engine      string    `json:"engine"` // eslint | ruff | semgrep
	RuleID      string    `json:"rule_id"`
	Payload     string    `json:"payload"`
	Rationale   string    `json:"rationale,omitempty"`
	Source      string    `json:"source,omitempty"` // manual | agent | git_fix
	Status      string    `json:"status"`           // active | disabled
	HitCount    int       `json:"hit_count"`
	CreatedAt   time.Time `json:"created_at"`
	AppliedAt   time.Time `json:"applied_at,omitempty"`
}

// InsertRule 新增一条规则台账记录。
func (s *Store) InsertRule(ctx context.Context, rule Rule) error {
	if s == nil || s.db == nil {
		return errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rule = normalizeRule(rule)
	if rule.Engine == "" {
		return errors.New("store: rule engine is required")
	}
	if rule.RuleID == "" {
		return errors.New("store: rule id is required")
	}
	if rule.ID == "" {
		rule.ID = newID("rule")
	}
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = time.Now().UTC()
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO rules
		(id, scope, project_path, engine, rule_key, payload, rationale, hit_count, source, status, applied_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rule.ID, rule.Scope, rule.ProjectPath, rule.Engine, rule.RuleID, rule.Payload,
		rule.Rationale, rule.HitCount, rule.Source, rule.Status, nullableTime(rule.AppliedAt),
		rule.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("store: insert rule: %w", err)
	}
	return nil
}

// UpdateRule 按 ID 更新规则台账；不存在时返回 ErrRuleNotFound。
func (s *Store) UpdateRule(ctx context.Context, rule Rule) error {
	if s == nil || s.db == nil {
		return errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rule = normalizeRule(rule)
	if strings.TrimSpace(rule.ID) == "" {
		return errors.New("store: rule id is required for update")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE rules SET
		scope = ?, project_path = ?, engine = ?, rule_key = ?, payload = ?, rationale = ?,
		hit_count = ?, source = ?, status = ?, applied_at = ?
		WHERE id = ?`,
		rule.Scope, rule.ProjectPath, rule.Engine, rule.RuleID, rule.Payload, rule.Rationale,
		rule.HitCount, rule.Source, rule.Status, nullableTime(rule.AppliedAt), rule.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update rule: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("store: update rule rows: %w", err)
	} else if affected == 0 {
		return ErrRuleNotFound
	}
	return nil
}

// GetRule 按 ID 查询规则台账。
func (s *Store) GetRule(ctx context.Context, id string) (Rule, error) {
	if s == nil || s.db == nil {
		return Rule{}, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	row := s.db.QueryRowContext(ctx, `SELECT
		id, scope, project_path, engine, rule_key, payload, rationale, hit_count,
		source, status, applied_at, created_at
		FROM rules WHERE id = ?`, strings.TrimSpace(id))
	rule, err := scanRule(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Rule{}, ErrRuleNotFound
		}
		return Rule{}, fmt.Errorf("store: get rule: %w", err)
	}
	return rule, nil
}

// ListRules 返回规则台账；engine 为空时返回全部引擎，否则按引擎过滤。
func (s *Store) ListRules(ctx context.Context, engine string) ([]Rule, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store: not opened")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	engine = strings.ToLower(strings.TrimSpace(engine))
	query := `SELECT id, scope, project_path, engine, rule_key, payload, rationale, hit_count,
		source, status, applied_at, created_at FROM rules`
	var args []any
	if engine != "" {
		query += ` WHERE engine = ?`
		args = append(args, engine)
	}
	query += ` ORDER BY created_at DESC, id ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list rules: %w", err)
	}
	defer rows.Close()
	rules := make([]Rule, 0)
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan rule: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate rules: %w", err)
	}
	return rules, nil
}

// UpdateMistakeStatus 按 fingerprint 更新错题状态（pending -> promoted / ignored），并同步 review_status。
func (s *Store) UpdateMistakeStatus(ctx context.Context, fingerprint, status string) error {
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
	status = strings.ToLower(strings.TrimSpace(status))
	var updateSQL string
	switch status {
	case "promoted":
		updateSQL = `UPDATE mistakes SET status = 'promoted', review_status = 'approved' WHERE fingerprint = ?`
	case "ignored":
		updateSQL = `UPDATE mistakes SET status = 'ignored', review_status = 'ignored' WHERE fingerprint = ?`
	case "pending":
		updateSQL = `UPDATE mistakes SET status = 'pending', review_status = CASE WHEN ai_decision != '' OR ai_analyzed_at IS NOT NULL THEN 'pending_user' ELSE 'pending_ai' END WHERE fingerprint = ?`
	default:
		return fmt.Errorf("%w: %q", ErrInvalidStatus, status)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, updateSQL, fingerprint)
	if err != nil {
		return fmt.Errorf("store: update mistake status: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("store: update mistake status rows: %w", err)
	} else if affected == 0 {
		return ErrMistakeNotFound
	}
	return nil
}

// IgnoreMistake 忽略指定错题，同步将 status 与 review_status 置为 ignored。
func (s *Store) IgnoreMistake(ctx context.Context, fingerprint string) error {
	return s.UpdateMistakeStatus(ctx, fingerprint, "ignored")
}

// RestoreMistake 恢复指定错题为 pending（若已有 AI 分析则 review_status 为 pending_user，否则为 pending_ai）。
func (s *Store) RestoreMistake(ctx context.Context, fingerprint string) error {
	return s.UpdateMistakeStatus(ctx, fingerprint, "pending")
}

func normalizeRule(rule Rule) Rule {
	rule.ID = strings.TrimSpace(rule.ID)
	rule.Scope = strings.TrimSpace(rule.Scope)
	if rule.Scope == "" {
		rule.Scope = "project"
	}
	rule.ProjectPath = strings.TrimSpace(rule.ProjectPath)
	rule.Engine = strings.ToLower(strings.TrimSpace(rule.Engine))
	rule.RuleID = strings.TrimSpace(rule.RuleID)
	rule.Source = strings.TrimSpace(rule.Source)
	if rule.Source == "" {
		rule.Source = "manual"
	}
	rule.Status = strings.TrimSpace(rule.Status)
	if rule.Status == "" {
		rule.Status = "active"
	}
	if rule.HitCount < 0 {
		rule.HitCount = 0
	}
	return rule
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRule(row rowScanner) (Rule, error) {
	var (
		rule    Rule
		applied any
		created any
	)
	if err := row.Scan(
		&rule.ID, &rule.Scope, &rule.ProjectPath, &rule.Engine, &rule.RuleID,
		&rule.Payload, &rule.Rationale, &rule.HitCount, &rule.Source, &rule.Status,
		&applied, &created,
	); err != nil {
		return Rule{}, err
	}
	rule.AppliedAt = coerceTime(applied)
	rule.CreatedAt = coerceTime(created)
	return rule, nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}
