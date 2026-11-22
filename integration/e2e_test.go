package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
	"github.com/HunterLzap/slopguard/internal/reviewer"
)

const ruleYAML = `rules:
  - id: bad-format
    languages: [go]
    severity: ERROR
    message: Use the matching format verb.
    pattern: fmt.Printf("%d", "wrong")
`

func TestE2E(t *testing.T) {
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	bin := build(t, repo, "")
	t.Run("A_self_scan", func(t *testing.T) {
		result := scan(t, bin, repo, t.TempDir(), contract.ScopeAll, 0)
		if result.Status != contract.Pass || result.ScannedFilesCount == 0 || !slices.Contains(result.ExecutedCheckers, "go") {
			t.Fatalf("self scan = %+v", result)
		}
		t.Logf("%s with %v, %d files", result.Status, result.ExecutedCheckers, result.ScannedFilesCount)
	})
	t.Run("B_review_and_approve", func(t *testing.T) {
		project, config := t.TempDir(), t.TempDir()
		bin := build(t, repo, fixtureMain(t))
		if os.Getenv("SLOPGUARD_E2E_REAL_SEMGREP") == "1" {
			if _, err := exec.LookPath("semgrep"); err != nil {
				t.Fatal("real Semgrep requested:", err)
			}
			t.Log("Using the installed Semgrep executable")
		} else {
			copyBinary(t, bin, filepath.Join(filepath.Dir(bin), exeName("semgrep")))
			t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Log("Using the offline Semgrep fixture executable")
		}
		ctx := context.Background()
		git := exec.CommandContext(ctx, "git", "init", "--quiet")
		git.Dir = project
		if output, err := git.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, output)
		}
		write(t, filepath.Join(project, "go.mod"), "module example.test/e2e\n\ngo 1.23\n")
		write(t, filepath.Join(project, "main.go"), "package main\nimport \"fmt\"\nfunc main(){fmt.Printf(\"%d\", \"wrong\")}\n")
		dbPath := filepath.Join(config, "slopguard.db")
		store, err := reviewer.OpenStore(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		settings, err := store.Settings(ctx)
		if err == nil {
			settings.APIKey = "offline-fixture"
			err = store.UpdateSettings(ctx, settings)
		}
		closeErr := store.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("set fixture credentials: %v %v", err, closeErr)
		}
		for i := 0; i < 3; i++ {
			result := scan(t, bin, project, config, contract.ScopeChanged, 1)
			if result.Status != contract.Fail || !slices.Contains(result.ExecutedCheckers, "go") || len(result.Diagnostics) == 0 {
				t.Fatalf("attempt %d = %+v", i+1, result)
			}
		}
		var proposals []contract.RuleProposal
		decode(t, invoke(t, bin, project, config, 0, "proposals", "--json"), &proposals)
		if len(proposals) != 1 {
			t.Fatalf("want one validated proposal, got %+v", proposals)
		}
		p := proposals[0]
		wantHash := core.SandboxProofHash(ruleYAML, p.SandboxPositiveHits, p.SandboxProjectHits)
		if p.Status != contract.ProposalProposed || p.Decision != contract.HardRule || p.SandboxPositiveHits < 1 ||
			p.SandboxProjectHits < 1 || p.SandboxProofHash != wantHash {
			t.Fatalf("missing sandbox evidence: %+v", p)
		}
		invoke(t, bin, project, config, 0, "approve", "--id", p.ID)
		rulePath := filepath.Join(project, core.ProjectDirectory, "rules", "bad-format.yaml")
		rule, err := os.ReadFile(rulePath)
		if err != nil || string(rule) != ruleYAML {
			t.Fatalf("approved rule = %q, %v", rule, err)
		}
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		}()
		var state contract.ProposalStatus
		var seen int
		err = db.QueryRow(`SELECT p.status, f.occurrence_count FROM rule_proposals p JOIN mistake_fingerprints f
			ON p.project_path=f.project_path AND p.fingerprint=f.fingerprint WHERE p.id=?`, p.ID).Scan(&state, &seen)
		if err != nil || state != contract.ProposalApproved || seen != 3 {
			t.Fatalf("approval in SQLite = %q, %d occurrences, %v", state, seen, err)
		}
		result := scan(t, bin, project, config, contract.ScopeAll, 1)
		if !slices.Contains(result.ExecutedCheckers, "semgrep") || !slices.ContainsFunc(result.Diagnostics, func(d contract.Diagnostic) bool {
			return d.Checker == "semgrep" && d.RuleID == "bad-format" && d.File == "main.go"
		}) {
			t.Fatalf("saved rule did not take effect: %+v", result)
		}
		t.Logf("%d positive hits, %d project hits, SQLite %s; subsequent scan enforced the rule", p.SandboxPositiveHits, p.SandboxProjectHits, state)
	})
	t.Run("C_existing_config", func(t *testing.T) {
		root, config := t.TempDir(), t.TempDir()
		path := filepath.Join(root, core.ProjectDirectory, "config.yaml")
		original := "# keep custom formatting\r\nlanguages: [python, dart] # comment\r\n"
		write(t, path, original)
		out := invoke(t, bin, root, config, 0, "init", "--root", root)
		actual, err := os.ReadFile(path)
		if err != nil || string(actual) != original || !bytes.Contains(out, []byte("Kept existing")) {
			t.Fatalf("init changed existing content: %q, %v; output %q", actual, err, out)
		}
	})
	t.Run("D_stdio_handshake", func(t *testing.T) {
		testStdio(t, bin, t.TempDir(), t.TempDir())
	})
}

func scan(t *testing.T, bin, root, config string, scope contract.VerifyScope, exit int) contract.VerifyResult {
	t.Helper()
	var result contract.VerifyResult
	decode(t, invoke(t, bin, root, config, exit, "verify", "--scope", string(scope), "--json"), &result)
	if result.ExitCode != exit {
		t.Fatalf("exit from JSON = %d, actual exit = %d", result.ExitCode, exit)
	}
	return result
}

func invoke(t *testing.T, bin, root, config string, wantExit int, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir, cmd.Env = root, append(os.Environ(), "SLOPGUARD_CONFIG_DIR="+config)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != wantExit {
		t.Fatalf("slopguard %v: %v\nstdout: %s\nstderr: %s", args, err, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr from %v: %s", args, stderr.String())
	}
	return stdout.Bytes()
}

func testStdio(t *testing.T, bin, project, config string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "mcp")
	cmd.Dir, cmd.Env = project, append(os.Environ(), "SLOPGUARD_CONFIG_DIR="+config)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cancel()
			if err := cmd.Wait(); err == nil {
				t.Error("MCP unexpectedly exited before handshake completed")
			}
		}
	}()
	encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
	send := func(method string, id int, params any) json.RawMessage {
		t.Helper()
		request := map[string]any{"jsonrpc": "2.0", "method": method}
		if id != 0 {
			request["id"] = id
		}
		if params != nil {
			request["params"] = params
		}
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
		if id == 0 {
			return nil
		}
		var response struct {
			Version       string `json:"jsonrpc"`
			ID            int
			Result, Error json.RawMessage
		}
		if err := decoder.Decode(&response); err != nil || response.ID != id || response.Version != "2.0" || len(response.Error) != 0 {
			t.Fatalf("%s response = %+v, %v", method, response, err)
		}
		return response.Result
	}
	var initialized struct {
		ProtocolVersion string
		ServerInfo      struct{ Name string }
	}
	decode(t, send("initialize", 1, map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "black-box", "version": "1"}}), &initialized)
	if initialized.ProtocolVersion != "2025-11-25" || initialized.ServerInfo.Name != "slopguard" {
		t.Fatalf("init = %+v", initialized)
	}
	send("notifications/initialized", 0, nil)
	var list struct{ Tools []struct{ Name string } }
	decode(t, send("tools/list", 2, nil), &list)
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	if !reflect.DeepEqual(names, []string{"sg_verify", "sg_status", "sg_list_proposals", "sg_apply_proposal", "sg_dismiss_proposal"}) {
		t.Fatalf("tool list = %v", names)
	}
	var status struct {
		IsError           bool
		StructuredContent struct {
			Path         string
			AppliedRules int `json:"applied_rules"`
		}
	}
	decode(t, send("tools/call", 3, map[string]any{"name": "sg_status", "arguments": map[string]any{"project_path": project}}), &status)
	path, err := core.ProjectPath(project)
	if err != nil || status.IsError || status.StructuredContent.Path != path || status.StructuredContent.AppliedRules != 0 {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("unexpected MCP stream after stdin EOF: %v", err)
	}
	if err := cmd.Wait(); err != nil || stderr.Len() != 0 {
		t.Fatalf("MCP exit: %v, stderr: %s", err, stderr.String())
	}
}

func build(t *testing.T, repo, overlay string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), exeName("slopguard"))
	args := []string{"build", "-o", bin}
	if overlay != "" {
		file := filepath.Join(t.TempDir(), "overlay.json")
		data, err := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(repo, "cmd", "slopguard", "main.go"): overlay}})
		if err != nil {
			t.Fatal(err)
		}
		write(t, file, string(data))
		args = append(args, "-overlay", file)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", append(args, "./cmd/slopguard")...)
	cmd.Dir, cmd.Env = repo, append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, out)
	}
	return bin
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func copyBinary(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.WriteFileAtomic(to, data, 0700, true, nil); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := core.WriteFileAtomic(name, []byte(content), 0600, true, nil); err != nil {
		t.Fatal(err)
	}
}

func decode(t *testing.T, data []byte, value any) {
	t.Helper()
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

// This executable uses the normal command entry point with a mock HTTP transport.
// The offline Semgrep fixture impersonates only the subprocess boundary.
func fixtureMain(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "main.go")
	write(t, file, strings.ReplaceAll(offlineMain, "FIXTURE_RULE_GO_LITERAL", fmt.Sprintf("%q", ruleYAML)))
	return file
}

const offlineMain = `package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/HunterLzap/slopguard/internal/bridge"
	"github.com/HunterLzap/slopguard/internal/core"
)

const rule = FIXTURE_RULE_GO_LITERAL

type transport struct{}

func (transport) RoundTrip(req *http.Request) (*http.Response,error) {
	defer req.Body.Close()
	b,err:=io.ReadAll(req.Body)
	if err!=nil { return nil,err }
	if req.URL.Path!="/v1/chat/completions" || req.Method!="POST" ||
		req.Header.Get("Authorization")!="Bearer offline-fixture" ||
		!strings.Contains(string(b),"Printf") {
		return nil,fmt.Errorf("invalid offline DeepSeek request")
	}
	decision,err:=json.Marshal(map[string]string{"decision":"HARD_RULE","summary_zh":"检查格式与参数类型","rule_id":"bad-format","semgrep_yaml":rule,"hint_line":""})
	if err!=nil {return nil,err}
	reply,err:=json.Marshal(map[string]any{"choices":[]any{map[string]any{"message":map[string]string{"content":string(decision)}}}})
	if err!=nil {return nil,err}
	return &http.Response{StatusCode:200,Body:io.NopCloser(strings.NewReader(string(reply)))},nil
}

func main() {
	name:=strings.TrimSuffix(filepath.Base(os.Args[0]),".exe")
	if name=="semgrep" {
		if err:=scan(os.Args[1:]);err!=nil {
			fmt.Fprintln(os.Stderr,err)
			os.Exit(2)
		}
		return
	}
	os.Exit(bridge.RunCLI(context.Background(),os.Args[1:],os.Stdin,os.Stdout,os.Stderr,core.ExecRunner{},&http.Client{Transport:transport{}}))
}

func scan(args []string)error{
	if len(args)==0 {return fmt.Errorf("missing arguments")}
	config,found:=false,false
	scanned:=[]string{}
	results:=[]any{}
	for i:=0;i<len(args);i++{
		switch {
		case args[i]=="--config":
			config=true
		case config:
			config=false
			b,err:=os.ReadFile(args[i]);if err!=nil{return err}
			if string(b)!=rule{return fmt.Errorf("fixture rule did not validate")}
			found=true
		case strings.HasPrefix(args[i],"-"):
		case filepath.Ext(args[i])==".go":
			path:=args[i]
			b,err:=os.ReadFile(path);if err!=nil{return err}
			scanned=append(scanned,path)
			for j,line:=range strings.Split(string(b),"\n"){
				if strings.Contains(line,"fmt.Printf(\"%d\", \"wrong\")"){
					results=append(results,map[string]any{"path":path,"check_id":"bad-format","start":map[string]int{"line":j+1,"col":1},"extra":map[string]string{"message":"Use the matching format verb."}})
				}
			}
		}
	}
	if !found || config{return fmt.Errorf("missing local fixture rule")}
	if args[0]=="--validate"{return nil}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"paths":map[string]any{"scanned":scanned},"results":results,"errors":[]any{}})
}
`
