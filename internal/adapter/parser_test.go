package adapter

import (
	"strings"
	"testing"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func TestParseESLintOutput(t *testing.T) {
	out := `[
	  {
	    "filePath": "src/a.ts",
	    "messages": [
	      {"ruleId": "no-explicit-any", "severity": 2, "message": "Unexpected any", "line": 3, "column": 5},
	      {"ruleId": null, "severity": 1, "message": "some warning", "line": 1, "column": 1}
	    ]
	  }
	]`
	findings := NewTypeScriptAdapter().ParseOutput("eslint", out)
	if len(findings) != 2 {
		t.Fatalf("len(findings) = %d, want 2: %+v", len(findings), findings)
	}
	if findings[0].Rule != "eslint:no-explicit-any" || findings[0].Severity != contract.SeverityError {
		t.Fatalf("finding[0] = %+v", findings[0])
	}
	if findings[1].Rule != "eslint" || findings[1].Severity != contract.SeverityWarn {
		t.Fatalf("finding[1] = %+v", findings[1])
	}
	if findings[0].File != "src/a.ts" || findings[0].Line != 3 || findings[0].Column != 5 {
		t.Fatalf("finding[0] location = %+v", findings[0])
	}
}

func TestParseTSCOutput(t *testing.T) {
	out := strings.Join([]string{
		"src/a.ts(3,5): error TS2322: Type 'number' is not assignable to type 'string'.",
		"src/b.ts:1:2 - warning TS9999: some warning",
		"unrelated noise",
	}, "\n")
	findings := NewTypeScriptAdapter().ParseOutput("tsc", out)
	if len(findings) != 2 {
		t.Fatalf("len(findings) = %d: %+v", len(findings), findings)
	}
	if findings[0].Rule != "tsc:TS2322" || findings[0].Line != 3 || findings[0].Column != 5 || findings[0].Severity != contract.SeverityError {
		t.Fatalf("finding[0] = %+v", findings[0])
	}
	if findings[1].Rule != "tsc:TS9999" || findings[1].Severity != contract.SeverityWarn || findings[1].Line != 1 {
		t.Fatalf("finding[1] = %+v", findings[1])
	}
}

func TestParseKnipOutput(t *testing.T) {
	out := `{"issues":[{"file":"src/a.ts","issues":[{"type":"unused","symbol":"foo","line":2,"col":3,"message":"Unused export"}]}]}`
	findings := NewTypeScriptAdapter().ParseOutput("knip", out)
	if len(findings) != 1 || findings[0].Rule != "knip:unused" || findings[0].Message != "Unused export" {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestParseJSCPDOutput(t *testing.T) {
	out := "Clone found (typescript):\n - src/a.ts [3:1 - 9:20] (6 lines, 80 tokens)\n - src/b.ts [20:1 - 26:20]\n"
	findings := NewTypeScriptAdapter().ParseOutput("jscpd", out)
	if len(findings) != 2 {
		t.Fatalf("len(findings) = %d: %+v", len(findings), findings)
	}
	if findings[0].File != "src/a.ts" || findings[0].Line != 3 || findings[0].Rule != "jscpd:duplicate" {
		t.Fatalf("finding = %+v", findings[0])
	}
	if findings[1].File != "src/b.ts" || findings[1].Line != 20 {
		t.Fatalf("finding[1] = %+v", findings[1])
	}
}

func TestParseRuffOutput(t *testing.T) {
	out := `[{"code":"F401","message":"unused import","filename":"app/main.py","location":{"row":2,"column":8}}]`
	findings := NewPythonAdapter().ParseOutput("ruff", out)
	if len(findings) != 1 || findings[0].Rule != "ruff:F401" || findings[0].Line != 2 || findings[0].Column != 8 {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestParsePyrightOutput(t *testing.T) {
	out := `{"generalDiagnostics":[{"file":"app/main.py","severity":"error","message":"bad type","rule":"reportAssignmentType","range":{"start":{"line":4,"character":9}}}]}`
	findings := NewPythonAdapter().ParseOutput("pyright", out)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].Line != 5 || findings[0].Column != 10 {
		t.Fatalf("pyright should convert 0-based to 1-based: %+v", findings[0])
	}
	if findings[0].Rule != "pyright:reportAssignmentType" || findings[0].Severity != contract.SeverityError {
		t.Fatalf("finding = %+v", findings[0])
	}
}

func TestParseSemgrepOutput(t *testing.T) {
	out := `{"results":[{"check_id":"python.lang.security.audit","path":"app/main.py","start":{"line":7,"col":2},"extra":{"message":"insecure","severity":"ERROR"}}]}`
	findings := NewPythonAdapter().ParseOutput("semgrep", out)
	if len(findings) != 1 || findings[0].Rule != "semgrep:python.lang.security.audit" || findings[0].Line != 7 {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].Severity != contract.SeverityError {
		t.Fatalf("severity = %v", findings[0].Severity)
	}
}

func TestParseOutputUnknownCheck(t *testing.T) {
	if got := NewTypeScriptAdapter().ParseOutput("unknown", "whatever"); got != nil {
		t.Fatalf("TS unknown check = %+v", got)
	}
	if got := NewPythonAdapter().ParseOutput("unknown", "whatever"); got != nil {
		t.Fatalf("Python unknown check = %+v", got)
	}
	if got := (&YAMLAdapter{}).ParseOutput("x", "y"); got != nil {
		t.Fatalf("YAML ParseOutput = %+v, want nil", got)
	}
}

func TestParseFindingsFallback(t *testing.T) {
	failed := contract.RunResult{ExitCode: 2, Stderr: "  fatal: tool exploded \nsecond line"}
	got := ParseFindings(NewTypeScriptAdapter(), "unknown-check", failed)
	if len(got) != 1 || got[0].Rule != "unknown-check" || !strings.Contains(got[0].Message, "fatal: tool exploded") {
		t.Fatalf("fallback = %+v", got)
	}

	passed := contract.RunResult{ExitCode: 0}
	if got := ParseFindings(NewTypeScriptAdapter(), "eslint", passed); len(got) != 0 {
		t.Fatalf("passing result findings = %+v", got)
	}

	system := contract.RunResult{ExitCode: -1, Err: errTest}
	got = ParseFindings(nil, "check", system)
	if len(got) != 1 || !strings.Contains(got[0].Message, "检查执行失败") {
		t.Fatalf("system error fallback = %+v", got)
	}
}

var errTest = &testError{}

type testError struct{}

func (*testError) Error() string { return "boom" }
