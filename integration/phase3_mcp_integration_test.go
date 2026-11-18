package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/store"
)

const mcpEslintWindows = `@echo off
echo [{"filePath":"src/app.ts","messages":[{"ruleId":"no-console","severity":2,"message":"Unexpected console statement","line":1,"column":1}]}]
exit /b 1
`

const mcpEslintUnix = `#!/bin/sh
echo '[{"filePath":"src/app.ts","messages":[{"ruleId":"no-console","severity":2,"message":"Unexpected console statement","line":1,"column":1}]}'
exit 1
`

func writeMCPFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type mcpClient struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	stderr  *bytes.Buffer
	nextID  int
	lines   []string
}

func startMCPClient(t *testing.T, root, home string) *mcpClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	cmd := exec.CommandContext(ctx, slopguardBin, "mcp", "--root", root)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("mcp stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("mcp stdout pipe: %v", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start slopguard mcp: %v", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	client := &mcpClient{t: t, cmd: cmd, stdin: stdin, scanner: scanner, stderr: stderr}
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		cancel()
	})
	return client
}

func (c *mcpClient) writeRequest(value map[string]any) {
	c.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		c.t.Fatalf("marshal mcp request: %v", err)
	}
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		c.t.Fatalf("write mcp request: %v", err)
	}
}

func (c *mcpClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		request["params"] = params
	}
	c.writeRequest(request)
	if !c.scanner.Scan() {
		c.t.Fatalf("mcp: no response for %s\nstderr:\n%s", method, c.stderr.String())
	}
	line := c.scanner.Text()
	c.lines = append(c.lines, line)
	var response map[string]any
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		c.t.Fatalf("mcp: stdout line is not JSON protocol frame: %q: %v", line, err)
	}
	if response["jsonrpc"] != "2.0" {
		c.t.Fatalf("mcp: frame missing jsonrpc 2.0: %v", response)
	}
	gotID, ok := response["id"].(float64)
	if !ok || int(gotID) != id {
		c.t.Fatalf("mcp: response id = %v, want %d", response["id"], id)
	}
	return response
}

func (c *mcpClient) notify(method string, params any) {
	c.t.Helper()
	request := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		request["params"] = params
	}
	c.writeRequest(request)
}

func (c *mcpClient) close() int {
	c.t.Helper()
	_ = c.stdin.Close()
	for c.scanner.Scan() {
		c.lines = append(c.lines, c.scanner.Text())
	}
	err := c.cmd.Wait()
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	c.t.Fatalf("wait slopguard mcp: %v\nstderr:\n%s", err, c.stderr.String())
	return -1
}

func callTool(t *testing.T, client *mcpClient, name string, args map[string]any) (map[string]any, bool, string) {
	t.Helper()
	response := client.call("tools/call", map[string]any{"name": name, "arguments": args})
	if rpcErr, ok := response["error"].(map[string]any); ok {
		t.Fatalf("%s returned JSON-RPC error: %v", name, rpcErr)
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s result = %v, want object", name, response["result"])
	}
	isError, _ := result["isError"].(bool)
	text := ""
	if content, ok := result["content"].([]any); ok && len(content) > 0 {
		if first, ok := content[0].(map[string]any); ok {
			text, _ = first["text"].(string)
		}
	}
	return result, isError, text
}

func structuredMap(t *testing.T, result map[string]any, tool string) map[string]any {
	t.Helper()
	value, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("%s structuredContent = %T %v, want object", tool, result["structuredContent"], result["structuredContent"])
	}
	return value
}

func TestPhase3MCPStdioEndToEnd(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writeMCPFixtureFile(t, filepath.Join(root, "package.json"), `{"name":"mcp-fixture","version":"1.0.0"}`)
	writeMCPFixtureFile(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"strict":true}}`)
	writeMCPFixtureFile(t, filepath.Join(root, "src", "app.ts"), "export function run() { console.log('x'); }\n")
	installStub(t, root, "eslint", mcpEslintWindows, mcpEslintUnix)

	client := startMCPClient(t, root, home)

	initResponse := client.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "qa-agent", "version": "1.0"},
	})
	initResult, _ := initResponse["result"].(map[string]any)
	if initResult["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocolVersion = %v", initResult["protocolVersion"])
	}
	serverInfo, _ := initResult["serverInfo"].(map[string]any)
	if serverInfo["name"] != "slopguard" {
		t.Fatalf("serverInfo = %v", serverInfo)
	}
	client.notify("notifications/initialized", nil)

	listResponse := client.call("tools/list", nil)
	listResult, _ := listResponse["result"].(map[string]any)
	toolsRaw, _ := listResult["tools"].([]any)
	names := map[string]bool{}
	for _, item := range toolsRaw {
		tool, _ := item.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
		if tool["inputSchema"] == nil {
			t.Fatalf("tool %s missing inputSchema", name)
		}
	}
	wantTools := []string{"sg_verify", "sg_radar", "sg_record_mistake", "sg_list_candidates", "sg_dry_run_rule", "sg_apply_rule"}
	if len(names) != len(wantTools) {
		t.Fatalf("tools = %v, want %v", names, wantTools)
	}
	for _, want := range wantTools {
		if !names[want] {
			t.Fatalf("missing tool %s; tools = %v", want, names)
		}
	}

	recordArgs := map[string]any{
		"category":     "type",
		"summary":      "avoid any in exported API",
		"bad_snippet":  "let x: any",
		"good_snippet": "let x: unknown",
	}
	firstResult, firstIsError, firstText := callTool(t, client, "sg_record_mistake", recordArgs)
	if firstIsError {
		t.Fatalf("first sg_record_mistake isError: %s", firstText)
	}
	first := structuredMap(t, firstResult, "sg_record_mistake")
	firstCount, _ := first["count"].(float64)
	if firstCount != 1 {
		t.Fatalf("first count = %v, want 1", first["count"])
	}
	fingerprint, _ := first["fingerprint"].(string)
	if fingerprint == "" {
		t.Fatal("first sg_record_mistake returned empty fingerprint")
	}
	earlyResult, earlyIsError, earlyText := callTool(t, client, "sg_list_candidates", map[string]any{"min_count": 2})
	if earlyIsError {
		t.Fatalf("early sg_list_candidates isError: %s", earlyText)
	}
	earlyCandidates, _ := earlyResult["structuredContent"].([]any)
	if len(earlyCandidates) != 0 {
		t.Fatalf("candidates after one occurrence = %v, want empty", earlyCandidates)
	}

	secondResult, secondIsError, secondText := callTool(t, client, "sg_record_mistake", recordArgs)
	if secondIsError {
		t.Fatalf("second sg_record_mistake isError: %s", secondText)
	}
	second := structuredMap(t, secondResult, "sg_record_mistake")
	if count, _ := second["count"].(float64); count != 2 {
		t.Fatalf("second count = %v, want 2", second["count"])
	}
	if second["fingerprint"] != fingerprint {
		t.Fatalf("duplicate fingerprint = %v, want %s", second["fingerprint"], fingerprint)
	}

	candidatesResult, candidatesIsError, candidatesText := callTool(t, client, "sg_list_candidates", map[string]any{"min_count": 2})
	if candidatesIsError {
		t.Fatalf("sg_list_candidates isError: %s", candidatesText)
	}
	candidates, ok := candidatesResult["structuredContent"].([]any)
	if !ok {
		t.Fatalf("candidates structuredContent = %T %v", candidatesResult["structuredContent"], candidatesResult["structuredContent"])
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %v, want one cluster", candidates)
	}
	candidate, _ := candidates[0].(map[string]any)
	if candidate["fingerprint"] != fingerprint || candidate["summary"] != recordArgs["summary"] {
		t.Fatalf("candidate = %v", candidate)
	}

	if _, err := os.Stat(filepath.Join(root, "eslint.config.mjs")); !os.IsNotExist(err) {
		t.Fatalf("eslint config exists before dry-run: %v", err)
	}
	dryResult, dryIsError, dryText := callTool(t, client, "sg_dry_run_rule", map[string]any{
		"engine":       "eslint",
		"rule_payload": `{"no-console":"error"}`,
		"files":        []any{"src/app.ts"},
		"threshold":    20,
	})
	if dryIsError {
		t.Fatalf("sg_dry_run_rule isError: %s", dryText)
	}
	dry := structuredMap(t, dryResult, "sg_dry_run_rule")
	if dry["rule_id"] != "no-console" {
		t.Fatalf("dry-run rule_id = %v, want derived no-console", dry["rule_id"])
	}
	hits, _ := dry["hits"].(float64)
	if hits < 1 {
		t.Fatalf("dry-run hits = %v, want >= 1 (result=%v)", dry["hits"], dry)
	}
	sampleFiles, _ := dry["sample_files"].([]any)
	foundFile := false
	for _, item := range sampleFiles {
		if item == "src/app.ts" {
			foundFile = true
		}
	}
	if !foundFile {
		t.Fatalf("sample_files = %v, want src/app.ts", sampleFiles)
	}
	if warning, _ := dry["warning"].(string); warning != "" {
		t.Fatalf("dry-run warning = %q, want empty", warning)
	}
	if _, err := os.Stat(filepath.Join(root, "eslint.config.mjs")); !os.IsNotExist(err) {
		t.Fatalf("dry-run polluted project config: %v", err)
	}
	originalPackage, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil || !strings.Contains(string(originalPackage), "mcp-fixture") {
		t.Fatalf("package.json mutated by dry-run: %v", err)
	}

	applyArgs := map[string]any{
		"engine":       "eslint",
		"rule_id":      "no-console",
		"rule_payload": `"error"`,
		"scope":        "project",
		"rationale":    "QA MCP end-to-end",
	}
	rejectedArgs := map[string]any{}
	for key, value := range applyArgs {
		rejectedArgs[key] = value
	}
	rejectedArgs["confirm"] = false
	_, rejectedIsError, rejectedText := callTool(t, client, "sg_apply_rule", rejectedArgs)
	if !rejectedIsError {
		t.Fatalf("sg_apply_rule confirm=false isError = false, text=%s", rejectedText)
	}
	if !strings.Contains(rejectedText, "confirm=true") {
		t.Fatalf("confirm=false text = %q", rejectedText)
	}
	if _, err := os.Stat(filepath.Join(root, "eslint.config.mjs")); !os.IsNotExist(err) {
		t.Fatalf("confirm=false wrote project config: %v", err)
	}

	applyArgs["confirm"] = true
	appliedResult, appliedIsError, appliedText := callTool(t, client, "sg_apply_rule", applyArgs)
	if appliedIsError {
		t.Fatalf("sg_apply_rule confirm=true isError: %s", appliedText)
	}
	applied := structuredMap(t, appliedResult, "sg_apply_rule")
	if applied["applied"] != true {
		t.Fatalf("applied = %v, want true", applied)
	}
	configPath, _ := applied["config_path"].(string)
	expectedConfig := filepath.Join(root, "eslint.config.mjs")
	if !strings.EqualFold(filepath.Clean(configPath), filepath.Clean(expectedConfig)) {
		t.Fatalf("config_path = %q, want %q", configPath, expectedConfig)
	}
	configData, err := os.ReadFile(expectedConfig)
	if err != nil {
		t.Fatalf("read applied config: %v", err)
	}
	if !strings.Contains(string(configData), "no-console") {
		t.Fatalf("applied config missing rule:\n%s", configData)
	}
	diff, _ := applied["diff"].(string)
	if !strings.Contains(diff, "+++ b/eslint.config.mjs") {
		t.Fatalf("apply diff = %q", diff)
	}

	if code := client.close(); code != 0 {
		t.Fatalf("mcp process exit = %d\nstderr:\n%s", code, client.stderr.String())
	}
	for _, line := range client.lines {
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("stdout contained non-protocol output %q: %v", line, err)
		}
		if frame["jsonrpc"] != "2.0" {
			t.Fatalf("stdout frame missing jsonrpc 2.0: %v", frame)
		}
	}

	db, err := store.Open(filepath.Join(home, ".slopguard", "slopguard.db"))
	if err != nil {
		t.Fatalf("open store after mcp session: %v", err)
	}
	defer db.Close()
	rules, err := db.ListRules(context.Background(), "eslint")
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules ledger = %+v, want one row", rules)
	}
	if rules[0].RuleID != "no-console" || rules[0].Scope != "project" || rules[0].Status != "active" {
		t.Fatalf("rules ledger row = %+v", rules[0])
	}
	storedCandidates, err := db.ListCandidates(context.Background(), 2)
	if err != nil || len(storedCandidates) != 1 {
		t.Fatalf("stored candidates = %v, err=%v", storedCandidates, err)
	}
}

func TestPhase3MCPVerifyTool(t *testing.T) {
	home := t.TempDir()
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)
	if res := runCLI(t, root, nil, "init", "--root", root); res.Code != 0 {
		t.Fatalf("init exit = %d: %s", res.Code, res.Stderr)
	}
	client := startMCPClient(t, root, home)
	result, isError, text := callTool(t, client, "sg_verify", map[string]any{"all": true})
	if isError {
		t.Fatalf("sg_verify isError: %s", text)
	}
	structured := structuredMap(t, result, "sg_verify")
	if structured["passed"] != true {
		t.Fatalf("sg_verify passed = %v, want true", structured["passed"])
	}
	if code := client.close(); code != 0 {
		t.Fatalf("mcp exit = %d: %s", code, client.stderr.String())
	}
}
