package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
)

const goodRule = `rules:
- id: bad-format
  languages: [go]
  severity: ERROR
  message: Fix the format
  pattern: fmt.Printf("%d", "x")
`
const badRule = `rules:
- id: bad-format
  languages: [go]
  severity: ERROR
  message: Fix the format
  pattern: "$BROKEN"
`

type ruleMock struct {
	sandboxCalls  int
	validationErr error
}

func (m *ruleMock) VerifyRuleInSandbox(_ context.Context, _ string, yaml string, samples []core.Snippet) (core.SandboxReport, error) {
	m.sandboxCalls++
	if yaml != goodRule {
		return core.SandboxReport{}, fmt.Errorf("sandbox matched zero supplied snippets")
	}
	if len(samples) == 0 || !strings.Contains(samples[0].Code, "Printf") {
		return core.SandboxReport{}, fmt.Errorf("no valid samples")
	}
	return core.SandboxReport{ProofHash: core.SandboxProofHash(yaml, 1, 2), PositiveHits: 1, ProjectHits: 2, DurationMs: 3}, nil
}

func (m *ruleMock) ValidateRuleFile(_ context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(data) != goodRule {
		return fmt.Errorf("unexpected committed rule content")
	}
	return m.validationErr
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type reviewFixture struct {
	*Reviewer
	project    string
	diagnostic contract.Diagnostic
	calls      int
	mock       *ruleMock
}

func fixtureFor(t *testing.T, answers ...string) *reviewFixture {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "db space#test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	settings, err := store.Settings(ctx)
	if err != nil || settings != (Settings{BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"}) {
		t.Fatalf("defaults = %+v, %v", settings, err)
	}
	if len(answers) > 0 {
		settings.APIKey = "test-secret"
		if err := store.UpdateSettings(ctx, settings); err != nil {
			t.Fatal(err)
		}
	}
	f := &reviewFixture{Reviewer: &Reviewer{Store: store}, project: t.TempDir(), mock: &ruleMock{},
		diagnostic: contract.Diagnostic{Language: "go", Checker: "go", Message: "incorrect format on line 32",
			File: "main.go", Severity: "error", CodeSnippet: "func f() { fmt.Printf(\"%d\", \"x\") }"}}
	f.Rules = f.mock
	f.HTTP = &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		call := f.calls
		f.calls++
		if call >= len(answers) {
			t.Errorf("unexpected request %d", call+1)
			return nil, errors.New("too many requests")
		}
		if req.URL.String() != "https://api.deepseek.com/v1/chat/completions" || req.Method != http.MethodPost ||
			req.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("unexpected DeepSeek request %s %s", req.Method, req.URL)
		}
		var body struct {
			Model    string
			Messages []message
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "deepseek-chat" || len(body.Messages) < 2 {
			t.Errorf("unexpected request body: %+v", body)
		}
		if call == 1 && answers[0] == answer(contract.HardRule, badRule) {
			if len(body.Messages) != 4 || !strings.Contains(body.Messages[3].Content, "matched zero supplied snippets") {
				t.Errorf("missing exact repair feedback: %+v", body.Messages)
			}
		}
		data, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": answers[call]}}}})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})}
	return f
}

func answer(decision contract.Decision, data string) string {
	p := contract.ReviewerDecision{Decision: decision, SummaryZH: "修复重复错误"}
	if decision == contract.HardRule {
		p.SemgrepYAML, p.RuleID = data, "bad-format"
	} else if decision == contract.GuardHint {
		p.HintLine = data
	}
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (f *reviewFixture) record(t *testing.T, fail bool) {
	t.Helper()
	res := contract.VerifyResult{Scope: contract.ScopeAll, Status: contract.Pass, DetectedLanguages: []string{"go"}}
	if fail {
		res.Status, res.Diagnostics = contract.Fail, []contract.Diagnostic{f.diagnostic}
	}
	if err := f.Store.RecordVerifyResult(context.Background(), f.project, res); err != nil {
		t.Fatal(err)
	}
}

func (f *reviewFixture) status(t *testing.T, want contract.MistakeStatus) {
	t.Helper()
	var status contract.MistakeStatus
	ctx := context.Background()
	if err := f.Store.db.QueryRowContext(ctx, `SELECT status FROM mistake_fingerprints WHERE project_path=? AND fingerprint=?`,
		f.project, Fingerprint(f.diagnostic)).Scan(&status); err != nil || status != want {
		t.Fatalf("fingerprint status = %q, %v; want %q", status, err, want)
	}
}

func (f *reviewFixture) step(t *testing.T, want bool) {
	t.Helper()
	processed, err := f.ProcessQueueOnce(context.Background())
	if err != nil || processed != want {
		t.Fatalf("processed = %t, %v; want %t", processed, err, want)
	}
}

func (f *reviewFixture) proposal(t *testing.T) contract.RuleProposal {
	t.Helper()
	p, err := f.Store.ListProposals(context.Background(), f.project)
	if err != nil || len(p) != 1 {
		t.Fatalf("expected one proposal, got %+v, %v", p, err)
	}
	return p[0]
}

func TestOccurrenceThreshold(t *testing.T) {
	f := fixtureFor(t)
	for i := 1; i <= 3; i++ {
		f.record(t, true)
		if i < 3 {
			f.status(t, contract.MistakeCollecting)
			f.record(t, false)
		}
	}
	f.status(t, contract.MistakeQueued)
	f.step(t, false)
	f.status(t, contract.MistakeQueued)
}

func TestConsecutiveThreshold(t *testing.T) {
	f := fixtureFor(t)
	f.record(t, true)
	f.status(t, contract.MistakeCollecting)
	f.record(t, true)
	f.status(t, contract.MistakeQueued)
}

func TestSandboxRepair(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			next := badRule
			if success {
				next = goodRule
			}
			f := fixtureFor(t, answer(contract.HardRule, badRule), "```json\n"+answer(contract.HardRule, next)+"\n```")
			f.record(t, true)
			f.record(t, true)
			f.step(t, true)
			f.step(t, false)
			if f.calls != 2 || f.mock.sandboxCalls != 2 {
				t.Fatalf("calls: HTTP=%d sandbox=%d", f.calls, f.mock.sandboxCalls)
			}
			if success {
				f.status(t, contract.MistakeProposed)
				p := f.proposal(t)
				if p.SemgrepYAML != goodRule || !validProof(p.SemgrepYAML, p.SandboxProofHash, 1, 2) || p.SandboxDurationMs != 3 {
					t.Fatalf("bad sandbox evidence: %+v", p)
				}
			} else {
				f.status(t, contract.MistakeFailedSandbox)
				var status contract.ProposalStatus
				err := f.Store.db.QueryRow(`SELECT status FROM rule_proposals`).Scan(&status)
				if err != nil || status != contract.ProposalFailedSandbox {
					t.Fatalf("proposal status %q, %v", status, err)
				}
			}
		})
	}
}

func TestApproveProofAndFilesystem(t *testing.T) {
	ctx := context.Background()
	f := fixtureFor(t, answer(contract.HardRule, goodRule))
	f.record(t, true)
	f.record(t, true)
	f.step(t, true)
	p := f.proposal(t)
	path := filepath.Join(core.RulesDir(f.project), "bad-format.yaml")
	setProof := func(proof string) {
		t.Helper()
		if _, err := f.Store.db.ExecContext(ctx, `UPDATE rule_proposals SET sandbox_proof_hash=? WHERE id=?`, proof, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, proof := range []string{"", core.SandboxProofHash(goodRule+"#", 1, 2)} {
		setProof(proof)
		if _, err := f.ApproveProposal(ctx, f.project, p.ID); err == nil || !strings.Contains(err.Error(), "sandbox proof") {
			t.Fatalf("invalid proof error = %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid rule wrote a file: %v", err)
		}
	}
	setProof(p.SandboxProofHash)
	f.mock.validationErr = errors.New("on-disk validation failed")
	if _, err := f.ApproveProposal(ctx, f.project, p.ID); !errors.Is(err, f.mock.validationErr) {
		t.Fatalf("expected revalidation failure, got %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed validation left a rule: %v", err)
	}
	f.status(t, contract.MistakeProposed)
	f.mock.validationErr = nil
	got, err := f.ApproveProposal(ctx, f.project, p.ID)
	if err != nil || got.AppliedPath != path || got.Status != contract.ProposalApproved {
		t.Fatalf("approval = %+v, %v", got, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != goodRule {
		t.Fatalf("disk rule = %q, %v", data, err)
	}
	f.status(t, contract.MistakeApproved)
	var state contract.ProposalStatus
	var appliedPath string
	if err := f.Store.db.QueryRow(`SELECT status, applied_path FROM rule_proposals WHERE id=?`, p.ID).Scan(&state, &appliedPath); err != nil ||
		state != contract.ProposalApproved || appliedPath != path {
		t.Fatalf("persisted state = %q, %q, %v", state, appliedPath, err)
	}
	if err := f.ToggleRule(ctx, f.project, p.RuleID, false); err != nil {
		t.Fatal(err)
	}
	rules, err := ListAppliedRules(f.project)
	if err != nil || len(rules) != 1 || rules[0].Enabled || rules[0].Path != path+".disabled" {
		t.Fatalf("disabled rules = %+v, %v", rules, err)
	}
	if err := f.ToggleRule(ctx, f.project, p.RuleID, true); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != goodRule {
		t.Fatalf("restored rule = %q, %v", data, err)
	}
	if err := f.DeleteAppliedRule(ctx, f.project, p.RuleID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed rule remains: %v", err)
	}
	if err := f.Store.db.QueryRow(`SELECT applied_path FROM rule_proposals WHERE id=?`, p.ID).Scan(&appliedPath); err != nil || appliedPath != "" {
		t.Fatalf("deleted rule DB path = %q, %v", appliedPath, err)
	}
}

func TestDismissAndSnooze(t *testing.T) {
	for _, mode := range []string{"dismiss", "snooze"} {
		t.Run(mode, func(t *testing.T) {
			a := answer(contract.GuardHint, "Prefer typed format parameters.")
			replies := []string{a}
			state := contract.MistakeDismissed
			if mode == "snooze" {
				replies = append(replies, a)
				state = contract.MistakeSnoozed
			}
			f := fixtureFor(t, replies...)
			f.record(t, true)
			f.record(t, true)
			f.step(t, true)
			p := f.proposal(t)
			if _, err := f.DismissProposal(context.Background(), f.project, p.ID, mode); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 5; i++ {
				f.record(t, true)
				if mode == "snooze" && i == 5 {
					f.status(t, contract.MistakeQueued)
					f.step(t, true)
					f.status(t, contract.MistakeProposed)
				} else {
					f.step(t, false)
					f.status(t, state)
				}
			}
			if f.calls != len(replies) {
				t.Fatalf("HTTP requests = %d, want %d", f.calls, len(replies))
			}
		})
	}
}

func TestFingerprintPrivacyAndContext(t *testing.T) {
	f := fixtureFor(t)
	f.diagnostic.Message = `cannot resolve "/home/alice/private project/file.go" on line 32`
	original := Fingerprint(f.diagnostic)
	f.diagnostic.Message = `cannot resolve "C:\Users\bob\private project\file.go" on line 386`
	if original != Fingerprint(f.diagnostic) {
		t.Fatal("paths or line numbers changed the fingerprint")
	}
	for i := 0; i < 5; i++ {
		f.diagnostic.CodeSnippet = fmt.Sprint(i)
		f.record(t, true)
	}
	var message, snippets string
	if err := f.Store.db.QueryRow(`SELECT sample_message,snippets_json FROM mistake_fingerprints`).Scan(&message, &snippets); err != nil {
		t.Fatal(err)
	}
	var context []core.Snippet
	if err := json.Unmarshal([]byte(snippets), &context); err != nil || len(context) != 3 || context[0].Code != "2" || context[2].Code != "4" || strings.Contains(message, "bob") {
		t.Fatalf("stored context = %q, %q, %v", message, snippets, err)
	}
}

func TestGuardHintCapacity(t *testing.T) {
	f := fixtureFor(t, answer(contract.GuardHint, "Use a concrete format type."))
	f.record(t, true)
	f.record(t, true)
	f.step(t, true)
	p := f.proposal(t)
	path := hintsPath(f.project)
	original := "# project hints\r\n" + strings.Repeat("some existing rule\r\n", maxHints-1)
	if err := core.WriteFileAtomic(path, []byte(original), 0600, true, nil); err != nil {
		t.Fatal(err)
	}
	got, err := f.ApproveProposal(context.Background(), f.project, p.ID)
	if err != nil || got.Status != contract.ProposalApproved {
		t.Fatalf("approve hint = %+v, %v", got, err)
	}
	data, n, err := loadHints(f.project)
	if err != nil || n != maxHints || string(data) != original+p.HintLine+"\r\n" {
		t.Fatalf("hints = %q, %d, %v", data, n, err)
	}
}
