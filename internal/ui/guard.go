package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HunterLzap/slopguard/internal/engine"
)

const (
	defaultGuardDebounceSec = 5
	defaultGuardPoll        = 3 * time.Second
	maxGuardWatchFiles      = 5000
	guardMaxWorkers         = 2

	gitHookStartMarker = "# >>> slopguard-auto-guard >>>"
	gitHookEndMarker   = "# <<< slopguard-auto-guard <<<"
)

type GuardProjectConfig struct {
	Project     string `json:"project"`
	Enabled     bool   `json:"enabled"`
	HookEnabled bool   `json:"hook_enabled"`
	DebounceSec int    `json:"debounce_sec"`
	UpdatedAt   string `json:"updated_at"`
}

type GuardRuntimeState struct {
	Running      bool   `json:"running"`
	LastScanAt   string `json:"last_scan_at,omitempty"`
	LastPassed   *bool  `json:"last_passed,omitempty"`
	LastFindings int    `json:"last_findings"`
	LastError    string `json:"last_error,omitempty"`
	LastTrigger  string `json:"last_trigger,omitempty"`
}

type GuardProjectStatus struct {
	Project       string `json:"project"`
	Enabled       bool   `json:"enabled"`
	HookEnabled   bool   `json:"hook_enabled"`
	HookInstalled bool   `json:"hook_installed"`
	HookPath      string `json:"hook_path,omitempty"`
	IsGitRepo     bool   `json:"is_git_repo"`
	DebounceSec   int    `json:"debounce_sec"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	GuardRuntimeState
}

type guardWatchState struct {
	lastTreeHash    string
	lastHeadHash    string
	dirtySince      time.Time
	headChangedOnly bool
}

// GuardManager manages background file-change debounce watching and non-blocking Git hooks.
type GuardManager struct {
	server       *Server
	mu           sync.Mutex
	configs      map[string]GuardProjectConfig
	loadedHome   string
	runtime      map[string]GuardRuntimeState
	watch        map[string]*guardWatchState
	pollInterval time.Duration
	sem          chan struct{}

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newGuardManager(server *Server) *GuardManager {
	return &GuardManager{
		server:       server,
		configs:      make(map[string]GuardProjectConfig),
		runtime:      make(map[string]GuardRuntimeState),
		watch:        make(map[string]*guardWatchState),
		pollInterval: defaultGuardPoll,
		sem:          make(chan struct{}, guardMaxWorkers),
	}
}

func (g *GuardManager) configPath() string {
	home := ""
	if g != nil && g.server != nil {
		home = g.server.resolveHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".slopguard", "guard.json")
}

func (g *GuardManager) ensureLoadedLocked() {
	home := ""
	if g.server != nil {
		home = g.server.resolveHomeDir()
	}
	if home == g.loadedHome && g.configs != nil {
		return
	}
	g.loadedHome = home
	g.configs = make(map[string]GuardProjectConfig)
	path := g.configPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var parsed map[string]GuardProjectConfig
	if err := json.Unmarshal(data, &parsed); err == nil && parsed != nil {
		g.configs = parsed
	}
}

func (g *GuardManager) saveLocked() error {
	path := g.configPath()
	if path == "" {
		return errors.New("无法定位 ~/.slopguard/guard.json")
	}
	encoded, err := json.MarshalIndent(g.configs, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(path, encoded, 0o644)
}

// Start launches the background debounce watcher loop until Stop or ctx cancellation.
func (g *GuardManager) Start(ctx context.Context) {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.cancel != nil {
		g.mu.Unlock()
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	watchCtx, cancel := context.WithCancel(ctx)
	g.cancel = cancel
	interval := g.pollInterval
	if interval <= 0 {
		interval = defaultGuardPoll
	}
	g.mu.Unlock()

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		g.pollOnce(watchCtx)
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				g.pollOnce(watchCtx)
			}
		}
	}()
}

// Stop halts the background watcher loop and waits for active verifications to finish.
func (g *GuardManager) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	cancel := g.cancel
	g.cancel = nil
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	g.wg.Wait()
}

// CountEnabled returns the number of projects with background guard or git hook enabled.
func (g *GuardManager) CountEnabled() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLoadedLocked()
	count := 0
	for project, cfg := range g.configs {
		if cfg.Enabled || isGitHookInstalled(project) {
			count++
		}
	}
	return count
}

// EnabledMap returns projectPath -> true for projects with active file watch or git hook.
func (g *GuardManager) EnabledMap() map[string]bool {
	out := make(map[string]bool)
	if g == nil {
		return out
	}
	g.mu.Lock()
	g.ensureLoadedLocked()
	projects := make([]string, 0, len(g.configs))
	for project, cfg := range g.configs {
		if cfg.Enabled {
			out[project] = true
		} else if cfg.HookEnabled {
			projects = append(projects, project)
		}
	}
	g.mu.Unlock()
	for _, project := range projects {
		if isGitHookInstalled(project) {
			out[project] = true
		}
	}
	return out
}

func (g *GuardManager) ProjectStatus(project string) GuardProjectStatus {
	project = filepath.Clean(strings.TrimSpace(project))
	if project == "." || project == "" {
		return GuardProjectStatus{}
	}
	g.mu.Lock()
	g.ensureLoadedLocked()
	cfg := g.configs[project]
	rt := g.runtime[project]
	g.mu.Unlock()

	hookPath, isGit := resolveGitHookPath(project)
	hookInstalled := isGit && hasSlopGuardHookBlock(hookPath)
	debounce := cfg.DebounceSec
	if debounce <= 0 {
		debounce = defaultGuardDebounceSec
	}
	return GuardProjectStatus{
		Project:           project,
		Enabled:           cfg.Enabled,
		HookEnabled:       cfg.HookEnabled || hookInstalled,
		HookInstalled:     hookInstalled,
		HookPath:          hookPath,
		IsGitRepo:         isGit,
		DebounceSec:       debounce,
		UpdatedAt:         cfg.UpdatedAt,
		GuardRuntimeState: rt,
	}
}

func (g *GuardManager) pollOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	type watchTarget struct {
		project  string
		debounce time.Duration
	}
	g.mu.Lock()
	g.ensureLoadedLocked()
	var targets []watchTarget
	for project, cfg := range g.configs {
		if !cfg.Enabled {
			continue
		}
		if g.runtime[project].Running {
			continue
		}
		sec := cfg.DebounceSec
		if sec <= 0 {
			sec = defaultGuardDebounceSec
		}
		targets = append(targets, watchTarget{
			project:  project,
			debounce: time.Duration(sec) * time.Second,
		})
	}
	g.mu.Unlock()

	now := time.Now()
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		treeHash, headHash, err := computeProjectSnapshot(t.project)
		if err != nil {
			continue
		}

		var trigger bool
		var lastCommit bool

		g.mu.Lock()
		ws := g.watch[t.project]
		if ws == nil {
			g.watch[t.project] = &guardWatchState{
				lastTreeHash: treeHash,
				lastHeadHash: headHash,
			}
			g.mu.Unlock()
			continue
		}
		if treeHash != ws.lastTreeHash || headHash != ws.lastHeadHash {
			ws.headChangedOnly = (treeHash == ws.lastTreeHash && headHash != ws.lastHeadHash)
			ws.lastTreeHash = treeHash
			ws.lastHeadHash = headHash
			ws.dirtySince = now
		}
		if !ws.dirtySince.IsZero() && now.Sub(ws.dirtySince) >= t.debounce && !g.runtime[t.project].Running {
			ws.dirtySince = time.Time{}
			lastCommit = ws.headChangedOnly
			ws.headChangedOnly = false
			rt := g.runtime[t.project]
			rt.Running = true
			rt.LastTrigger = "watch"
			g.runtime[t.project] = rt
			trigger = true
		}
		g.mu.Unlock()

		if trigger {
			g.spawnBackgroundVerify(ctx, t.project, lastCommit, "auto_watch")
		}
	}
}

func (g *GuardManager) spawnBackgroundVerify(ctx context.Context, project string, lastCommit bool, triggerSource string) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		select {
		case g.sem <- struct{}{}:
			defer func() { <-g.sem }()
		case <-ctx.Done():
			g.finishRun(project, nil, ctx.Err())
			return
		}

		runner := g.server.runner
		if runner == nil {
			runner = engine.NewExecRunner()
		}
		runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()

		res, err := g.server.runVerify(runCtx, runner, project, verifyRequest{
			Project:    project,
			LastCommit: lastCommit,
			Record:     true,
			Source:     triggerSource,
		})
		if err != nil {
			g.finishRun(project, nil, err)
			return
		}
		g.finishRun(project, &res.Passed, nil, len(res.Findings))
	}()
}

func (g *GuardManager) finishRun(project string, passed *bool, runErr error, findingsCount ...int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt := g.runtime[project]
	rt.Running = false
	rt.LastScanAt = time.Now().UTC().Format(time.RFC3339)
	if runErr != nil {
		rt.LastError = runErr.Error()
	} else {
		rt.LastError = ""
		rt.LastPassed = passed
		if len(findingsCount) > 0 {
			rt.LastFindings = findingsCount[0]
		}
	}
	g.runtime[project] = rt
}

func computeProjectSnapshot(root string) (treeHash, headHash string, err error) {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", "", errors.New("project directory unavailable")
	}

	headHasher := sha256.New()
	if gitDir := resolveGitDir(root); gitDir != "" {
		headPath := filepath.Join(gitDir, "HEAD")
		if data, readErr := os.ReadFile(headPath); readErr == nil {
			headHasher.Write(data)
			line := strings.TrimSpace(string(data))
			if strings.HasPrefix(line, "ref:") {
				refRel := strings.TrimSpace(strings.TrimPrefix(line, "ref:"))
				refPath := filepath.Join(gitDir, filepath.FromSlash(refRel))
				if refData, refErr := os.ReadFile(refPath); refErr == nil {
					headHasher.Write(refData)
				}
			}
		}
	}
	headHash = hex.EncodeToString(headHasher.Sum(nil))

	treeHasher := sha256.New()
	count := 0
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, wErr error) error {
		if wErr != nil {
			if path == root {
				return wErr
			}
			return nil
		}
		if entry.IsDir() {
			if path != root && isGuardSkippedDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if count >= maxGuardWatchFiles {
			return fs.SkipAll
		}
		fi, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		fmt.Fprintf(treeHasher, "%s\x00%d\x00%d\n", filepath.ToSlash(rel), fi.Size(), fi.ModTime().UnixNano())
		count++
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return "", "", walkErr
	}
	treeHash = hex.EncodeToString(treeHasher.Sum(nil))
	return treeHash, headHash, nil
}

func isGuardSkippedDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", ".cache", ".pytest_cache", ".ruff_cache", ".mypy_cache",
		".tox", ".nox", ".next", ".nuxt", ".turbo", "coverage",
		"node_modules", "vendor", "dist", "build", "target",
		"__pycache__", ".venv", "venv", ".idea", ".vscode", ".slopguard":
		return true
	default:
		return false
	}
}

func resolveGitDir(project string) string {
	dotGit := filepath.Join(project, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return dotGit
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir:") {
		return ""
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(project, target)
	}
	target = filepath.Clean(target)
	if ti, statErr := os.Stat(target); statErr == nil && ti.IsDir() {
		return target
	}
	return ""
}

func resolveGitHookPath(project string) (hookPath string, isGitRepo bool) {
	gitDir := resolveGitDir(project)
	if gitDir == "" {
		return "", false
	}
	return filepath.Join(gitDir, "hooks", "post-commit"), true
}

func hasSlopGuardHookBlock(hookPath string) bool {
	if hookPath == "" {
		return false
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		return false
	}
	text := string(data)
	return strings.Contains(text, gitHookStartMarker) && strings.Contains(text, gitHookEndMarker)
}

func isGitHookInstalled(project string) bool {
	hookPath, ok := resolveGitHookPath(project)
	return ok && hasSlopGuardHookBlock(hookPath)
}

func buildGitHookBlock(binaryPath string) string {
	quotedExe := "'" + strings.ReplaceAll(binaryPath, "'", "'\"'\"'") + "'"
	return strings.Join([]string{
		gitHookStartMarker,
		fmt.Sprintf("_sg_bin=%s", quotedExe),
		`if [ ! -x "$_sg_bin" ]; then`,
		`  _sg_bin="$(command -v slopguard 2>/dev/null || true)"`,
		`fi`,
		`if [ -n "$_sg_bin" ]; then`,
		`  _sg_root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"`,
		`  nohup "$_sg_bin" verify --record --last-commit --source git_hook --root "$_sg_root" >/dev/null 2>&1 </dev/null &`,
		`fi`,
		gitHookEndMarker,
	}, "\n")
}

func installGitPostCommitHook(backupsDir, project, binaryPath string) (string, error) {
	hookPath, isGit := resolveGitHookPath(project)
	if !isGit {
		return "", errors.New("当前目录不是 Git 仓库，无法安装 post-commit 钩子")
	}
	if _, _, err := ensureBackup(backupsDir, "hook", "post-commit", hookPath); err != nil {
		return "", err
	}
	var existing string
	if data, err := os.ReadFile(hookPath); err == nil {
		existing = string(data)
	}
	block := buildGitHookBlock(binaryPath)
	updated := upsertHookMarkerBlock(existing, block)
	if err := writeFileAtomic(hookPath, []byte(updated), 0o755); err != nil {
		return "", fmt.Errorf("写入 post-commit 钩子失败: %w", err)
	}
	return hookPath, nil
}

func uninstallGitPostCommitHook(backupsDir, project string) (string, error) {
	hookPath, isGit := resolveGitHookPath(project)
	if !isGit {
		return "", nil
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		if os.IsNotExist(err) {
			return hookPath, nil
		}
		return hookPath, err
	}
	stripped := stripHookMarkerBlock(string(data))
	trimmed := strings.TrimSpace(stripped)

	backupPath, rec, hasBackup := lookupBackup(backupsDir, "hook", "post-commit", hookPath)
	if hasBackup {
		if !rec.Existed && (trimmed == "" || trimmed == "#!/bin/sh" || trimmed == "#!/usr/bin/env sh") {
			_, _, restoreErr := restoreFromBackup(backupsDir, "hook", "post-commit", hookPath)
			return hookPath, restoreErr
		}
		if rec.Existed && stripped == string(rec.OriginalBytes) {
			_, _, restoreErr := restoreFromBackup(backupsDir, "hook", "post-commit", hookPath)
			return hookPath, restoreErr
		}
	}

	if trimmed == "" || trimmed == "#!/bin/sh" || trimmed == "#!/usr/bin/env sh" {
		if err := os.Remove(hookPath); err != nil && !os.IsNotExist(err) {
			return hookPath, err
		}
	} else {
		mode := os.FileMode(0o755)
		if info, statErr := os.Stat(hookPath); statErr == nil && info.Mode().Perm() != 0 {
			mode = info.Mode().Perm()
		}
		if err := writeFileAtomic(hookPath, []byte(stripped), mode); err != nil {
			return hookPath, err
		}
	}
	if hasBackup {
		_ = os.Remove(backupPath)
	}
	return hookPath, nil
}

func upsertHookMarkerBlock(existing, block string) string {
	normalized := strings.ReplaceAll(existing, "\r\n", "\n")
	if strings.TrimSpace(normalized) == "" {
		return "#!/bin/sh\n\n" + block + "\n"
	}
	startIdx := strings.Index(normalized, gitHookStartMarker)
	endIdx := strings.Index(normalized, gitHookEndMarker)
	if startIdx >= 0 && endIdx > startIdx {
		afterEnd := endIdx + len(gitHookEndMarker)
		prefix := strings.TrimRight(normalized[:startIdx], "\n")
		suffix := strings.TrimLeft(normalized[afterEnd:], "\n")
		var out strings.Builder
		if prefix != "" {
			out.WriteString(prefix)
			out.WriteString("\n\n")
		}
		out.WriteString(block)
		out.WriteByte('\n')
		if suffix != "" {
			out.WriteByte('\n')
			out.WriteString(suffix)
			if !strings.HasSuffix(suffix, "\n") {
				out.WriteByte('\n')
			}
		}
		return out.String()
	}
	if !strings.HasSuffix(normalized, "\n") {
		normalized += "\n"
	}
	return normalized + "\n" + block + "\n"
}

func stripHookMarkerBlock(existing string) string {
	normalized := strings.ReplaceAll(existing, "\r\n", "\n")
	startIdx := strings.Index(normalized, gitHookStartMarker)
	endIdx := strings.Index(normalized, gitHookEndMarker)
	if startIdx < 0 || endIdx < startIdx {
		return existing
	}
	afterEnd := endIdx + len(gitHookEndMarker)
	prefix := strings.TrimRight(normalized[:startIdx], "\n")
	suffix := strings.TrimLeft(normalized[afterEnd:], "\n")
	switch {
	case prefix == "" && suffix == "":
		return ""
	case prefix == "":
		return suffix
	case suffix == "":
		return prefix + "\n"
	default:
		return prefix + "\n\n" + suffix
	}
}

func (s *Server) handleGuardStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	var current *GuardProjectStatus
	if project != "" {
		abs, err := filepath.Abs(project)
		if err == nil {
			st := s.guard.ProjectStatus(abs)
			current = &st
		}
	}

	s.guard.mu.Lock()
	s.guard.ensureLoadedLocked()
	allProjects := make(map[string]GuardProjectStatus, len(s.guard.configs))
	for proj := range s.guard.configs {
		s.guard.mu.Unlock()
		st := s.guard.ProjectStatus(proj)
		s.guard.mu.Lock()
		if st.Enabled || st.HookInstalled {
			allProjects[proj] = st
		}
	}
	s.guard.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"project":          project,
		"status":           current,
		"guarded_projects": len(allProjects),
		"projects":         allProjects,
	})
}

var errGuardInvalidProject = errors.New("项目目录不存在")

// SetProjectConfig validates a project directory, applies optional Git hook
// changes and watch debounce settings, persists them, and returns live status.
func (g *GuardManager) SetProjectConfig(project string, enabled *bool, hookEnabled *bool, debounceSec int) (GuardProjectStatus, error) {
	rawProject := strings.TrimSpace(project)
	if rawProject == "" {
		return GuardProjectStatus{}, errors.New("请先选择一个项目")
	}
	absProject, err := filepath.Abs(rawProject)
	if err != nil {
		return GuardProjectStatus{}, fmt.Errorf("无效项目路径: %w", err)
	}
	info, err := os.Stat(absProject)
	if err != nil || !info.IsDir() {
		return GuardProjectStatus{}, errGuardInvalidProject
	}

	if hookEnabled != nil {
		backupsDir := ""
		if g.server != nil {
			backupsDir = g.server.backupsDir()
		}
		if *hookEnabled {
			exe, exeErr := resolveSelfBinary()
			if exeErr != nil {
				exe = "slopguard"
			}
			if _, err := installGitPostCommitHook(backupsDir, absProject, exe); err != nil {
				return GuardProjectStatus{}, err
			}
		} else {
			if _, err := uninstallGitPostCommitHook(backupsDir, absProject); err != nil {
				return GuardProjectStatus{}, err
			}
		}
	}

	g.mu.Lock()
	g.ensureLoadedLocked()
	cfg := g.configs[absProject]
	cfg.Project = absProject
	if enabled != nil {
		cfg.Enabled = *enabled
		if *enabled {
			// Prime initial snapshot so toggling on doesn't double-fire stale diffs.
			delete(g.watch, absProject)
		}
	}
	if hookEnabled != nil {
		cfg.HookEnabled = *hookEnabled
	}
	if debounceSec > 0 {
		cfg.DebounceSec = debounceSec
	} else if cfg.DebounceSec <= 0 {
		cfg.DebounceSec = defaultGuardDebounceSec
	}
	cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	g.configs[absProject] = cfg
	saveErr := g.saveLocked()
	g.mu.Unlock()

	if saveErr != nil {
		return GuardProjectStatus{}, fmt.Errorf("保存守护配置失败: %w", saveErr)
	}
	return g.ProjectStatus(absProject), nil
}

func (s *Server) handleGuardToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req struct {
		Project     string `json:"project"`
		Enabled     *bool  `json:"enabled"`
		HookEnabled *bool  `json:"hook_enabled"`
		DebounceSec int    `json:"debounce_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	st, err := s.guard.SetProjectConfig(req.Project, req.Enabled, req.HookEnabled, req.DebounceSec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": st,
	})
}
