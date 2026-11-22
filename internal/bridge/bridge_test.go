package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/core"
	"github.com/HunterLzap/slopguard/internal/reviewer"
	"github.com/HunterLzap/slopguard/skills"
)

func testService(t *testing.T) *Service {
	t.Helper()
	store, err := reviewer.OpenStore(context.Background(), filepath.Join(t.TempDir(), "slopguard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return NewService(store, core.ExecRunner{}, nil)
}

func TestStdioMCP(t *testing.T) {
	s := testService(t)
	project := t.TempDir()
	if err := core.WriteFileAtomic(filepath.Join(project, "readme.txt"), []byte("plain text"), 0600, true, nil); err != nil {
		t.Fatal(err)
	}
	var input, output, logs bytes.Buffer
	requests := []struct {
		id     string
		method string
		params any
	}{
		{"1", "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "1"}}},
		{"", "notifications/initialized", nil},
		{"2", "tools/list", nil},
		{"3", "tools/call", map[string]any{"name": "sg_verify", "arguments": map[string]any{"project_path": project, "scope": "all"}}},
		{"4", "tools/call", map[string]any{"name": "sg_status", "arguments": map[string]any{"project_path": project}}},
		{"5", "tools/call", map[string]any{"name": "sg_list_proposals", "arguments": map[string]any{"project_path": project}}},
		{"6", "tools/call", map[string]any{"name": "sg_apply_proposal", "arguments": map[string]any{"project_path": project, "proposal_id": "p", "confirm": false}}},
		{"7", "tools/call", map[string]any{"name": "sg_dismiss_proposal", "arguments": map[string]any{"project_path": project, "proposal_id": "p", "mode": "snooze"}}},
	}
	for _, r := range requests {
		request := map[string]any{"jsonrpc": "2.0", "method": r.method}
		if r.id != "" {
			request["id"] = r.id
		}
		if r.params != nil {
			request["params"] = r.params
		}
		if err := json.NewEncoder(&input).Encode(request); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ServeMCP(context.Background(), &input, &output, &logs); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	for _, r := range requests {
		if r.id == "" {
			continue
		}
		var response struct {
			ID, Version string
			Error       json.RawMessage
			Result      json.RawMessage
		}
		if err := decoder.Decode(&response); err != nil || response.ID != r.id || len(response.Error) != 0 {
			t.Fatalf("response for %s = %+v, %v", r.id, response, err)
		}
		switch r.id {
		case "1":
			if !bytes.Contains(response.Result, []byte(`"protocolVersion":"2025-06-18"`)) {
				t.Fatalf("initialize = %s", response.Result)
			}
		case "2":
			var list struct{ Tools []toolSpec }
			if err := json.Unmarshal(response.Result, &list); err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tool := range list.Tools {
				names = append(names, tool.Name)
			}
			want := []string{"sg_verify", "sg_status", "sg_list_proposals", "sg_apply_proposal", "sg_dismiss_proposal"}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("tools = %v", names)
			}
		case "3":
			checkContains(t, response.Result, `"status":"UNSUPPORTED"`)
		case "4":
			checkContains(t, response.Result, `"last_status":"UNSUPPORTED"`)
		case "5":
			checkContains(t, response.Result, `"proposals":[]`)
		case "6":
			checkContains(t, response.Result, `"isError":true`, "confirm must be true")
		case "7":
			checkContains(t, response.Result, `"isError":true`, "not found in project")
		}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("unexpected response after requests: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected log: %s", logs.String())
	}
}

func checkContains(t *testing.T, data []byte, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !bytes.Contains(data, []byte(part)) {
			t.Fatalf("%q missing in %s", part, data)
		}
	}
}

func writeFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := core.WriteFileAtomic(path, []byte(text), 0600, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSwitchRoundTrip(t *testing.T) {
	for _, test := range []struct{ id, path, source string }{
		{"cursor", ".cursor/mcp.json", "{\r\n // retain\r\n \"mcpServers\": {\r\n  \"other\": {\"command\": \"tool\"}, // tail\r\n },\r\n \"editor\": 7,\r\n}\r\n"},
		{"codex", ".codex/config.toml", "model = \"something\" # keep\n\n[mcp_servers.other] # plugin\ncommand = '/a/tool'\n\n[features]\nhello = true\n"},
		{"codex", ".codex/config.toml", "mcp_servers = { other = { command = 'other' } } # inline\nmodel = 'custom'\n"},
		{"openclaw", ".openclaw/openclaw.json", "{ // JSON5\n mcp: {servers:{'other':{command:'thing'},},}, gateway:{mode:'local'},\n}\n"},
		{"dsh", ".dsh/cordis.patch.yml", "# user comment\n- insert:\n    - id: other\n      name: 'other-package'\n      config:\n        destination: !!js process.cwd()\n"},
	} {
		t.Run(test.id+"_"+fmt.Sprint(strings.Count(test.source, "\n")), func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, filepath.FromSlash(test.path))
			writeFixture(t, path, test.source)
			toggle := AgentSwitch{Home: home, ConfigDir: t.TempDir(), Executable: filepath.Join(home, "Slop Guard.exe")}
			for i := 0; i < 2; i++ {
				if err := toggle.Set(test.id, true); err != nil {
					t.Fatal(err)
				}
			}
			agents, err := toggle.List()
			if err != nil || len(agents) != 1 || !agents[0].Enabled {
				t.Fatalf("agents = %+v, %v", agents, err)
			}
			skillPath := agents[0].SkillPath
			actual, err := os.ReadFile(skillPath)
			if err != nil || string(actual) != skills.Markdown {
				t.Fatalf("skill = %q, %v", actual, err)
			}
			active, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			checkContains(t, active, "slopguard", "Slop Guard.exe", "other")
			if err := toggle.Set(test.id, false); err != nil {
				t.Fatal(err)
			}
			actual, err = os.ReadFile(path)
			if err != nil || string(actual) != test.source {
				t.Fatalf("round trip = %q, %v; want %q", actual, err, test.source)
			}
			if _, err := os.Stat(skillPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("skill remains: %v", err)
			}
			backups, err := filepath.Glob(filepath.Join(toggle.ConfigDir, "backups", test.id, "*", "config.original"))
			if err != nil || len(backups) != 1 {
				t.Fatalf("snapshots = %v, %v", backups, err)
			}
			got, err := os.ReadFile(backups[0])
			if err != nil || string(got) != test.source {
				t.Fatalf("snapshot = %q, %v", got, err)
			}
		})
	}
}

func TestAgentSwitchPreservesInterveningEdits(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".cursor", "mcp.json")
	source := `{"mcpServers":{"other":{"command":"old"}}}`
	writeFixture(t, path, source)
	a := AgentSwitch{Home: home, ConfigDir: t.TempDir(), Executable: "slopguard"}
	if err := a.Set("cursor", true); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, strings.Replace(string(current), `"old"`, `"user change"`, 1))
	if err := a.Set("cursor", false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != strings.Replace(source, "old", "user change", 1) {
		t.Fatalf("after disable = %s, %v", got, err)
	}
}

func TestAgentClaudeSettingsDetection(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	const config = `{"permissions":{"allow":[]}}`
	writeFixture(t, settings, config)
	a := AgentSwitch{Home: home, ConfigDir: t.TempDir(), Executable: "slopguard"}
	agents, err := a.List()
	if err != nil || len(agents) != 1 || agents[0].ID != "claude" {
		t.Fatalf("agents = %+v, %v", agents, err)
	}
	if err := a.Set("claude", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	if err := a.Set("claude", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed config remains: %v", err)
	}
	if got, err := os.ReadFile(settings); err != nil || string(got) != config {
		t.Fatalf("original settings = %q, %v", got, err)
	}
}

func TestGitHookRoundTrip(t *testing.T) {
	ctx := context.Background()
	app := testService(t)
	dir, err := core.ProjectPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := core.ExecRunner{}
	git, err := r.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if init, err := r.Run(ctx, dir, git, "init", "--quiet"); err != nil || init.ExitCode != 0 {
		t.Fatalf("git init = %+v, %v", init, err)
	}
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	content := "#!/bin/sh\nprintf 'work' # keep the final line without EOL"
	writeFixture(t, hook, content)
	for i := 0; i < 2; i++ {
		if err := app.SetHook(ctx, dir, true); err != nil {
			t.Fatal(err)
		}
	}
	contents, err := os.ReadFile(hook)
	if err != nil || !bytes.HasPrefix(contents, []byte(content)) || bytes.Count(contents, []byte(hookStart)) != 1 ||
		!bytes.Contains(contents, []byte("slopguard verify --scope changed")) {
		t.Fatalf("installed hook = %q, %v", contents, err)
	}
	if err := app.SetHook(ctx, dir, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(hook)
	if err != nil || string(got) != content {
		t.Fatalf("uninstalled hook = %q, %v", got, err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := app.SetHook(ctx, dir, true); err != nil {
		t.Fatal(err)
	}
	if err := app.SetHook(ctx, dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hook); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty pre-commit remains: %v", err)
	}
}
