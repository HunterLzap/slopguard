package engine

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
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
	if results := RunChecksConcurrent(context.Background(), &stubRunner{}, ".", nil, 2); len(results) != 0 {
		t.Fatalf("concurrent len(results) = %d", len(results))
	}
}

func TestRunChecksConcurrentOrderAndLimit(t *testing.T) {
	const total = 6
	var active, maxSeen int32
	runner := &stubRunner{runFn: func(contract.RunSpec) contract.RunResult {
		current := atomic.AddInt32(&active, 1)
		for {
			observed := atomic.LoadInt32(&maxSeen)
			if current <= observed || atomic.CompareAndSwapInt32(&maxSeen, observed, current) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return contract.RunResult{ExitCode: 0}
	}}
	checks := make([]contract.Check, total)
	for i := range checks {
		checks[i] = contract.Check{ID: fmt.Sprintf("check-%d", i), Cmd: []string{"tool"}}
	}
	results := RunChecksConcurrent(context.Background(), runner, ".", checks, 2)
	if len(results) != total {
		t.Fatalf("len(results) = %d", len(results))
	}
	for i := range results {
		if results[i].Check.ID != checks[i].ID {
			t.Fatalf("results[%d].Check.ID = %q, want %q", i, results[i].Check.ID, checks[i].ID)
		}
	}
	if got := atomic.LoadInt32(&maxSeen); got > 2 {
		t.Fatalf("max concurrency = %d, want <= 2", got)
	}
}

func TestRunChecksConcurrentErrors(t *testing.T) {
	results := RunChecksConcurrent(context.Background(), nil, ".", []contract.Check{
		{ID: "empty"},
		{ID: "runner", Cmd: []string{"tool"}},
	}, 2)
	if results[0].Result.Err == nil || !strings.Contains(results[0].Result.Err.Error(), "empty command") {
		t.Fatalf("empty cmd result = %+v", results[0].Result)
	}
	if results[1].Result.Err == nil || !strings.Contains(results[1].Result.Err.Error(), "nil runner") {
		t.Fatalf("nil runner result = %+v", results[1].Result)
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
