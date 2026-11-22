package reviewer

import (
	"context"
	"encoding/json"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
)

// RegisteredProjects returns the same registrations used by the CLI and MCP.
func (s *Store) RegisteredProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, name, languages_json, last_status, last_scan_at, hook_enabled
		FROM projects ORDER BY name COLLATE NOCASE, path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		p := Project{Languages: []string{}}
		var languages, scan string
		if err := rows.Scan(&p.Path, &p.Name, &languages, &p.LastStatus, &scan, &p.HookEnabled); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(languages), &p.Languages); err != nil {
			return nil, err
		}
		if scan != "" {
			parsed, err := time.Parse(time.RFC3339Nano, scan)
			if err != nil {
				return nil, err
			}
			p.LastScanAt = &parsed
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// LatestDiagnosticSummaries reads only fingerprints seen in the last recorded scan.
// The shared store does not retain full per-scan diagnostics or missing tools.
func (s *Store) LatestDiagnosticSummaries(ctx context.Context, project string) ([]contract.Diagnostic, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.language, f.checker, f.rule_id, f.sample_message, f.snippets_json
		FROM mistake_fingerprints f JOIN projects p ON p.path = f.project_path
		WHERE p.path = ? AND f.last_seen_at = p.last_scan_at ORDER BY f.checker, f.rule_id, f.id LIMIT 20`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	diagnostics := []contract.Diagnostic{}
	for rows.Next() {
		d := contract.Diagnostic{Severity: "error"}
		var serialized string
		if err := rows.Scan(&d.Language, &d.Checker, &d.RuleID, &d.Message, &serialized); err != nil {
			return nil, err
		}
		var samples []core.Snippet
		if err := json.Unmarshal([]byte(serialized), &samples); err != nil {
			return nil, err
		}
		if len(samples) != 0 {
			d.File = samples[len(samples)-1].File
		}
		diagnostics = append(diagnostics, d)
	}
	return diagnostics, rows.Err()
}

// PendingProposalIDs returns project paths and ordered proposal IDs without loading rule text.
func (s *Store) PendingProposalIDs(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_path, id FROM rule_proposals WHERE status=? ORDER BY id`, contract.ProposalProposed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string][]string)
	for rows.Next() {
		var project, id string
		if err := rows.Scan(&project, &id); err != nil {
			return nil, err
		}
		ids[project] = append(ids[project], id)
	}
	return ids, rows.Err()
}
