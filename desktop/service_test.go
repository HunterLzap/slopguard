//go:build windows || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/bridge"
	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
	"github.com/HunterLzap/slopguard/internal/reviewer"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type testPicker struct{ path string }

func (p *testPicker) PickDirectory(context.Context) (string, error) { return p.path, nil }

type testAutostart struct {
	enabled bool
	failure error
}

func (a *testAutostart) Enabled(context.Context) (bool, error) { return a.enabled, nil }

func (a *testAutostart) SetEnabled(_ context.Context, enabled bool) error {
	if a.failure != nil {
		return a.failure
	}
	a.enabled = enabled
	return nil
}

type testHTTP func(*http.Request) (*http.Response, error)

func (fn testHTTP) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

type withoutSemgrep struct{ core.ExecRunner }

func (r withoutSemgrep) LookPath(bin string) (string, error) {
	if bin == "semgrep" {
		return "", exec.ErrNotFound
	}
	return r.ExecRunner.LookPath(bin)
}

func fixtureService(t *testing.T, runner core.ProcessRunner, transport testHTTP) (*AppService, *testPicker, *testAutostart) {
	t.Helper()
	config, home := t.TempDir(), t.TempDir()
	store, err := reviewer.OpenStore(t.Context(), filepath.Join(config, "slopguard.db"))
	if err != nil {
		t.Fatal(err)
	}
	picker, login := &testPicker{}, &testAutostart{}
	agents := bridge.AgentSwitch{Home: home, ConfigDir: config, Executable: filepath.Join(config, "SlopGuard Desktop.exe")}
	var client reviewer.HTTPClient
	if transport != nil {
		client = &http.Client{Transport: transport}
	}
	service, err := newAppService(store, runner, client, agents, login, picker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.ServiceShutdown(); err != nil {
			t.Error(err)
		}
	})
	return service, picker, login
}

func writeFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := core.WriteFileAtomic(path, []byte(text), 0600, false, nil); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func TestProjectBindings(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	ctx := t.Context()
	s, picker, _ := fixtureService(t, withoutSemgrep{}, nil)
	cancelled, err := s.PickAndRegisterProject(ctx)
	if err != nil || !cancelled.Cancelled {
		t.Fatalf("cancel: %+v, %v", cancelled, err)
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	goFile := filepath.Join(dir, "example.go")
	writeFixture(t, filepath.Join(dir, "go.mod"), "module example.test/desktop\n\ngo 1.23\n")
	writeFixture(t, goFile, "package example\nimport \"fmt\"\nfunc Show(){fmt.Printf(\"%d\", \"wrong\")}\n")
	configFile := filepath.Join(dir, core.ProjectDirectory, "config.yaml")
	original := "# user custom\r\nlanguages: [go]\r\n"
	writeFixture(t, configFile, original)
	picker.path = dir
	registered, err := s.PickAndRegisterProject(ctx)
	if err != nil || registered.Cancelled || !registered.AlreadyExists {
		t.Fatalf("register: %+v, %v", registered, err)
	}
	path := registered.ProjectPath
	if actual, err := os.ReadFile(configFile); err != nil || string(actual) != original {
		t.Fatalf("custom config: %q, %v", actual, err)
	}

	result, err := s.VerifyProject(ctx, path, contract.ScopeAll)
	if err != nil || result.Status != contract.Fail || !slices.Contains(result.ExecutedCheckers, "go") {
		t.Fatalf("native verification: %+v, %v", result, err)
	}
	details, err := s.GetProjectDetails(ctx, path)
	if err != nil || len(details.Diagnostics) == 0 || details.Diagnostics[0].File != "example.go" || details.Diagnostics[0].Line != 3 {
		t.Fatalf("native diagnostics: %+v, %v", details, err)
	}
	// A subsequent CLI scan replaces the desktop snapshot but shares fingerprint summaries.
	if _, err := s.projects.Verify(ctx, path, contract.ScopeAll, false); err != nil {
		t.Fatal(err)
	}
	details, err = s.GetProjectDetails(ctx, path)
	if err != nil || len(details.Diagnostics) == 0 || details.Diagnostics[0].Line != 0 || details.Diagnostics[0].File != "example.go" {
		t.Fatalf("shared summaries: %+v, %v", details, err)
	}
	writeFixture(t, goFile, "package example\nfunc Show(){}\n")
	rulePath := filepath.Join(core.RulesDir(path), "manual.yaml")
	writeFixture(t, rulePath, fixtureRule)
	result, err = s.VerifyProject(ctx, path, contract.ScopeAll)
	if err != nil || result.Status != contract.MissingTool {
		t.Fatalf("missing tool scan: %+v, %v", result, err)
	}
	details, err = s.GetProjectDetails(ctx, path)
	if err != nil || len(details.MissingTools) != 1 || details.MissingTools[0].InstallCommand == "" || len(details.Rules) != 1 || !details.Rules[0].Enabled {
		t.Fatalf("real rules and missing tools: %+v, %v", details, err)
	}
	state, err := s.GetDashboardState(ctx)
	if err != nil || len(state.Projects) != 1 || state.Projects[0].AppliedRules != 1 || state.Projects[0].LastStatus != contract.MissingTool {
		t.Fatalf("registered dashboard: %+v, %v", state, err)
	}
	rules, err := s.ToggleProjectRule(ctx, path, "manual", false)
	if err != nil || len(rules) != 1 || rules[0].Enabled {
		t.Fatalf("disable rule: %+v, %v", rules, err)
	}
	if _, err := os.Stat(rulePath + ".disabled"); err != nil {
		t.Fatal(err)
	}
	rules, err = s.ToggleProjectRule(ctx, path, "manual", true)
	if err != nil || len(rules) != 1 || !rules[0].Enabled {
		t.Fatalf("enable rule: %+v, %v", rules, err)
	}
	rules, err = s.DeleteProjectRule(ctx, path, "manual")
	if err != nil || len(rules) != 0 {
		t.Fatalf("delete rule: %+v, %v", rules, err)
	}
	if enabled, err := s.ToggleProjectHook(ctx, path, true); err != nil || !enabled {
		t.Fatalf("install hook: %t, %v", enabled, err)
	}
	state, err = s.GetDashboardState(ctx)
	if err != nil || !state.Projects[0].HookEnabled || state.Projects[0].AppliedRules != 0 {
		t.Fatalf("dashboard disk state: %+v, %v", state, err)
	}
	if enabled, err := s.ToggleProjectHook(ctx, path, false); err != nil || enabled {
		t.Fatalf("uninstall hook: %t, %v", enabled, err)
	}

	if removed, err := s.RemoveProject(ctx, path); err != nil || !removed {
		t.Fatalf("remove: %t, %v", removed, err)
	}
	state, err = s.GetDashboardState(ctx)
	if err != nil || len(state.Projects) != 0 {
		t.Fatalf("removed dashboard: %+v, %v", state, err)
	}
	reopened, err := loadViewState(s.view.path)
	if err != nil || !reopened.get(path).Hidden {
		t.Fatalf("persisted removal: %v", err)
	}
	for _, file := range []string{goFile, configFile} {
		if _, err := os.Stat(file); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PickAndRegisterProject(ctx); err != nil {
		t.Fatal(err)
	}
	state, err = s.GetDashboardState(ctx)
	if err != nil || len(state.Projects) != 1 {
		t.Fatalf("reregistered dashboard: %+v, %v", state, err)
	}
}

func TestAgentAndSettingsBindings(t *testing.T) {
	ctx := t.Context()
	transport := testHTTP(func(req *http.Request) (*http.Response, error) {
		defer req.Body.Close()
		if req.Method != http.MethodPost || req.URL.String() != "https://fixture.test/v1/chat/completions" ||
			req.Header.Get("Authorization") != "Bearer test-key" {
			return nil, fmt.Errorf("bad request for connectivity fixture")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))}, nil
	})
	s, _, login := fixtureService(t, core.ExecRunner{}, transport)
	settings, err := s.GetSettings(ctx)
	if err != nil || settings.BaseURL != "https://api.deepseek.com/v1" || settings.Model != "deepseek-chat" || settings.Autostart {
		t.Fatalf("default settings: %+v, %v", settings, err)
	}
	const base = "https://fixture.test/v1"
	saved, err := s.SaveSettings(ctx, base, "test-key", "fixture-model", true)
	if err != nil || !saved.Autostart || !login.enabled {
		t.Fatalf("save: %+v, %v", saved, err)
	}
	settings, err = s.GetSettings(ctx)
	if err != nil || settings != saved {
		t.Fatalf("read: %+v, %v", settings, err)
	}
	ok, err := s.TestLLMConnection(ctx, base, "test-key", "fixture-model")
	if err != nil || !ok {
		t.Fatalf("test: %t, %v", ok, err)
	}
	if _, err := s.TestLLMConnection(ctx, base, "wrong-key", "fixture-model"); err == nil {
		t.Fatal("transport error was dropped")
	}
	login.failure = errors.New("denied by fixture")
	if _, err := s.SaveSettings(ctx, base, "replacement", "other", false); !errors.Is(err, login.failure) {
		t.Fatalf("save error: %v", err)
	}
	if after, err := s.GetSettings(ctx); err != nil || after != saved {
		t.Fatalf("rollback: %+v, %v", after, err)
	}

	file := filepath.Join(s.agents.Home, ".cursor", "mcp.json")
	const config = "{\r\n // user comment\r\n \"mcpServers\": { \"other\": { \"command\": \"other\", }, },\r\n}\r\n"
	writeFixture(t, file, config)
	agents, err := s.ToggleAgent("cursor", true)
	if err != nil || len(agents) != 1 || !agents[0].Enabled {
		t.Fatalf("enable: %+v, %v", agents, err)
	}
	snippets, err := s.GetGenericMCPSnippets()
	if err != nil || !json.Valid([]byte(snippets.JSON)) || !strings.Contains(snippets.TOML, "SlopGuard Desktop.exe") {
		t.Fatalf("snippets: %+v, %v", snippets, err)
	}
	agents, err = s.ToggleAgent("cursor", false)
	if err != nil || len(agents) != 1 || agents[0].Enabled {
		t.Fatalf("disable: %+v, %v", agents, err)
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != config {
		t.Fatalf("agent config round trip: %q, %v", content, err)
	}
}

const fixtureRule = `rules:
  - id: bad-format
    languages: [go]
    severity: ERROR
    message: Match the format verb.
    pattern: fmt.Printf("%d", "wrong")
`

type fixtureSemgrep struct{ core.ExecRunner }

func (r fixtureSemgrep) LookPath(bin string) (string, error) {
	if bin == "semgrep" {
		return bin, nil
	}
	return r.ExecRunner.LookPath(bin)
}

func (r fixtureSemgrep) Run(ctx context.Context, dir, bin string, args ...string) (core.ProcessResult, error) {
	if bin != "semgrep" {
		return r.ExecRunner.Run(ctx, dir, bin, args...)
	}
	for i, arg := range args {
		if arg == "--config" {
			b, err := os.ReadFile(args[i+1])
			if err != nil {
				return core.ProcessResult{}, err
			}
			if string(b) != fixtureRule {
				return core.ProcessResult{}, fmt.Errorf("unexpected fixture rule")
			}
		}
	}
	if args[0] == "--validate" {
		return core.ProcessResult{}, nil
	}
	scanned := []string{}
	results := []any{}
	for _, arg := range args {
		if filepath.Ext(arg) != ".go" {
			continue
		}
		data, err := os.ReadFile(arg)
		if err != nil {
			return core.ProcessResult{}, err
		}
		scanned = append(scanned, arg)
		if bytes.Contains(data, []byte("Printf")) {
			results = append(results, map[string]any{"path": arg, "check_id": "bad-format", "start": map[string]int{"line": 3, "col": 1}, "extra": map[string]string{"message": "fixture"}})
		}
	}
	b, err := json.Marshal(map[string]any{"results": results, "paths": map[string]any{"scanned": scanned}})
	return core.ProcessResult{Stdout: string(b)}, err
}

func TestPendingProposalsAndWorker(t *testing.T) {
	ctx := t.Context()
	transport := testHTTP(func(req *http.Request) (*http.Response, error) {
		defer req.Body.Close()
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		result := contract.ReviewerDecision{Decision: contract.HardRule, SummaryZH: "匹配格式符", SemgrepYAML: fixtureRule, RuleID: "bad-format"}
		if bytes.Contains(body, []byte("another-sample")) {
			result = contract.ReviewerDecision{Decision: contract.GuardHint, SummaryZH: "补充类型说明", HintLine: "Keep format argument types clear."}
		}
		decision, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(decision)}}}})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(response))}, err
	})
	s, picker, _ := fixtureService(t, fixtureSemgrep{}, transport)
	ticks := make(chan bool, 8)
	s.onProposals = func(pending bool) { ticks <- pending }
	if err := s.ServiceStartup(ctx, application.ServiceOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSettings(ctx, "https://fixture.test/v1", "test-key", "fixture-model", false); err != nil {
		t.Fatal(err)
	}
	picker.path = t.TempDir()
	writeFixture(t, filepath.Join(picker.path, "go.mod"), "module example.test/reviewer\n\ngo 1.23\n")
	writeFixture(t, filepath.Join(picker.path, "main.go"), "package main\nimport \"fmt\"\nfunc main(){fmt.Printf(\"%d\", \"wrong\")}\n")
	p, err := s.PickAndRegisterProject(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if r, err := s.VerifyProject(ctx, p.ProjectPath, contract.ScopeAll); err != nil || r.Status != contract.Fail {
			t.Fatalf("verify: %+v, %v", r, err)
		}
	}
	wait := func(want bool) {
		t.Helper()
		select {
		case got := <-ticks:
			if got != want {
				t.Fatalf("pending badge = %t, want %t", got, want)
			}
		case <-time.After(12 * time.Second):
			t.Fatalf("pending badge did not switch to %t", want)
		}
	}
	wait(true)
	state, err := s.GetDashboardState(ctx)
	if err != nil || !state.GuardRunning || len(state.Proposals) != 1 || state.Proposals[0].SandboxPositiveHits == 0 {
		t.Fatalf("review dashboard: %+v, %v", state, err)
	}
	approved, err := s.ApproveProposal(ctx, p.ProjectPath, state.Proposals[0].ID)
	if err != nil || approved.Status != contract.ProposalApproved {
		t.Fatalf("approve: %+v, %v", approved, err)
	}
	wait(false)
	state, err = s.GetDashboardState(ctx)
	if err != nil || len(state.Proposals) != 0 || state.Projects[0].AppliedRules != 1 {
		t.Fatalf("applied dashboard: %+v, %v", state, err)
	}

	result := contract.VerifyResult{Status: contract.Fail, Scope: contract.ScopeAll, Diagnostics: []contract.Diagnostic{{
		Language: "go", Checker: "go", RuleID: "another-sample", Severity: "error", Message: "another-sample",
	}}}
	for range 2 {
		if err := s.store.RecordVerifyResult(ctx, p.ProjectPath, result); err != nil {
			t.Fatal(err)
		}
	}
	notify(s.wake)
	wait(true)
	state, err = s.GetDashboardState(ctx)
	if err != nil || len(state.Proposals) != 1 {
		t.Fatalf("second proposal: %+v, %v", state, err)
	}
	pending, err := s.DismissProposal(ctx, p.ProjectPath, state.Proposals[0].ID, "snooze")
	if err != nil || pending.Status != contract.ProposalSnoozed {
		t.Fatalf("snooze: %+v, %v", pending, err)
	}
	wait(false)
}
