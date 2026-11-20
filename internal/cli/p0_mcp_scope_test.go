package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/mcp"
	"github.com/HunterLzap/slopguard/internal/store"
)

func TestP0MCPListCandidatesProjectIsolation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "slopguard.db")
	oldOpen := openMCPStore
	openMCPStore = func() (*store.Store, error) {
		return store.Open(dbPath)
	}
	t.Cleanup(func() { openMCPStore = oldOpen })

	repoA := t.TempDir()
	repoB := t.TempDir()
	s, err := openMCPStore()
	if err != nil {
		t.Fatal(err)
	}
	record := func(project, fingerprint, summary string) {
		for i := 0; i < 2; i++ {
			if _, err := s.RecordMistake(context.Background(), contract.Mistake{
				ProjectPath: project,
				Fingerprint: fingerprint,
				Category:    "type",
				Summary:     summary,
				Source:      "auto_verify",
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	record(repoA, "fp-repo-a", "issue A")
	record(repoB, "fp-repo-b", "secret issue B")
	_ = s.Close()

	tools := buildMCPTools(repoA)
	candidateTool := findMCPTool(t, tools, "sg_list_candidates")
	res, err := candidateTool.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("sg_list_candidates(repoA): %v", err)
	}
	candidates, ok := res.([]contract.Mistake)
	if !ok || len(candidates) != 1 || candidates[0].Fingerprint != "fp-repo-a" {
		t.Fatalf("sg_list_candidates result = %#v", res)
	}

	// Empty workspace root must fail closed instead of falling back to a global query.
	emptyTools := buildMCPTools("")
	emptyCandidate := findMCPTool(t, emptyTools, "sg_list_candidates")
	if _, err := emptyCandidate.Handler(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("sg_list_candidates with empty root must fail")
	}
}

func TestP0MCPVerifyPendingInboxProjectIsolation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "slopguard.db")
	oldOpen := openMCPStore
	openMCPStore = func() (*store.Store, error) {
		return store.Open(dbPath)
	}
	t.Cleanup(func() { openMCPStore = oldOpen })

	repoA := t.TempDir()
	repoB := t.TempDir()
	s, err := openMCPStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: repoA,
		Fingerprint: "fp-verify-a",
		Category:    "type",
		Summary:     "pending issue A",
		Source:      "auto_watch",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: repoB,
		Fingerprint: "fp-verify-b",
		Category:    "type",
		Summary:     "pending issue B",
		Source:      "auto_watch",
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	tools := buildMCPTools(repoA)
	verifyTool := findMCPTool(t, tools, "sg_verify")
	res, err := verifyTool.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("sg_verify(repoA): %v", err)
	}
	payload, ok := res.(mcpVerifyResponse)
	if !ok {
		t.Fatalf("unexpected sg_verify response type %T", res)
	}
	for _, item := range payload.PendingInbox {
		if item.Fingerprint == "fp-verify-b" {
			t.Fatalf("sg_verify leaked repoB pending inbox: %+v", payload.PendingInbox)
		}
	}
	if payload.PendingInboxCount != 1 || len(payload.PendingInbox) != 1 || payload.PendingInbox[0].Fingerprint != "fp-verify-a" {
		t.Fatalf("sg_verify pending inbox = %+v", payload.PendingInbox)
	}
}

func TestP0MCPStoreListCandidatesScopedRequiresProject(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "slopguard.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ListCandidatesScoped(context.Background(), "", 2); !errors.Is(err, store.ErrProjectScopeRequired) {
		t.Fatalf("empty project err = %v, want ErrProjectScopeRequired", err)
	}
}

func TestGlobalMCPProjectPathAndDisallowedRoots(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "slopguard.db")
	oldOpen := openMCPStore
	openMCPStore = func() (*store.Store, error) {
		return store.Open(dbPath)
	}
	t.Cleanup(func() { openMCPStore = oldOpen })

	repoA := t.TempDir()
	repoB := t.TempDir()
	globalTools := buildMCPTools("")
	recordTool := findMCPTool(t, globalTools, "sg_record_mistake")
	submitTool := findMCPTool(t, globalTools, "sg_submit_analysis")
	inboxTool := findMCPTool(t, globalTools, "sg_list_inbox")

	// Disallowed filesystem root must be rejected.
	if _, err := recordTool.Handler(context.Background(), json.RawMessage(`{"project_path":"/","category":"ui","summary":"root should fail"}`)); err == nil {
		t.Fatal("sg_record_mistake with project_path='/' must fail")
	}

	// Concrete project_path works in global MCP mode and scopes cleanly.
	recPayload, _ := json.Marshal(map[string]any{
		"project_path": repoA,
		"category":     "architecture",
		"summary":      "repoA mistake",
	})
	recRes, err := recordTool.Handler(context.Background(), recPayload)
	if err != nil {
		t.Fatalf("sg_record_mistake(repoA): %v", err)
	}
	recMap, ok := recRes.(map[string]any)
	if !ok {
		t.Fatalf("unexpected record result %T", recRes)
	}
	fp, _ := recMap["fingerprint"].(string)
	if fp == "" {
		t.Fatalf("missing fingerprint in %#v", recRes)
	}

	subPayload, _ := json.Marshal(map[string]any{
		"project_path": repoA,
		"fingerprint":  fp,
		"decision":     "recommend",
		"reason":       "测试原因",
	})
	if _, err := submitTool.Handler(context.Background(), subPayload); err != nil {
		t.Fatalf("sg_submit_analysis(repoA): %v", err)
	}

	// Session root from MCP initialize must block mismatched project_path.
	sessionCtx := mcp.WithSessionRoot(context.Background(), repoA)
	badInboxPayload, _ := json.Marshal(map[string]any{
		"project_path": repoB,
	})
	if _, err := inboxTool.Handler(sessionCtx, badInboxPayload); err == nil {
		t.Fatal("cross-project project_path inside session root must be blocked")
	}

	okInboxRes, err := inboxTool.Handler(sessionCtx, json.RawMessage(`{"review_status":"pending_user"}`))
	if err != nil {
		t.Fatalf("sg_list_inbox(sessionRoot): %v", err)
	}
	items, ok := okInboxRes.([]store.MistakeRecord)
	if !ok || len(items) != 1 || items[0].Fingerprint != fp {
		t.Fatalf("sg_list_inbox items = %#v", okInboxRes)
	}
}
