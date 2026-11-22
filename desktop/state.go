package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"sync"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
)

type scanDetails struct {
	Diagnostics        []contract.Diagnostic      `json:"diagnostics"`
	MissingTools       []contract.ToolRequirement `json:"missing_tools"`
	Warnings           []string                   `json:"warnings"`
	OmittedDiagnostics int                        `json:"omitted_diagnostics"`
}

type scanSnapshot struct {
	At time.Time `json:"at"`
	scanDetails
}

type projectView struct {
	Hidden bool          `json:"hidden,omitempty"`
	Scan   *scanSnapshot `json:"scan,omitempty"`
}

// The desktop keeps presentation state separately from shared scan and proposal history.
type viewState struct {
	mu       sync.RWMutex
	path     string
	projects map[string]projectView
}

func loadViewState(path string) (*viewState, error) {
	state := &viewState{path: path, projects: make(map[string]projectView)}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &state.projects); err != nil {
		return nil, err
	}
	if state.projects == nil {
		state.projects = make(map[string]projectView)
	}
	return state, nil
}

func (s *viewState) get(path string) projectView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.projects[path]
}

func (s *viewState) update(path string, change func(*projectView)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.projects[path]
	after := before
	change(&after)
	if before == after {
		return nil
	}
	next := maps.Clone(s.projects)
	if after == (projectView{}) {
		delete(next, path)
	} else {
		next[path] = after
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := core.WriteFileAtomic(s.path, data, 0600, false, nil); err != nil {
		return err
	}
	s.projects = next
	return nil
}

func detailFromScan(result contract.VerifyResult) scanDetails {
	const maxDiagnostics = 20
	count := min(len(result.Diagnostics), maxDiagnostics)
	diagnostics := make([]contract.Diagnostic, count)
	copy(diagnostics, result.Diagnostics[:count])
	for i := range diagnostics {
		diagnostics[i].CodeSnippet = ""
	}
	return scanDetails{
		Diagnostics: diagnostics, MissingTools: append([]contract.ToolRequirement{}, result.MissingTools...),
		Warnings: append([]string{}, result.Warnings...), OmittedDiagnostics: len(result.Diagnostics) - count,
	}
}
