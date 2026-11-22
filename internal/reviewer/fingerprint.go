package reviewer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
)

var (
	quotedPath   = regexp.MustCompile(`["'](?:[A-Za-z]:[\\/]|[\\]{2}|/)[^"'\r\n]+["']`)
	absolutePath = regexp.MustCompile(`(^|[\s(=\x60])(?:[A-Za-z]:[\\/]|[\\]{2}|/)[^\s"'\x60<>|,;)]+`)
	lineNumber   = regexp.MustCompile(`(?i)(:\d+(?::\d+)?\b|\b(?:line|column|position|col)\s+\d+)`)
)

func normalizeMessage(message string) string {
	message = quotedPath.ReplaceAllString(message, "<path>")
	message = absolutePath.ReplaceAllString(message, "${1}<path>")
	message = lineNumber.ReplaceAllString(message, "<location>")
	return strings.Join(strings.Fields(message), " ")
}

func Fingerprint(d contract.Diagnostic) string {
	key := d.RuleID
	if key == "" {
		key = normalizeMessage(d.Message)
	}
	return digest(d.Language + "|" + d.Checker + "|" + key)
}

func digest(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }

type occurrence struct {
	fingerprint, language, checker, rule, message string
	count                                         int
	snippets                                      []core.Snippet
}

func (s *Store) RecordVerifyResult(ctx context.Context, project string, result contract.VerifyResult) error {
	seen := map[string]*occurrence{}
	for _, d := range result.Diagnostics {
		if d.Severity != "error" {
			continue
		}
		fp := Fingerprint(d)
		current := seen[fp]
		if current == nil {
			current = &occurrence{fingerprint: fp, language: d.Language, checker: d.Checker, rule: d.RuleID, message: normalizeMessage(d.Message)}
			seen[fp] = current
		}
		current.count++
		if d.CodeSnippet != "" {
			current.snippets = append(current.snippets, core.Snippet{Language: d.Language, File: d.File, Code: d.CodeSnippet})
			current.snippets = current.snippets[max(0, len(current.snippets)-3):]
		}
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		now := timestamp(time.Now())
		languages := slices.Clone(result.DetectedLanguages)
		if result.Scope == contract.ScopeChanged {
			var previous string
			err := tx.QueryRowContext(ctx, `SELECT languages_json FROM projects WHERE path=?`, project).Scan(&previous)
			if err == nil {
				var recorded []string
				if err := json.Unmarshal([]byte(previous), &recorded); err != nil {
					return err
				}
				for _, id := range recorded {
					if !slices.Contains(languages, id) {
						languages = append(languages, id)
					}
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		slices.Sort(languages)
		encoded, err := encodeArray(languages)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO projects(path, name, languages_json, last_status, last_scan_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(path) DO UPDATE SET
			languages_json=excluded.languages_json, last_status=excluded.last_status, last_scan_at=excluded.last_scan_at`,
			project, filepath.Base(project), encoded, result.Status, now, now)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(seen))
		for fp, item := range seen {
			if err := upsertOccurrence(ctx, tx, project, now, item); err != nil {
				return err
			}
			keys = append(keys, fp)
		}
		// json_each avoids a SQL parameter per distinct diagnostic.
		list, err := encodeArray(keys)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE mistake_fingerprints SET consecutive_failures=0
			WHERE project_path=? AND consecutive_failures<>0 AND fingerprint NOT IN (SELECT value FROM json_each(?))`, project, list)
		return err
	})
}

func upsertOccurrence(ctx context.Context, tx *sql.Tx, project, now string, item *occurrence) error {
	var snippetsJSON string
	var snippets []core.Snippet
	var count, consecutive, snoozed int
	status := contract.MistakeCollecting
	err := tx.QueryRowContext(ctx, `SELECT snippets_json, occurrence_count, consecutive_failures, status, snoozed_at_count
		FROM mistake_fingerprints WHERE project_path=? AND fingerprint=?`, project, item.fingerprint).
		Scan(&snippetsJSON, &count, &consecutive, &status, &snoozed)
	if err == nil {
		if err := json.Unmarshal([]byte(snippetsJSON), &snippets); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	snippets = append(snippets, item.snippets...)
	data, err := encodeArray(snippets[max(0, len(snippets)-3):])
	if err != nil {
		return err
	}
	count += item.count
	consecutive++
	if (status == contract.MistakeCollecting && (count >= 3 || consecutive >= 2)) ||
		(status == contract.MistakeSnoozed && count-snoozed >= 5) {
		status = contract.MistakeQueued
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mistake_fingerprints(id, project_path, fingerprint, language, checker, rule_id,
		sample_message, snippets_json, occurrence_count, consecutive_failures, status, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(project_path, fingerprint) DO UPDATE SET
		sample_message=excluded.sample_message, snippets_json=excluded.snippets_json, occurrence_count=excluded.occurrence_count,
		consecutive_failures=excluded.consecutive_failures, status=excluded.status, last_seen_at=excluded.last_seen_at`,
		digest(project+"\x00"+item.fingerprint), project, item.fingerprint, item.language, item.checker, item.rule,
		item.message, data, count, consecutive, status, now)
	return err
}
