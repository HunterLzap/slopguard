package engine

import (
	"context"
	"sync"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// stubRunner is a programmable contract.Runner for unit tests.
type stubRunner struct {
	mu    sync.Mutex
	calls []contract.RunSpec
	runFn func(contract.RunSpec) contract.RunResult
	look  func(string) (string, bool)
}

func (s *stubRunner) Run(_ context.Context, spec contract.RunSpec) contract.RunResult {
	s.mu.Lock()
	s.calls = append(s.calls, spec)
	s.mu.Unlock()
	if s.runFn != nil {
		return s.runFn(spec)
	}
	return contract.RunResult{ExitCode: 0}
}

func (s *stubRunner) LookPath(name string) (string, bool) {
	if s.look != nil {
		return s.look(name)
	}
	return name, true
}

func (s *stubRunner) Calls() []contract.RunSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contract.RunSpec(nil), s.calls...)
}
