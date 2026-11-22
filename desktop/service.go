package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HunterLzap/slopguard/internal/bridge"
	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
	"github.com/HunterLzap/slopguard/internal/reviewer"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type directoryPicker interface {
	PickDirectory(context.Context) (string, error)
}

type AppService struct {
	store     *reviewer.Store
	projects  *bridge.Service
	reviews   *reviewer.Reviewer
	agents    bridge.AgentSwitch
	autostart AutostartManager
	picker    directoryPicker
	view      *viewState

	settingsMu  sync.Mutex
	projectMu   sync.Mutex
	agentMu     sync.Mutex
	running     atomic.Bool
	cancel      context.CancelFunc
	workers     sync.WaitGroup
	wake        chan struct{}
	refresh     chan struct{}
	onProposals func(bool)
}

func newAppService(store *reviewer.Store, runner core.ProcessRunner, httpClient reviewer.HTTPClient,
	agents bridge.AgentSwitch, autostart AutostartManager, picker directoryPicker) (*AppService, error) {
	view, err := loadViewState(filepath.Join(agents.ConfigDir, "desktop-state.json"))
	if err != nil {
		return nil, fmt.Errorf("读取桌面视图状态：%w", err)
	}
	return &AppService{
		store:    store,
		projects: bridge.NewService(store, runner, httpClient),
		reviews:  &reviewer.Reviewer{Store: store, Rules: core.Semgrep{Runner: runner}, HTTP: httpClient},
		agents:   agents, autostart: autostart, picker: picker, view: view,
		wake: make(chan struct{}, 1), refresh: make(chan struct{}, 1),
	}, nil
}

func (s *AppService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	ctx, s.cancel = context.WithCancel(ctx)
	s.running.Store(true)
	s.workers.Go(func() {
		s.reviews.RunQueue(ctx, s.wake, func(err error) {
			slog.Error("后台评审失败", "error", err)
		})
	})
	s.workers.Go(func() { s.watchProposals(ctx) })
	notify(s.wake)
	return nil
}

func (s *AppService) ServiceShutdown() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.workers.Wait()
	s.running.Store(false)
	return s.store.Close()
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *AppService) watchProposals(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var previous []string
	check := func() {
		byProject, err := s.store.PendingProposalIDs(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("读取待审批状态失败", "error", err)
			}
			return
		}
		var current []string
		for project, ids := range byProject {
			if !s.view.get(project).Hidden {
				current = append(current, ids...)
			}
		}
		slices.Sort(current)
		if !slices.Equal(previous, current) {
			previous = current
			if s.onProposals != nil {
				s.onProposals(len(current) > 0)
			}
		}
	}
	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		case <-s.refresh:
			check()
		}
	}
}

type ProjectCard struct {
	reviewer.Project
	AppliedRules int    `json:"applied_rules"`
	Error        string `json:"error,omitempty"`
	HookError    string `json:"hook_error,omitempty"`
}

type DashboardState struct {
	GuardRunning bool                    `json:"guard_running"`
	Projects     []ProjectCard           `json:"projects"`
	Agents       []bridge.Agent          `json:"agents"`
	Proposals    []contract.RuleProposal `json:"proposals"`
	AgentError   string                  `json:"agent_error,omitempty"`
}

func (s *AppService) GetDashboardState(ctx context.Context) (DashboardState, error) {
	state := DashboardState{GuardRunning: s.running.Load(), Projects: []ProjectCard{}, Proposals: []contract.RuleProposal{}, Agents: []bridge.Agent{}}
	agents, err := s.agents.List()
	if err != nil {
		state.AgentError = err.Error()
	} else {
		state.Agents = agents
	}
	projects, err := s.store.RegisteredProjects(ctx)
	if err != nil {
		return state, err
	}
	for _, p := range projects {
		if s.view.get(p.Path).Hidden {
			continue
		}
		card := ProjectCard{Project: p}
		card.HookEnabled = false
		if _, err := core.ProjectPath(p.Path); err != nil {
			card.Error = err.Error()
		} else {
			rules, err := reviewer.ListAppliedRules(p.Path)
			if err != nil {
				card.Error = err.Error()
			} else {
				for _, rule := range rules {
					if rule.Enabled {
						card.AppliedRules++
					}
				}
			}
			card.HookEnabled, err = s.projects.HookEnabled(ctx, p.Path)
			if err != nil {
				card.HookError = err.Error()
			}
		}
		state.Projects = append(state.Projects, card)
		proposals, err := s.store.ListProposals(ctx, p.Path)
		if err != nil {
			return state, err
		}
		state.Proposals = append(state.Proposals, proposals...)
	}
	return state, nil
}

type Registration struct {
	Cancelled     bool   `json:"cancelled"`
	ProjectPath   string `json:"project_path,omitempty"`
	AlreadyExists bool   `json:"already_exists"`
}

func (s *AppService) PickAndRegisterProject(ctx context.Context) (Registration, error) {
	selected, err := s.picker.PickDirectory(ctx)
	if err != nil {
		return Registration{}, err
	}
	if selected == "" {
		return Registration{Cancelled: true}, nil
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	path, err := core.ProjectPath(selected)
	if err != nil {
		return Registration{}, err
	}
	config, err := s.projects.Init(ctx, path)
	if err != nil {
		return Registration{}, err
	}
	if err := s.view.update(path, func(p *projectView) { p.Hidden = false }); err != nil {
		return Registration{}, err
	}
	notify(s.refresh)
	return Registration{ProjectPath: path, AlreadyExists: config.AlreadyExists}, nil
}

func (s *AppService) RemoveProject(ctx context.Context, path string) (bool, error) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	projects, err := s.store.RegisteredProjects(ctx)
	if err != nil {
		return false, err
	}
	if !slices.ContainsFunc(projects, func(p reviewer.Project) bool { return p.Path == path }) {
		return false, fmt.Errorf("项目未登记：%s", path)
	}
	if err := s.view.update(path, func(p *projectView) { p.Hidden = true }); err != nil {
		return false, err
	}
	notify(s.refresh)
	return true, nil
}

func (s *AppService) VerifyProject(ctx context.Context, path string, scope contract.VerifyScope) (contract.VerifyResult, error) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	result, err := s.projects.Verify(ctx, path, scope, false)
	if err != nil {
		return result, err
	}
	finished := time.Now()
	notify(s.wake)
	project, err := s.store.Project(ctx, result.ProjectPath)
	if err != nil {
		return result, err
	}
	if project.LastScanAt != nil && !project.LastScanAt.After(finished) && project.LastStatus == result.Status {
		snapshot := &scanSnapshot{At: *project.LastScanAt, scanDetails: detailFromScan(result)}
		if err := s.view.update(result.ProjectPath, func(p *projectView) { p.Scan = snapshot }); err != nil {
			return result, fmt.Errorf("检查已完成，保存诊断摘要失败：%w", err)
		}
	}
	return result, nil
}

type ProjectDetails struct {
	reviewer.Project
	scanDetails
	Rules []reviewer.AppliedRule `json:"rules"`
}

func (s *AppService) GetProjectDetails(ctx context.Context, path string) (ProjectDetails, error) {
	project, err := core.ProjectPath(path)
	if err != nil {
		return ProjectDetails{}, err
	}
	info, err := s.store.Project(ctx, project)
	if err != nil {
		return ProjectDetails{}, err
	}
	rules, err := reviewer.ListAppliedRules(project)
	if err != nil {
		return ProjectDetails{}, err
	}
	details := ProjectDetails{Project: info, Rules: rules}
	snapshot := s.view.get(project).Scan
	if snapshot != nil && info.LastScanAt != nil && snapshot.At.Equal(*info.LastScanAt) {
		details.scanDetails = snapshot.scanDetails
	} else {
		details.Diagnostics, err = s.store.LatestDiagnosticSummaries(ctx, project)
		details.MissingTools = []contract.ToolRequirement{}
		details.Warnings = []string{}
	}
	return details, err
}

func (s *AppService) ToggleProjectRule(ctx context.Context, path, id string, enabled bool) ([]reviewer.AppliedRule, error) {
	path, err := core.ProjectPath(path)
	if err != nil {
		return nil, err
	}
	if err := s.reviews.ToggleRule(ctx, path, id, enabled); err != nil {
		return nil, err
	}
	return reviewer.ListAppliedRules(path)
}

func (s *AppService) DeleteProjectRule(ctx context.Context, path, id string) ([]reviewer.AppliedRule, error) {
	path, err := core.ProjectPath(path)
	if err != nil {
		return nil, err
	}
	if err := s.reviews.DeleteAppliedRule(ctx, path, id); err != nil {
		return nil, err
	}
	return reviewer.ListAppliedRules(path)
}

func (s *AppService) ToggleProjectHook(ctx context.Context, path string, enabled bool) (bool, error) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	if err := s.projects.SetHook(ctx, path, enabled); err != nil {
		return false, err
	}
	return s.projects.HookEnabled(ctx, path)
}

func (s *AppService) ApproveProposal(ctx context.Context, path, id string) (contract.RuleProposal, error) {
	proposal, err := s.projects.Approve(ctx, path, id)
	if err == nil {
		notify(s.refresh)
	}
	return proposal, err
}

func (s *AppService) DismissProposal(ctx context.Context, path, id, mode string) (contract.RuleProposal, error) {
	proposal, err := s.projects.Dismiss(ctx, path, id, mode)
	if err == nil {
		notify(s.refresh)
	}
	return proposal, err
}

func (s *AppService) ToggleAgent(id string, enabled bool) ([]bridge.Agent, error) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if err := s.agents.Set(id, enabled); err != nil {
		return nil, err
	}
	return s.agents.List()
}

func (s *AppService) GetGenericMCPSnippets() (bridge.MCPConfig, error) {
	return bridge.ExportGenericMCPConfig(s.agents.Executable)
}

type SettingsView struct {
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	Model          string `json:"model"`
	Autostart      bool   `json:"autostart"`
	AutostartError string `json:"autostart_error,omitempty"`
}

func (s *AppService) GetSettings(ctx context.Context) (SettingsView, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	view := SettingsView{BaseURL: settings.BaseURL, APIKey: settings.APIKey, Model: settings.Model}
	view.Autostart, err = s.autostart.Enabled(ctx)
	if err != nil {
		view.AutostartError = err.Error()
	}
	return view, nil
}

func (s *AppService) SaveSettings(ctx context.Context, baseURL, apiKey, model string, autostart bool) (SettingsView, error) {
	settings, err := llmSettings(baseURL, apiKey, model)
	if err != nil {
		return SettingsView{}, err
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	previous, err := s.store.Settings(ctx)
	if err != nil {
		return SettingsView{}, err
	}
	current, loginErr := s.autostart.Enabled(ctx)
	if loginErr != nil && autostart {
		return SettingsView{}, loginErr
	}
	if err := s.store.UpdateSettings(ctx, settings); err != nil {
		return SettingsView{}, err
	}
	if loginErr == nil && current != autostart {
		if err := s.autostart.SetEnabled(ctx, autostart); err != nil {
			restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			return SettingsView{}, errors.Join(err, s.store.UpdateSettings(restore, previous))
		}
	}
	notify(s.wake)
	view := SettingsView{BaseURL: settings.BaseURL, APIKey: settings.APIKey, Model: settings.Model, Autostart: autostart}
	if loginErr != nil {
		view.AutostartError = loginErr.Error()
	}
	return view, nil
}

func (s *AppService) TestLLMConnection(ctx context.Context, baseURL, apiKey, model string) (bool, error) {
	settings, err := llmSettings(baseURL, apiKey, model)
	if err != nil {
		return false, err
	}
	client := reviewer.Client{HTTP: s.reviews.HTTP, Settings: settings}
	if err := client.TestConnection(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func llmSettings(baseURL, apiKey, model string) (reviewer.Settings, error) {
	baseURL, apiKey, model = strings.TrimSpace(baseURL), strings.TrimSpace(apiKey), strings.TrimSpace(model)
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return reviewer.Settings{}, errors.New("API Base URL 必须是有效的 HTTP 或 HTTPS 基础地址")
	}
	if model == "" {
		return reviewer.Settings{}, errors.New("Model 不能为空")
	}
	return reviewer.Settings{BaseURL: baseURL, APIKey: apiKey, Model: model}, nil
}
