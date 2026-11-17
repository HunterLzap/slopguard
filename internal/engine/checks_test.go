package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func TestRunChecksOrderAndSpec(t *testing.T) {
	runner := &stubRunner{runFn: func(spec contract.RunSpec) contract.RunResult {
		return contract.RunResult{ExitCode: 0, Stdout: spec.Name + " ok"}
	}}
	checks := []contract.Check{
		{ID: "one", Cmd: []string{"tool-one", "--flag"}, Timeout: 5 * time.Second},
		{ID: "two", Cmd: []string{"tool-two"}},
		{ID: "three", Cmd: []string{"tool-three", "arg"}},
	}
	results := RunChecks(context.Background(), runner, `C:\repo`, checks, 2)
	if len(results) != 3 {
		t.Fatalf("len(results) = %d", len(results))
	}
	for i, want := range []string{"one", "two", "three"} {
		if results[i].Check.ID != want {
			t.Fatalf("results[%d].Check.ID = %q, want %q", i, results[i].Check.ID, want)
		}
		if results[i].Result.ExitCode != 0 || results[i].Result.Stdout == "" {
			t.Fatalf("results[%d] = %+v", i, results[i])
		}
	}
	calls := runner.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].Name != "tool-one" || calls[0].Args[0] != "--flag" || calls[0].Dir != `C:\repo` {
		t.Fatalf("calls[0] = %+v", calls[0])
	}
	if calls[0].Timeout != 5*time.Second {
		t.Fatalf("calls[0].Timeout = %s", calls[0].Timeout)
	}
}

func TestRunChecksEmptyCommand(t *testing.T) {
	results := RunChecks(context.Background(), &stubRunner{}, ".", []contract.Check{{ID: "broken"}}, 1)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d", len(results))
	}
	if results[0].Result.Err == nil || !strings.Contains(results[0].Result.Err.Error(), "empty command") {
		t.Fatalf("result = %+v", results[0].Result)
	}
	if results[0].Failed() {
		t.Fatal("Failed() = true for system error, want false")
	}
}

func TestRunChecksNilRunner(t *testing.T) {
	results := RunChecks(context.Background(), nil, ".", []contract.Check{{ID: "x", Cmd: []string{"tool"}}}, 1)
	if results[0].Result.Err == nil || !strings.Contains(results[0].Result.Err.Error(), "nil runner") {
		t.Fatalf("result = %+v", results[0].Result)
	}
}

func TestRunChecksEmpty(t *testing.T) {
	if results := RunChecks(context.Background(), &stubRunner{}, ".", nil, 2); len(results) != 0 {
		t.Fatalf("len(results) = %d", len(results))
	}
}

func TestCheckResultFailed(t *testing.T) {
	cases := []struct {
		name   string
		result contract.RunResult
		want   bool
	}{
		{"zero", contract.RunResult{ExitCode: 0}, false},
		{"failed", contract.RunResult{ExitCode: 2}, true},
		{"skipped", contract.RunResult{ExitCode: -1, Skipped: true}, false},
		{"system", contract.RunResult{ExitCode: -1, Err: context.DeadlineExceeded}, false},
	}
	for _, tt := range cases {
		if got := (CheckResult{Result: tt.result}).Failed(); got != tt.want {
			t.Errorf("%s: Failed() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
