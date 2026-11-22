package reviewer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
)

const reviewDeadline = 5 * time.Minute

type RuleValidator interface {
	ValidateRuleFile(ctx context.Context, path string) error
	VerifyRuleInSandbox(ctx context.Context, project, ruleYAML string, samples []core.Snippet) (core.SandboxReport, error)
}

type Reviewer struct {
	Store *Store
	Rules RuleValidator
	HTTP  HTTPClient
	mu    sync.Mutex
}

type queued struct {
	Project     string         `json:"-"`
	Fingerprint string         `json:"-"`
	Language    string         `json:"language"`
	Checker     string         `json:"checker"`
	RuleID      string         `json:"rule_id"`
	Message     string         `json:"message"`
	Snippets    []core.Snippet `json:"snippets"`
	Occurrences int            `json:"occurrence_count"`
	token       string
}

func (r *Reviewer) ProcessQueueOnce(ctx context.Context) (processed bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	settings, err := r.Store.Settings(ctx)
	if err != nil || strings.TrimSpace(settings.APIKey) == "" {
		return false, err
	}
	item, err := r.Store.claim(ctx)
	if err != nil || item.token == "" {
		return false, err
	}
	// A canceled CLI or MCP request must release the durable claim as well.
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err != nil {
			err = errors.Join(err, r.Store.finish(releaseCtx, item, nil))
		}
	}()
	reviewCtx, cancel := context.WithTimeout(ctx, reviewDeadline)
	defer cancel()
	proposal, err := r.review(reviewCtx, Client{HTTP: r.HTTP, Settings: settings}, item)
	if err != nil {
		return false, err
	}
	if err := r.Store.finish(reviewCtx, item, &proposal); err != nil {
		return false, err
	}
	return true, nil
}

// RunQueue consumes the queue serially while workers signal that new scans arrived.
func (r *Reviewer) RunQueue(ctx context.Context, wake <-chan struct{}, report func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-wake:
			if !ok {
				return
			}
		}
		for ctx.Err() == nil {
			processed, err := r.ProcessQueueOnce(ctx)
			if err != nil {
				if ctx.Err() == nil {
					report(err)
				}
				break
			}
			if !processed {
				break
			}
		}
	}
}

func (r *Reviewer) review(ctx context.Context, client Client, item queued) (contract.RuleProposal, error) {
	sample, err := json.Marshal(item)
	if err != nil {
		return contract.RuleProposal{}, err
	}
	messages := []message{{Role: "system", Content: reviewInstruction}, {Role: "user", Content: string(sample)}}
	answer, err := client.complete(ctx, messages)
	if err != nil {
		return contract.RuleProposal{}, err
	}
	decision, err := parseDecision(answer)
	if err != nil {
		return contract.RuleProposal{}, fmt.Errorf("parse reviewer decision: %w", err)
	}
	proposal := contract.RuleProposal{ID: item.token, ProjectPath: item.Project, Fingerprint: item.Fingerprint, Status: contract.ProposalProposed}
	fill := func() {
		proposal.Decision, proposal.SummaryZH, proposal.RuleID, proposal.SemgrepYAML, proposal.HintLine =
			decision.Decision, decision.SummaryZH, decision.RuleID, decision.SemgrepYAML, decision.HintLine
	}
	fill()
	switch decision.Decision {
	case contract.Ignore:
		proposal.Status = contract.ProposalIgnoredByAI
	case contract.GuardHint:
		_, n, err := loadHints(item.Project)
		if err != nil {
			return proposal, err
		}
		if n >= maxHints {
			return proposal, fmt.Errorf("guard hint capacity reached (%d)", maxHints)
		}
	case contract.HardRule:
		report, sandboxErr := r.verifyRule(ctx, item, decision)
		if sandboxErr != nil {
			if ctx.Err() != nil {
				return proposal, ctx.Err()
			}
			messages = append(messages, message{Role: "assistant", Content: answer},
				message{Role: "user", Content: "The sandbox rejected this rule. Return a corrected HARD_RULE JSON object. Exact failure: " + sandboxErr.Error()})
			answer, err := client.complete(ctx, messages)
			if err != nil {
				return proposal, err
			}
			decision, err = parseDecision(answer)
			if err != nil || decision.Decision != contract.HardRule {
				proposal.Status = contract.ProposalFailedSandbox
				return proposal, nil
			}
			fill()
			report, sandboxErr = r.verifyRule(ctx, item, decision)
		}
		proposal.SandboxPositiveHits = report.PositiveHits
		proposal.SandboxProjectHits = report.ProjectHits
		proposal.SandboxDurationMs = report.DurationMs
		if sandboxErr != nil {
			if ctx.Err() != nil {
				return proposal, ctx.Err()
			}
			proposal.Status = contract.ProposalFailedSandbox
		} else {
			proposal.SandboxProofHash = report.ProofHash
		}
	}
	return proposal, nil
}

func (r *Reviewer) verifyRule(ctx context.Context, item queued, d contract.ReviewerDecision) (core.SandboxReport, error) {
	if err := validateRuleIdentity(d.RuleID, d.SemgrepYAML); err != nil {
		return core.SandboxReport{}, err
	}
	report, err := r.Rules.VerifyRuleInSandbox(ctx, item.Project, d.SemgrepYAML, item.Snippets)
	if err != nil {
		return report, err
	}
	if !validProof(d.SemgrepYAML, report.ProofHash, report.PositiveHits, report.ProjectHits) {
		return report, fmt.Errorf("sandbox returned invalid proof")
	}
	return report, nil
}

func randomID() (string, error) {
	var id [16]byte
	_, err := rand.Read(id[:])
	return hex.EncodeToString(id[:]), err
}

func (s *Store) claim(ctx context.Context) (item queued, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		now := time.Now()
		var until string
		err := tx.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key='review_lease_until'`).Scan(&until)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if until > timestamp(now) {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE mistake_fingerprints SET status=? WHERE status=?`, contract.MistakeQueued, contract.MistakeReviewing)
		if err != nil {
			return err
		}
		var snippets string
		err = tx.QueryRowContext(ctx, `SELECT project_path, fingerprint, language, checker, rule_id, sample_message, snippets_json, occurrence_count
			FROM mistake_fingerprints WHERE status=? ORDER BY last_seen_at, id LIMIT 1`, contract.MistakeQueued).
			Scan(&item.Project, &item.Fingerprint, &item.Language, &item.Checker, &item.RuleID, &item.Message, &snippets, &item.Occurrences)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(snippets), &item.Snippets); err != nil {
			return err
		}
		item.token, err = randomID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mistake_fingerprints SET status=? WHERE project_path=? AND fingerprint=?`,
			contract.MistakeReviewing, item.Project, item.Fingerprint); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO app_settings(key, value) VALUES ('review_lease_token', ?), ('review_lease_until', ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, item.token, timestamp(now.Add(reviewDeadline+30*time.Second)))
		return err
	})
	return item, err
}

func (s *Store) finish(ctx context.Context, item queued, p *contract.RuleProposal) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var token string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key='review_lease_token'`).Scan(&token); err != nil {
			return err
		}
		if token != item.token {
			return fmt.Errorf("review claim expired")
		}
		status := contract.MistakeQueued
		if p != nil {
			status = contract.MistakeStatus(p.Status)
			now := timestamp(time.Now())
			_, err := tx.ExecContext(ctx, `INSERT INTO rule_proposals(`+proposalFields+`)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.ID, p.ProjectPath, p.Fingerprint, p.Decision, p.SummaryZH, p.RuleID, p.SemgrepYAML, p.HintLine,
				p.SandboxProofHash, p.SandboxPositiveHits, p.SandboxProjectHits, p.SandboxDurationMs, p.Status, "", now, now)
			if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mistake_fingerprints SET status=? WHERE project_path=? AND fingerprint=? AND status=?`,
			status, item.Project, item.Fingerprint, contract.MistakeReviewing); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM app_settings WHERE key IN ('review_lease_token', 'review_lease_until')`)
		return err
	})
}
