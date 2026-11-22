package reviewer

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
)

const maxHints = 15

func validProof(yaml, proof string, positives, projectHits int) bool {
	return positives > 0 && projectHits >= 0 && proof != "" && proof == core.SandboxProofHash(yaml, positives, projectHits)
}

func (r *Reviewer) ApproveProposal(ctx context.Context, project, id string) (out contract.RuleProposal, err error) {
	var rollback func() error
	err = r.Store.transaction(ctx, func(tx *sql.Tx) error {
		p, err := pendingProposal(ctx, tx, project, id)
		if err != nil {
			return err
		}
		switch p.Decision {
		case contract.HardRule:
			if !validProof(p.SemgrepYAML, p.SandboxProofHash, p.SandboxPositiveHits, p.SandboxProjectHits) {
				return fmt.Errorf("proposal sandbox proof is missing or does not match the rule")
			}
			if err := validateRuleIdentity(p.RuleID, p.SemgrepYAML); err != nil {
				return err
			}
			path, err := rulePath(project, p.RuleID)
			if err != nil {
				return err
			}
			if _, exists, err := fileMode(path + ".disabled"); err != nil {
				return err
			} else if exists {
				return fmt.Errorf("disabled rule %q already exists", p.RuleID)
			}
			if err := core.WriteFileAtomic(path, []byte(p.SemgrepYAML), 0600, true, nil); err != nil {
				return err
			}
			rollback = func() error { return os.Remove(path) }
			if err := r.Rules.ValidateRuleFile(ctx, path); err != nil {
				return err
			}
			p.AppliedPath = path
		case contract.GuardHint:
			path := hintsPath(project)
			original, count, err := loadHints(project)
			if err != nil {
				return err
			}
			if !validHint(p.HintLine) || count >= maxHints {
				return fmt.Errorf("guard hint must be one rule line within the %d-hint limit", maxHints)
			}
			mode, exists, err := fileMode(path)
			if err != nil {
				return err
			}
			lineEnd := "\n"
			if bytes.Contains(original, []byte("\r\n")) {
				lineEnd = "\r\n"
			}
			next := bytes.Clone(original)
			if len(next) != 0 && !bytes.HasSuffix(next, []byte("\n")) {
				next = append(next, lineEnd...)
			}
			next = append(next, p.HintLine+lineEnd...)
			if err := core.WriteFileAtomic(path, next, mode, !exists, nil); err != nil {
				return err
			}
			rollback = func() error {
				if exists {
					return core.WriteFileAtomic(path, original, mode, false, nil)
				}
				return os.Remove(path)
			}
			p.AppliedPath = path
		default:
			return fmt.Errorf("decision %q cannot be applied", p.Decision)
		}
		p.Status, p.UpdatedAt = contract.ProposalApproved, time.Now().UTC()
		if err := updateProposal(ctx, tx, p, false); err != nil {
			return err
		}
		out = p
		return nil
	})
	if err != nil {
		if rollback != nil {
			err = errors.Join(err, rollback())
		}
		return contract.RuleProposal{}, err
	}
	return out, nil
}

func (r *Reviewer) DismissProposal(ctx context.Context, project, id, mode string) (out contract.RuleProposal, err error) {
	var status contract.ProposalStatus
	switch mode {
	case "dismiss":
		status = contract.ProposalDismissed
	case "snooze":
		status = contract.ProposalSnoozed
	default:
		return out, fmt.Errorf("invalid mode %q; use dismiss or snooze", mode)
	}
	err = r.Store.transaction(ctx, func(tx *sql.Tx) error {
		p, err := pendingProposal(ctx, tx, project, id)
		if err != nil {
			return err
		}
		p.Status, p.UpdatedAt = status, time.Now().UTC()
		if err := updateProposal(ctx, tx, p, mode == "snooze"); err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, err
}

func pendingProposal(ctx context.Context, tx *sql.Tx, project, id string) (contract.RuleProposal, error) {
	p, err := readProposal(tx.QueryRowContext(ctx, `SELECT `+proposalFields+` FROM rule_proposals WHERE project_path=? AND id=?`, project, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return p, fmt.Errorf("proposal %q not found in project", id)
	case err != nil:
		return p, err
	case p.Status != contract.ProposalProposed:
		return p, fmt.Errorf("proposal %q has status %s; expected PROPOSED", id, p.Status)
	}
	return p, nil
}

func updateProposal(ctx context.Context, tx *sql.Tx, p contract.RuleProposal, snooze bool) error {
	res, err := tx.ExecContext(ctx, `UPDATE mistake_fingerprints SET status=?,
		snoozed_at_count=CASE WHEN ? THEN occurrence_count ELSE snoozed_at_count END WHERE project_path=? AND fingerprint=? AND status=?`,
		p.Status, snooze, p.ProjectPath, p.Fingerprint, contract.MistakeProposed)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("proposal %q has no pending fingerprint", p.ID)
	}
	_, err = tx.ExecContext(ctx, `UPDATE rule_proposals SET status=?, applied_path=?, updated_at=? WHERE id=?`,
		p.Status, p.AppliedPath, timestamp(p.UpdatedAt), p.ID)
	return err
}

func hintsPath(project string) string {
	return filepath.Join(project, core.ProjectDirectory, "hints.md")
}

func validHint(line string) bool {
	line = strings.TrimSpace(line)
	return line != "" && !strings.ContainsAny(line, "\r\n\x00") && !strings.HasPrefix(line, "#")
}

func loadHints(project string) ([]byte, int, error) {
	path := hintsPath(project)
	if err := actualDirectory(filepath.Dir(path)); err != nil {
		return nil, 0, err
	}
	if _, exists, err := fileMode(path); err != nil || !exists {
		return nil, 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if validHint(line) {
			count++
		}
	}
	return data, count, nil
}

type AppliedRule struct {
	ID      string `json:"rule_id"`
	Path    string `json:"path"`
	Enabled bool   `json:"enabled"`
}

func ListAppliedRules(project string) ([]AppliedRule, error) {
	out := []AppliedRule{}
	dir := core.RulesDir(project)
	if err := actualDirectory(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	if err := actualDirectory(dir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		if id, ok := strings.CutSuffix(name, ".yaml"); ok {
			out = append(out, AppliedRule{ID: id, Path: filepath.Join(dir, name), Enabled: true})
		} else if id, ok := strings.CutSuffix(name, ".yaml.disabled"); ok {
			out = append(out, AppliedRule{ID: id, Path: filepath.Join(dir, name)})
		}
	}
	return out, nil
}

func (r *Reviewer) ToggleRule(ctx context.Context, project, ruleID string, enabled bool) (err error) {
	return r.mutateRule(ctx, project, ruleID, func(path string, mode fs.FileMode) (string, func() error, error) {
		destination := strings.TrimSuffix(path, ".disabled")
		if !enabled {
			destination += ".disabled"
		}
		if path == destination {
			return path, nil, nil
		}
		err := os.Rename(path, destination)
		return destination, func() error { return os.Rename(destination, path) }, err
	})
}

func (r *Reviewer) DeleteAppliedRule(ctx context.Context, project, ruleID string) error {
	return r.mutateRule(ctx, project, ruleID, func(path string, mode fs.FileMode) (string, func() error, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, err
		}
		err = os.Remove(path)
		return "", func() error { return core.WriteFileAtomic(path, data, mode, true, nil) }, err
	})
}

func (r *Reviewer) mutateRule(ctx context.Context, project, id string, mutate func(string, fs.FileMode) (string, func() error, error)) error {
	var rollback func() error
	err := r.Store.transaction(ctx, func(tx *sql.Tx) error {
		active, err := rulePath(project, id)
		if err != nil {
			return err
		}
		inactive := active + ".disabled"
		mode, isActive, err := fileMode(active)
		if err != nil {
			return err
		}
		offlineMode, isInactive, err := fileMode(inactive)
		if err != nil {
			return err
		}
		path := active
		switch {
		case isActive && isInactive:
			return fmt.Errorf("both enabled and disabled copies exist for rule %q", id)
		case isInactive:
			path, mode = inactive, offlineMode
		case !isActive:
			return fmt.Errorf("rule %q not found", id)
		}
		next, undo, err := mutate(path, mode)
		if err != nil {
			return err
		}
		rollback = undo
		_, err = tx.ExecContext(ctx, `UPDATE rule_proposals SET applied_path=?, updated_at=?
			WHERE project_path=? AND rule_id=? AND decision=? AND status=? AND applied_path IN (?, ?)`,
			next, timestamp(time.Now()), project, id, contract.HardRule, contract.ProposalApproved, active, inactive)
		return err
	})
	if err != nil && rollback != nil {
		return errors.Join(err, rollback())
	}
	return err
}

func rulePath(project, id string) (string, error) {
	file := id + ".yaml"
	if id == "" || !filepath.IsLocal(file) || filepath.Base(file) != file || strings.ContainsAny(id, "/\\:\x00\r\n") {
		return "", fmt.Errorf("invalid rule file name %q", id)
	}
	dir := core.RulesDir(project)
	if err := actualDirectory(filepath.Dir(dir)); err != nil {
		return "", err
	}
	if err := actualDirectory(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, file), nil
}

func actualDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s must be a directory", path)
	}
	return nil
}

func fileMode(path string) (fs.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0600, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, false, fmt.Errorf("%s must be a regular file", path)
	}
	return info.Mode().Perm(), true, nil
}
