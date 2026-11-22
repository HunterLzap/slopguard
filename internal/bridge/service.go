package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
	"github.com/HunterLzap/slopguard/internal/reviewer"
)

type Service struct {
	engine   core.Engine
	reviewer *reviewer.Reviewer
}

func NewService(store *reviewer.Store, runner core.ProcessRunner, client reviewer.HTTPClient) *Service {
	return &Service{engine: core.Engine{Runner: runner}, reviewer: &reviewer.Reviewer{
		Store: store, Rules: core.Semgrep{Runner: runner}, HTTP: client,
	}}
}

type Status struct {
	reviewer.Project
	AppliedRules     int `json:"applied_rules"`
	PendingProposals int `json:"pending_proposals"`
}

func (s *Service) Init(ctx context.Context, path string) (core.InitResult, error) {
	project, err := root(path)
	if err != nil {
		return core.InitResult{}, err
	}
	langs, err := core.DetectLanguages(project)
	if err != nil {
		return core.InitResult{}, err
	}
	result, err := core.InitProjectConfig(project, langs)
	if err != nil {
		return result, err
	}
	if result.AlreadyExists {
		if _, err := core.LoadProjectConfig(project); err != nil {
			return result, fmt.Errorf("preserved existing config: %w", err)
		}
	}
	return result, s.reviewer.Store.RegisterProject(ctx, project, langs)
}

func (s *Service) Verify(ctx context.Context, path string, scope contract.VerifyScope, syncReview bool) (contract.VerifyResult, error) {
	result, err := s.engine.Verify(ctx, path, scope)
	if err != nil {
		return result, err
	}
	if err := s.reviewer.Store.RecordVerifyResult(ctx, result.ProjectPath, result); err != nil {
		return result, fmt.Errorf("record scan: %w", err)
	}
	if syncReview {
		if _, err := s.reviewer.ProcessQueueOnce(ctx); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("review queue: %v", err))
		}
	}
	return result, nil
}

func (s *Service) Status(ctx context.Context, path string) (Status, error) {
	project, err := root(path)
	if err != nil {
		return Status{}, err
	}
	info, err := s.reviewer.Store.Project(ctx, project)
	if err != nil {
		return Status{}, err
	}
	if len(info.Languages) == 0 {
		info.Languages, err = core.DetectLanguages(project)
		if err != nil {
			return Status{}, err
		}
	}
	rules, err := reviewer.ListAppliedRules(project)
	if err != nil {
		return Status{}, err
	}
	proposals, err := s.reviewer.Store.ListProposals(ctx, project)
	if err != nil {
		return Status{}, err
	}
	status := Status{Project: info, PendingProposals: len(proposals)}
	for _, rule := range rules {
		if rule.Enabled {
			status.AppliedRules++
		}
	}
	return status, nil
}

func (s *Service) Proposals(ctx context.Context, path string) ([]contract.RuleProposal, error) {
	project, err := root(path)
	if err != nil {
		return nil, err
	}
	return s.reviewer.Store.ListProposals(ctx, project)
}

func (s *Service) Approve(ctx context.Context, path, id string) (contract.RuleProposal, error) {
	project, err := root(path)
	if err != nil {
		return contract.RuleProposal{}, err
	}
	if strings.TrimSpace(id) == "" {
		return contract.RuleProposal{}, fmt.Errorf("proposal ID is required")
	}
	return s.reviewer.ApproveProposal(ctx, project, id)
}

func (s *Service) Dismiss(ctx context.Context, path, id, mode string) (contract.RuleProposal, error) {
	project, err := root(path)
	if err != nil {
		return contract.RuleProposal{}, err
	}
	if strings.TrimSpace(id) == "" {
		return contract.RuleProposal{}, fmt.Errorf("proposal ID is required")
	}
	return s.reviewer.DismissProposal(ctx, project, id, mode)
}

func root(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("project path is required")
	}
	return core.ProjectPath(path)
}
