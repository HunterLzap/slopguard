package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/mcp"
	"github.com/HunterLzap/slopguard/internal/store"
)

func findMCPTool(t *testing.T, tools []mcp.Tool, name string) mcp.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return mcp.Tool{}
}

func TestMCPAuditAndInboxTools(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "slopguard.db")
	oldOpen := openMCPStore
	openMCPStore = func() (*store.Store, error) {
		return store.Open(dbPath)
	}
	t.Cleanup(func() { openMCPStore = oldOpen })

	root := t.TempDir()
	s, err := openMCPStore()
	if err != nil {
		t.Fatalf("open temp store: %v", err)
	}
	if _, err := s.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: root,
		Fingerprint: "fp-inbox-1",
		Category:    "type",
		Summary:     "avoid explicit any",
		Source:      "auto_verify",
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	tools := buildMCPTools(root)
	inboxTool := findMCPTool(t, tools, "sg_list_inbox")
	res, err := inboxTool.Handler(context.Background(), json.RawMessage(`{"review_status":"pending_ai"}`))
	if err != nil {
		t.Fatalf("sg_list_inbox: %v", err)
	}
	items, ok := res.([]store.MistakeRecord)
	if !ok || len(items) != 1 || items[0].Fingerprint != "fp-inbox-1" {
		t.Fatalf("sg_list_inbox result = %#v", res)
	}

	submitTool := findMCPTool(t, tools, "sg_submit_analysis")
	if _, err := submitTool.Handler(context.Background(), json.RawMessage(`{
		"fingerprint": "fp-inbox-1",
		"decision": "recommend",
		"reason": "多次出现 any 类型逃生舱，建议沉淀为 ESLint 规则",
		"rule_draft": "\"@typescript-eslint/no-explicit-any\": \"error\""
	}`)); err != nil {
		t.Fatalf("sg_submit_analysis: %v", err)
	}

	s2, err := openMCPStore()
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	events, err := s2.ListMCPEvents(context.Background(), root, "", 10)
	if err != nil {
		t.Fatalf("ListMCPEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2 (%+v)", len(events), events)
	}
	pendingUser, err := s2.ListInboxMistakes(context.Background(), root, "pending_user")
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingUser) != 1 || pendingUser[0].AIDecision != "recommend" {
		t.Fatalf("pendingUser = %+v", pendingUser)
	}
}

func TestMCPVerifyReturnsPendingInboxAndNextAction(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "slopguard.db")
	oldOpen := openMCPStore
	openMCPStore = func() (*store.Store, error) {
		return store.Open(dbPath)
	}
	t.Cleanup(func() { openMCPStore = oldOpen })

	root := t.TempDir()
	s, err := openMCPStore()
	if err != nil {
		t.Fatalf("open temp store: %v", err)
	}
	if _, err := s.RecordMistake(context.Background(), contract.Mistake{
		ProjectPath: root,
		Fingerprint: "fp-verify-inbox",
		Category:    "type",
		Summary:     "[pyright:reportOptionalMemberAccess] None access",
		Source:      "auto_watch",
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	tools := buildMCPTools(root)
	verifyTool := findMCPTool(t, tools, "sg_verify")
	res, err := verifyTool.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("sg_verify: %v", err)
	}
	payload, ok := res.(mcpVerifyResponse)
	if !ok {
		t.Fatalf("unexpected sg_verify response type %T", res)
	}
	if payload.PendingInboxCount != 1 || len(payload.PendingInbox) != 1 {
		t.Fatalf("pending inbox = count %d items %+v", payload.PendingInboxCount, payload.PendingInbox)
	}
	if payload.PendingInbox[0].Fingerprint != "fp-verify-inbox" {
		t.Fatalf("pending inbox fingerprint = %q", payload.PendingInbox[0].Fingerprint)
	}
	if payload.NextAction == "" {
		t.Fatalf("expected non-empty NextAction guiding sg_submit_analysis")
	}
}
