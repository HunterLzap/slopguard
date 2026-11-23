package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
	"github.com/HunterLzap/slopguard/internal/mcp"
	"github.com/HunterLzap/slopguard/internal/store"
	"github.com/HunterLzap/slopguard/internal/verify"
)

var openMCPStore = store.OpenDefault

const projectPathSchemaDesc = "target project repository path (required when MCP server runs globally without --root)"

func buildMCPTools(root string) []mcp.Tool {
	scopeRoot := normalizeMCPScopePath(root)
	tools := []mcp.Tool{
		{
			Name:        "sg_verify",
			Description: "Run the deterministic SlopGuard gate for the project. Also returns any pending_inbox mistakes awaiting plain-language Chinese analysis via sg_submit_analysis.",
			InputSchema: objectSchema(map[string]any{
				"project_path": map[string]any{"type": "string", "description": projectPathSchemaDesc},
				"all":          map[string]any{"type": "boolean", "description": "scan all files instead of git-changed files"},
				"paths":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "optional explicit file paths"},
				"ui":           map[string]any{"type": "boolean", "description": "also run Playwright UI smoke checks after regular checks pass"},
			}),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a struct {
					ProjectPath string   `json:"project_path"`
					All         bool     `json:"all"`
					Paths       []string `json:"paths"`
					UI          bool     `json:"ui"`
				}
				if err := decodeMCPArgs(args, &a); err != nil {
					return nil, err
				}
				resolved, err := resolveMCPToolScope(ctx, scopeRoot, a.ProjectPath)
				if err != nil {
					return nil, err
				}
				return verifyForMCP(ctx, resolved, a.All, a.Paths, a.UI)
			},
		},
		{
			Name:        "sg_radar",
			Description: "Scan for high-risk bad-sample hotspots and return Top N ranked files.",
			InputSchema: objectSchema(map[string]any{
				"project_path": map[string]any{"type": "string", "description": projectPathSchemaDesc},
				"top":          map[string]any{"type": "integer", "description": "number of hotspots to return (default 3)"},
			}),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a struct {
					ProjectPath string `json:"project_path"`
					Top         int    `json:"top"`
				}
				if err := decodeMCPArgs(args, &a); err != nil {
					return nil, err
				}
				resolved, err := resolveMCPToolScope(ctx, scopeRoot, a.ProjectPath)
				if err != nil {
					return nil, err
				}
				return radarForMCP(ctx, resolved, a.Top)
			},
		},
		{
			Name:        "sg_record_mistake",
			Description: "Record an AI mistake in the current project for later rule extraction; repeated fingerprints increment the count.",
			InputSchema: objectSchema(map[string]any{
				"project_path": map[string]any{"type": "string", "description": projectPathSchemaDesc},
				"category":     map[string]any{"type": "string", "description": "import | syntax | type | architecture | ui"},
				"summary":      map[string]any{"type": "string"},
				"bad_snippet":  map[string]any{"type": "string"},
				"good_snippet": map[string]any{"type": "string"},
			}, "summary"),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a struct {
					ProjectPath string `json:"project_path"`
					Category    string `json:"category"`
					Summary     string `json:"summary"`
					BadSnippet  string `json:"bad_snippet"`
					GoodSnippet string `json:"good_snippet"`
				}
				if err := decodeMCPArgs(args, &a); err != nil {
					return nil, err
				}
				resolved, err := resolveMCPToolScope(ctx, scopeRoot, a.ProjectPath)
				if err != nil {
					return nil, err
				}
				return recordMistakeForMCP(ctx, resolved, a.Category, a.Summary, a.BadSnippet, a.GoodSnippet)
			},
		},
		{
			Name:        "sg_list_candidates",
			Description: "List pending mistake clusters in the current project that reached the minimum occurrence count.",
			InputSchema: objectSchema(map[string]any{
				"project_path": map[string]any{"type": "string", "description": projectPathSchemaDesc},
				"min_count":    map[string]any{"type": "integer", "description": "minimum occurrence count (default 2)"},
			}),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a struct {
					ProjectPath string `json:"project_path"`
					MinCount    int    `json:"min_count"`
				}
				if err := decodeMCPArgs(args, &a); err != nil {
					return nil, err
				}
				resolved, err := resolveMCPToolScope(ctx, scopeRoot, a.ProjectPath)
				if err != nil {
					return nil, err
				}
				return listCandidatesForMCP(ctx, resolved, a.MinCount)
			},
		},
		{
			Name:        "sg_dry_run_rule",
			Description: "Run a candidate rule in an isolated temporary config; never modifies project config.",
			InputSchema: objectSchema(map[string]any{
				"project_path": map[string]any{"type": "string", "description": projectPathSchemaDesc},
				"engine":       map[string]any{"type": "string", "description": "eslint | ruff | semgrep"},
				"rule_id":      map[string]any{"type": "string"},
				"rule_payload": map[string]any{"description": "rule configuration fragment (JSON or YAML)"},
				"sample_bad":   map[string]any{"type": "string"},
				"files":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"threshold":    map[string]any{"type": "integer"},
			}, "engine", "rule_payload"),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				return dryRunRuleForMCP(ctx, scopeRoot, args)
			},
		},
		{
			Name:        "sg_apply_rule",
			Description: "Apply a confirmed rule to project config or global presets and record it in the rule ledger.",
			InputSchema: objectSchema(map[string]any{
				"project_path": map[string]any{"type": "string", "description": projectPathSchemaDesc},
				"engine":       map[string]any{"type": "string", "description": "eslint | ruff | semgrep"},
				"rule_id":      map[string]any{"type": "string"},
				"rule_payload": map[string]any{"description": "rule configuration fragment"},
				"scope":        map[string]any{"type": "string", "enum": []string{"project", "global"}},
				"rationale":    map[string]any{"type": "string"},
				"confirm":      map[string]any{"type": "boolean", "description": "must be true; user confirmation is required"},
			}, "engine", "rule_payload", "confirm"),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				return applyRuleForMCP(ctx, scopeRoot, args)
			},
		},
		{
			Name:        "sg_list_inbox",
			Description: "List auto-captured or pending inbox mistakes (pending_ai / pending_user) so the host Agent can analyze them.",
			InputSchema: objectSchema(map[string]any{
				"project_path":  map[string]any{"type": "string", "description": projectPathSchemaDesc},
				"project":       map[string]any{"type": "string", "description": "alias of project_path"},
				"review_status": map[string]any{"type": "string", "enum": []string{"pending_ai", "pending_user", "approved", "ignored"}},
			}),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a struct {
					ProjectPath  string `json:"project_path"`
					Project      string `json:"project"`
					ReviewStatus string `json:"review_status"`
				}
				if err := decodeMCPArgs(args, &a); err != nil {
					return nil, err
				}
				reqProject := firstNonEmpty(a.ProjectPath, a.Project)
				resolved, err := resolveMCPToolScope(ctx, scopeRoot, reqProject)
				if err != nil {
					return nil, err
				}
				return listInboxForMCP(ctx, resolved, reqProject, a.ReviewStatus)
			},
		},
		{
			Name:        "sg_submit_analysis",
			Description: "Submit plain-language Chinese AI analysis and recommendation for a captured mistake to await user approval in the dashboard.",
			InputSchema: objectSchema(map[string]any{
				"project_path": map[string]any{"type": "string", "description": projectPathSchemaDesc},
				"fingerprint":  map[string]any{"type": "string"},
				"decision":     map[string]any{"type": "string", "enum": []string{"recommend", "skip"}},
				"reason":       map[string]any{"type": "string", "description": "plain-language Chinese explanation of why to recommend or skip"},
				"rule_draft":   map[string]any{"type": "string", "description": "optional draft rule configuration"},
			}, "fingerprint", "decision", "reason"),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var a struct {
					ProjectPath string `json:"project_path"`
					Fingerprint string `json:"fingerprint"`
					Decision    string `json:"decision"`
					Reason      string `json:"reason"`
					RuleDraft   string `json:"rule_draft"`
				}
				if err := decodeMCPArgs(args, &a); err != nil {
					return nil, err
				}
				resolved, err := resolveMCPToolScope(ctx, scopeRoot, a.ProjectPath)
				if err != nil {
					return nil, err
				}
				return submitAnalysisForMCP(ctx, resolved, a.Fingerprint, a.Decision, a.Reason, a.RuleDraft)
			},
		},
	}
	return wrapMCPToolsWithAudit(scopeRoot, tools)
}

func wrapMCPToolsWithAudit(root string, tools []mcp.Tool) []mcp.Tool {
	wrapped := make([]mcp.Tool, len(tools))
	for i, tool := range tools {
		t := tool
		orig := t.Handler
		t.Handler = func(ctx context.Context, args json.RawMessage) (any, error) {
			started := time.Now()
			res, err := orig(ctx, args)
			durationMs := time.Since(started).Milliseconds()
			auditProject := root
			if resolved, rErr := resolveMCPToolScope(ctx, root, extractMCPRequestedProject(args)); rErr == nil {
				auditProject = resolved
			}
			recordMCPAuditEvent(ctx, auditProject, t.Name, args, res, err, durationMs)
			return res, err
		}
		wrapped[i] = t
	}
	return wrapped
}

func extractMCPRequestedProject(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var a struct {
		ProjectPath string `json:"project_path"`
		Project     string `json:"project"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ""
	}
	return firstNonEmpty(a.ProjectPath, a.Project)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func recordMCPAuditEvent(ctx context.Context, root, toolName string, args json.RawMessage, res any, callErr error, durationMs int64) {
	db, err := openMCPStore()
	if err != nil {
		return
	}
	defer db.Close()

	passed := callErr == nil
	findingsCount := 0
	errorText := ""
	if callErr != nil {
		errorText = callErr.Error()
	}
	switch v := res.(type) {
	case mcpVerifyResponse:
		passed = passed && v.Passed
		findingsCount = len(v.Findings)
	case *mcpVerifyResponse:
		if v != nil {
			passed = passed && v.Passed
			findingsCount = len(v.Findings)
		}
	case contract.VerifyResult:
		passed = passed && v.Passed
		findingsCount = len(v.Findings)
	case *contract.VerifyResult:
		if v != nil {
			passed = passed && v.Passed
			findingsCount = len(v.Findings)
		}
	}

	auditCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = db.RecordMCPEvent(auditCtx, store.MCPEvent{
		ProjectPath:   root,
		ToolName:      toolName,
		ArgsSummary:   summarizeMCPArgs(args),
		Passed:        passed,
		FindingsCount: findingsCount,
		DurationMs:    durationMs,
		ErrorText:     errorText,
	})
}

func summarizeMCPArgs(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "{}"
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(trimmed)); err == nil {
		trimmed = compact.String()
	}
	const maxLen = 240
	if len(trimmed) > maxLen {
		return trimmed[:maxLen] + "..."
	}
	return trimmed
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

type mcpInboxBriefItem struct {
	Fingerprint string `json:"fingerprint"`
	Category    string `json:"category"`
	Summary     string `json:"summary"`
	BadSnippet  string `json:"bad_snippet,omitempty"`
	Count       int    `json:"count"`
}

type mcpVerifyResponse struct {
	contract.VerifyResult
	PendingInboxCount int                 `json:"pending_inbox_count,omitempty"`
	PendingInbox      []mcpInboxBriefItem `json:"pending_inbox,omitempty"`
	NextAction        string              `json:"next_action,omitempty"`
}

type mcpVerifyRecorder struct{}

func (mcpVerifyRecorder) RecordScan(ctx context.Context, projectPath string, durationMs int64, escapes int, passed bool) error {
	db, err := openMCPStore()
	if err != nil {
		return err
	}
	defer db.Close()
	recordCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.RecordScan(recordCtx, projectPath, durationMs, escapes, passed)
}

func (mcpVerifyRecorder) RecordMistake(ctx context.Context, m contract.Mistake) (int, error) {
	db, err := openMCPStore()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	recordCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.RecordMistake(recordCtx, m)
}

func verifyForMCP(ctx context.Context, root string, all bool, paths []string, ui bool) (mcpVerifyResponse, error) {
	scopeRoot, err := resolveMCPEnforcedRoot(root)
	if err != nil {
		return mcpVerifyResponse{}, err
	}
	result, err := verify.Run(ctx, verify.Options{
		ProjectRoot: scopeRoot,
		All:         all,
		Paths:       paths,
		UI:          ui,
		AutoCapture: true,
		Recorder:    mcpVerifyRecorder{},
	})
	if err != nil {
		return mcpVerifyResponse{}, err
	}

	out := mcpVerifyResponse{VerifyResult: result}
	qCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if items, qErr := listInboxForMCP(qCtx, scopeRoot, "", "pending_ai"); qErr == nil && len(items) > 0 {
		out.PendingInboxCount = len(items)
		limit := len(items)
		if limit > 5 {
			limit = 5
		}
		out.PendingInbox = make([]mcpInboxBriefItem, 0, limit)
		for i := 0; i < limit; i++ {
			out.PendingInbox = append(out.PendingInbox, mcpInboxBriefItem{
				Fingerprint: items[i].Fingerprint,
				Category:    items[i].Category,
				Summary:     items[i].Summary,
				BadSnippet:  items[i].BadSnippet,
				Count:       items[i].Count,
			})
		}
		out.NextAction = "There are unanalyzed mistakes in pending_inbox. Please call sg_submit_analysis with a plain-language Chinese reason and recommend/skip decision so the user can review them in the SlopGuard dashboard."
	}
	return out, nil
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

func recordMistakeForMCP(ctx context.Context, root, category, summary, bad, good string) (any, error) {
	if strings.TrimSpace(summary) == "" {
		return nil, errors.New("summary is required")
	}
	scopeRoot, err := resolveMCPEnforcedRoot(root)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(category) == "" {
		category = "other"
	}
	fingerprint := mistakeFingerprint(scopeRoot, category, summary)
	db, err := openMCPStore()
	if err != nil {
		return nil, fmt.Errorf("open mistake store: %w", err)
	}
	defer db.Close()
	count, err := db.RecordMistake(ctx, contract.Mistake{
		ProjectPath: scopeRoot,
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
	return map[string]any{"recorded": true, "project": scopeRoot, "fingerprint": fingerprint, "count": count}, nil
}

func listCandidatesForMCP(ctx context.Context, root string, minCount int) ([]contract.Mistake, error) {
	scopeRoot, err := resolveMCPEnforcedRoot(root)
	if err != nil {
		return nil, err
	}
	db, err := openMCPStore()
	if err != nil {
		return nil, fmt.Errorf("open mistake store: %w", err)
	}
	defer db.Close()
	candidates, err := db.ListCandidatesScoped(ctx, scopeRoot, minCount)
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	if candidates == nil {
		candidates = []contract.Mistake{}
	}
	return candidates, nil
}

func normalizeMCPScopePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if strings.ContainsRune(p, 0) {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != "" {
		p = resolved
	}
	return filepath.Clean(p)
}

func isDisallowedGlobalMCPDir(p string) bool {
	clean := normalizeMCPScopePath(p)
	if clean == "" {
		return true
	}
	if filepath.Dir(clean) == clean {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		if normalizeMCPScopePath(home) == clean {
			return true
		}
	}
	return false
}

func resolveMCPToolScope(ctx context.Context, pinnedRoot, requestedProject string) (string, error) {
	pinned := normalizeMCPScopePath(pinnedRoot)
	if pinned != "" {
		if isDisallowedGlobalMCPDir(pinned) {
			return "", fmt.Errorf("mcp project root %q cannot be filesystem root or home directory; pass a project repository path", pinned)
		}
		if strings.TrimSpace(requestedProject) != "" {
			reqNorm := normalizeMCPScopePath(requestedProject)
			if reqNorm == "" || reqNorm != pinned {
				return "", fmt.Errorf("cross-project operation blocked: MCP server is scoped to %s", pinned)
			}
		}
		return resolveMCPEnforcedRoot(pinned)
	}

	if sessionRoot := normalizeMCPScopePath(mcp.SessionRootFromContext(ctx)); sessionRoot != "" && !isDisallowedGlobalMCPDir(sessionRoot) {
		if strings.TrimSpace(requestedProject) != "" {
			reqNorm := normalizeMCPScopePath(requestedProject)
			if reqNorm == "" || reqNorm != sessionRoot {
				return "", fmt.Errorf("cross-project operation blocked: MCP session is scoped to %s", sessionRoot)
			}
		}
		return resolveMCPEnforcedRoot(sessionRoot)
	}

	reqRaw := strings.TrimSpace(requestedProject)
	if reqRaw == "" {
		return "", errors.New("mcp workspace root is required: pass project_path for the target repository or start slopguard mcp with --root")
	}
	reqNorm := normalizeMCPScopePath(reqRaw)
	if isDisallowedGlobalMCPDir(reqNorm) {
		return "", fmt.Errorf("invalid project_path %q: must be a concrete project directory, not filesystem root or home directory", reqRaw)
	}
	return resolveMCPEnforcedRoot(reqNorm)
}

func resolveMCPEnforcedRoot(root string) (string, error) {
	raw := strings.TrimSpace(root)
	if raw == "" || strings.ContainsRune(raw, 0) {
		return "", errors.New("mcp workspace root is required for mistake operations")
	}
	scopeRoot := normalizeMCPScopePath(raw)
	if scopeRoot == "" || isDisallowedGlobalMCPDir(scopeRoot) {
		return "", errors.New("mcp workspace root cannot be filesystem root or home directory")
	}
	info, err := os.Stat(scopeRoot)
	if err != nil {
		return "", fmt.Errorf("mcp project root %s: %w", scopeRoot, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("mcp project root %s is not a directory", scopeRoot)
	}
	return scopeRoot, nil
}

func listInboxForMCP(ctx context.Context, root, requestedProject, reviewStatus string) ([]store.MistakeRecord, error) {
	scopeRoot, err := resolveMCPEnforcedRoot(root)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(requestedProject) != "" {
		if normalizeMCPScopePath(requestedProject) != normalizeMCPScopePath(scopeRoot) {
			return nil, fmt.Errorf("cross-project query blocked: sg_list_inbox is scoped to %s", scopeRoot)
		}
	}
	db, err := openMCPStore()
	if err != nil {
		return nil, fmt.Errorf("open mistake store: %w", err)
	}
	defer db.Close()
	items, err := db.ListInboxMistakes(ctx, scopeRoot, reviewStatus)
	if err != nil {
		return nil, fmt.Errorf("list inbox mistakes: %w", err)
	}
	// Double-check strict project scope across symlink aliases too.
	scoped := make([]store.MistakeRecord, 0, len(items))
	wantCanon := normalizeMCPScopePath(scopeRoot)
	for _, rec := range items {
		if normalizeMCPScopePath(rec.ProjectPath) == wantCanon {
			scoped = append(scoped, rec)
		}
	}
	return scoped, nil
}

func submitAnalysisForMCP(ctx context.Context, root, fingerprint, decision, reason, ruleDraft string) (any, error) {
	scopeRoot, err := resolveMCPEnforcedRoot(root)
	if err != nil {
		return nil, err
	}
	db, err := openMCPStore()
	if err != nil {
		return nil, fmt.Errorf("open mistake store: %w", err)
	}
	defer db.Close()
	if err := db.SubmitMistakeAnalysisScoped(ctx, scopeRoot, fingerprint, decision, reason, ruleDraft); err != nil {
		return nil, fmt.Errorf("submit analysis: %w", err)
	}
	return map[string]any{
		"submitted":     true,
		"project":       scopeRoot,
		"fingerprint":   strings.TrimSpace(fingerprint),
		"decision":      strings.ToLower(strings.TrimSpace(decision)),
		"review_status": "pending_user",
	}, nil
}

type dryRunArgs struct {
	ProjectPath string          `json:"project_path"`
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
	scopeRoot, err := resolveMCPToolScope(ctx, root, a.ProjectPath)
	if err != nil {
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
		ProjectRoot: scopeRoot,
		SampleBad:   a.SampleBad,
		Files:       a.Files,
		Threshold:   a.Threshold,
	})
}

type applyRuleArgs struct {
	ProjectPath string          `json:"project_path"`
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

	var scopeRoot string
	if scope == "project" || strings.TrimSpace(root) != "" || strings.TrimSpace(a.ProjectPath) != "" {
		var scopeErr error
		scopeRoot, scopeErr = resolveMCPToolScope(ctx, root, a.ProjectPath)
		if scopeErr != nil && scope == "project" {
			return nil, scopeErr
		}
	}

	var result adapter.RuleApplyResult
	switch scope {
	case "project":
		result, err = adapter.ApplyRule(a.Engine, ruleID, []byte(payload), scopeRoot)
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

	ledgerErr := recordRuleLedger(ctx, scope, scopeRoot, a.Engine, ruleID, payload, a.Rationale)
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
	db, err := openMCPStore()
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
