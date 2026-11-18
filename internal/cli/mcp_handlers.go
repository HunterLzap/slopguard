package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
	"github.com/HunterLzap/slopguard/internal/mcp"
	"github.com/HunterLzap/slopguard/internal/store"
)

func buildMCPTools(root string) []mcp.Tool {
	return []mcp.Tool{
		{
			Name:        "sg_verify",
			Description: "Run the deterministic SlopGuard gate for the project.",
			InputSchema: objectSchema(map[string]any{
				"all":   map[string]any{"type": "boolean", "description": "scan all files instead of git-changed files"},
				"paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "optional explicit file paths"},
			}),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a struct {
					All   bool     `json:"all"`
					Paths []string `json:"paths"`
				}
				if err := decodeMCPArgs(args, &a); err != nil {
					return nil, err
				}
				return verifyForMCP(root, a.All, a.Paths)
			},
		},
		{
			Name:        "sg_radar",
			Description: "Scan for high-risk bad-sample hotspots and return Top N ranked files.",
			InputSchema: objectSchema(map[string]any{
				"top": map[string]any{"type": "integer", "description": "number of hotspots to return (default 3)"},
			}),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a struct {
					Top int `json:"top"`
				}
				if err := decodeMCPArgs(args, &a); err != nil {
					return nil, err
				}
				return radarForMCP(ctx, root, a.Top)
			},
		},
		{
			Name:        "sg_record_mistake",
			Description: "Record an AI mistake for later rule extraction; repeated fingerprints increment the count.",
			InputSchema: objectSchema(map[string]any{
				"category":     map[string]any{"type": "string", "description": "import | syntax | type | architecture | ui"},
				"summary":      map[string]any{"type": "string"},
				"bad_snippet":  map[string]any{"type": "string"},
				"good_snippet": map[string]any{"type": "string"},
			}, "summary"),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a struct {
					Category    string `json:"category"`
					Summary     string `json:"summary"`
					BadSnippet  string `json:"bad_snippet"`
					GoodSnippet string `json:"good_snippet"`
				}
				if err := decodeMCPArgs(args, &a); err != nil {
					return nil, err
				}
				return recordMistakeForMCP(ctx, root, a.Category, a.Summary, a.BadSnippet, a.GoodSnippet)
			},
		},
		{
			Name:        "sg_list_candidates",
			Description: "List pending mistake clusters that reached the minimum occurrence count.",
			InputSchema: objectSchema(map[string]any{
				"min_count": map[string]any{"type": "integer", "description": "minimum occurrence count (default 2)"},
			}),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a struct {
					MinCount int `json:"min_count"`
				}
				if err := decodeMCPArgs(args, &a); err != nil {
					return nil, err
				}
				return listCandidatesForMCP(ctx, a.MinCount)
			},
		},
		{
			Name:        "sg_dry_run_rule",
			Description: "Run a candidate rule in an isolated temporary config; never modifies project config.",
			InputSchema: objectSchema(map[string]any{
				"engine":       map[string]any{"type": "string", "description": "eslint | ruff | semgrep"},
				"rule_id":      map[string]any{"type": "string"},
				"rule_payload": map[string]any{"description": "rule configuration fragment (JSON or YAML)"},
				"sample_bad":   map[string]any{"type": "string"},
				"files":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"threshold":    map[string]any{"type": "integer"},
			}, "engine", "rule_payload"),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				return dryRunRuleForMCP(ctx, root, args)
			},
		},
		{
			Name:        "sg_apply_rule",
			Description: "Apply a confirmed rule to project config or global presets and record it in the rule ledger.",
			InputSchema: objectSchema(map[string]any{
				"engine":       map[string]any{"type": "string", "description": "eslint | ruff | semgrep"},
				"rule_id":      map[string]any{"type": "string"},
				"rule_payload": map[string]any{"description": "rule configuration fragment"},
				"scope":        map[string]any{"type": "string", "enum": []string{"project", "global"}},
				"rationale":    map[string]any{"type": "string"},
				"confirm":      map[string]any{"type": "boolean", "description": "must be true; user confirmation is required"},
			}, "engine", "rule_payload", "confirm"),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				return applyRuleForMCP(ctx, root, args)
			},
		},
	}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func decodeMCPArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}

func verifyForMCP(root string, all bool, paths []string) (contract.VerifyResult, error) {
	var stdout, stderr bytes.Buffer
	code := runVerifyWith(verifyOptions{
		root:  root,
		all:   all,
		json:  true,
		paths: paths,
	}, &stdout, &stderr)
	if code == ExitError {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "verify failed"
		}
		return contract.VerifyResult{}, errors.New(msg)
	}
	var result contract.VerifyResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return contract.VerifyResult{}, fmt.Errorf("decode verify result: %w (output=%q)", err, strings.TrimSpace(stdout.String()))
	}
	return result, nil
}

func radarForMCP(ctx context.Context, root string, top int) (radarResult, error) {
	deps, err := newDependencies()
	if err != nil {
		return radarResult{}, err
	}
	if deps.radar == nil {
		return radarResult{}, errors.New("radar engine is not available")
	}
	return deps.radar.Scan(ctx, root, top)
}

func recordMistakeForMCP(ctx context.Context, project, category, summary, bad, good string) (any, error) {
	if strings.TrimSpace(summary) == "" {
		return nil, errors.New("summary is required")
	}
	if strings.TrimSpace(category) == "" {
		category = "other"
	}
	fingerprint := mistakeFingerprint(project, category, summary)
	db, err := store.OpenDefault()
	if err != nil {
		return nil, fmt.Errorf("open mistake store: %w", err)
	}
	defer db.Close()
	count, err := db.RecordMistake(ctx, contract.Mistake{
		ProjectPath: project,
		Fingerprint: fingerprint,
		Category:    category,
		Summary:     summary,
		BadSnippet:  bad,
		GoodSnippet: good,
		Source:      "agent",
	})
	if err != nil {
		return nil, fmt.Errorf("record mistake: %w", err)
	}
	return map[string]any{"recorded": true, "fingerprint": fingerprint, "count": count}, nil
}

func listCandidatesForMCP(ctx context.Context, minCount int) ([]contract.Mistake, error) {
	db, err := store.OpenDefault()
	if err != nil {
		return nil, fmt.Errorf("open mistake store: %w", err)
	}
	defer db.Close()
	candidates, err := db.ListCandidates(ctx, minCount)
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	if candidates == nil {
		candidates = []contract.Mistake{}
	}
	return candidates, nil
}

type dryRunArgs struct {
	Engine      string          `json:"engine"`
	RuleID      string          `json:"rule_id"`
	RulePayload json.RawMessage `json:"rule_payload"`
	SampleBad   string          `json:"sample_bad"`
	Files       []string        `json:"files"`
	Threshold   int             `json:"threshold"`
}

func dryRunRuleForMCP(ctx context.Context, root string, args json.RawMessage) (any, error) {
	var a dryRunArgs
	if err := decodeMCPArgs(args, &a); err != nil {
		return nil, err
	}
	payload, err := rawPayloadString(a.RulePayload)
	if err != nil {
		return nil, err
	}
	ruleID := strings.TrimSpace(a.RuleID)
	if ruleID == "" {
		ruleID = deriveRuleID([]byte(payload))
	}
	return engine.DryRunRule(ctx, engine.NewExecRunner(), engine.DryRunOptions{
		Engine:      a.Engine,
		RuleID:      ruleID,
		RulePayload: payload,
		ProjectRoot: root,
		SampleBad:   a.SampleBad,
		Files:       a.Files,
		Threshold:   a.Threshold,
	})
}

type applyRuleArgs struct {
	Engine      string          `json:"engine"`
	RuleID      string          `json:"rule_id"`
	RulePayload json.RawMessage `json:"rule_payload"`
	Scope       string          `json:"scope"`
	Rationale   string          `json:"rationale"`
	Confirm     bool            `json:"confirm"`
}

func applyRuleForMCP(ctx context.Context, root string, args json.RawMessage) (any, error) {
	var a applyRuleArgs
	if err := decodeMCPArgs(args, &a); err != nil {
		return nil, err
	}
	if !a.Confirm {
		return nil, errors.New("confirm=true is required; ask the user to approve the rule diff before applying")
	}
	payload, err := rawPayloadString(a.RulePayload)
	if err != nil {
		return nil, err
	}
	ruleID := strings.TrimSpace(a.RuleID)
	if ruleID == "" {
		ruleID = deriveRuleID([]byte(payload))
	}
	scope := strings.ToLower(strings.TrimSpace(a.Scope))
	if scope == "" {
		scope = "project"
	}

	var result adapter.RuleApplyResult
	switch scope {
	case "project":
		result, err = adapter.ApplyRule(a.Engine, ruleID, []byte(payload), root)
	case "global":
		var path string
		path, err = adapter.MergeGlobalRuleTemplate(a.Engine, ruleID, []byte(payload))
		result = adapter.RuleApplyResult{ConfigPath: path}
	default:
		return nil, fmt.Errorf("unsupported scope %q (want project or global)", a.Scope)
	}
	if err != nil {
		return nil, fmt.Errorf("apply rule: %w", err)
	}

	ledgerErr := recordRuleLedger(ctx, scope, root, a.Engine, ruleID, payload, a.Rationale)
	out := map[string]any{
		"applied":     true,
		"engine":      strings.ToLower(a.Engine),
		"rule_id":     ruleID,
		"scope":       scope,
		"config_path": result.ConfigPath,
		"backup_path": result.BackupPath,
		"diff":        result.Diff,
	}
	if ledgerErr != nil {
		out["ledger_error"] = ledgerErr.Error()
	}
	return out, nil
}

func recordRuleLedger(ctx context.Context, scope, project, engineName, ruleID, payload, rationale string) error {
	db, err := store.OpenDefault()
	if err != nil {
		return err
	}
	defer db.Close()
	if scope == "global" {
		project = ""
	}
	return db.InsertRule(ctx, store.Rule{
		Scope:       scope,
		ProjectPath: project,
		Engine:      strings.ToLower(engineName),
		RuleID:      ruleID,
		Payload:     payload,
		Rationale:   rationale,
		Source:      "agent",
		Status:      "active",
		AppliedAt:   time.Now().UTC(),
	})
}

func rawPayloadString(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", errors.New("rule_payload is required")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	return string(raw), nil
}

func deriveRuleID(payload []byte) string {
	var rules map[string]json.RawMessage
	if err := json.Unmarshal(payload, &rules); err == nil && len(rules) == 1 {
		for key := range rules {
			if key != "" {
				return key
			}
		}
	}
	return "slopguard-rule"
}
