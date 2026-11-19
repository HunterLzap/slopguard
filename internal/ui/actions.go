package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func (s *Server) registerActionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/pick-folder", s.handlePickFolder)
	mux.HandleFunc("/api/projects/add", s.handleAddProject)
	mux.HandleFunc("/api/service/toggle", s.handleServiceToggle)
	mux.HandleFunc("/api/baseline/tighten", s.handleBaselineTighten)
	mux.HandleFunc("/api/baseline/reset", s.handleBaselineReset)
	mux.HandleFunc("/api/mcp-clients", s.handleMCPClients)
	mux.HandleFunc("/api/mcp-clients/preview", s.handlePreviewMCPClient)
	mux.HandleFunc("/api/mcp-clients/install", s.handleInstallMCPClient)
	mux.HandleFunc("/api/mcp-clients/restore", s.handleRestoreMCPClient)
	mux.HandleFunc("/api/skills/preview", s.handlePreviewSkill)
	mux.HandleFunc("/api/skills/install", s.handleInstallSkill)
	mux.HandleFunc("/api/skills/restore", s.handleRestoreSkill)
	mux.HandleFunc("/api/shared-skill", s.handleSharedSkill)
	mux.HandleFunc("/api/agents", s.handleAgents)
	mux.HandleFunc("/api/agents/preview", s.handlePreviewAgent)
	mux.HandleFunc("/api/agents/toggle", s.handleToggleAgent)
	mux.HandleFunc("/api/guard/status", s.handleGuardStatus)
	mux.HandleFunc("/api/guard/toggle", s.handleGuardToggle)
	mux.HandleFunc("/api/sessions/scan", s.handleScanSessions)
	mux.HandleFunc("/api/mistakes/analyze-all", s.handleAnalyzeAllMistakes)
}

func resolveSelfBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate slopguard binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
		exe = resolved
	}
	return exe, nil
}

func runSelfCLI(ctx context.Context, args ...string) (string, int, error) {
	exe, err := resolveSelfBinary()
	if err != nil {
		return "", -1, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, exe, args...)
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return string(out), -1, err
		}
	}
	return strings.TrimSpace(string(out)), exitCode, nil
}

func (s *Server) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if runtime.GOOS != "darwin" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        false,
			"supported": false,
			"error":     "当前系统不支持原生文件夹选择器，请直接输入目录路径",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	script := `POSIX path of (choose folder with prompt "选择要接入 SlopGuard 的项目目录")`
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "-128") || strings.Contains(strings.ToLower(msg), "cancel") {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "supported": true, "cancelled": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "supported": true, "error": msg})
		return
	}
	picked := strings.TrimRight(strings.TrimSpace(string(out)), "/")
	if picked == "" {
		picked = "/"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"supported": true,
		"path":      picked,
	})
}

func (s *Server) handleAddProject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	rawPath := strings.TrimSpace(req.Path)
	if rawPath == "" {
		writeError(w, http.StatusBadRequest, "项目路径不能为空")
		return
	}
	absPath, err := filepath.Abs(rawPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("无效路径: %v", err))
		return
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, "所选路径不存在或不是目录")
		return
	}

	initOut, initCode, initErr := runSelfCLI(r.Context(), "init", "--yes", "--root", absPath)
	if initErr != nil || initCode != 0 {
		msg := initOut
		if initErr != nil && msg == "" {
			msg = initErr.Error()
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("初始化失败: %s", msg))
		return
	}

	verifyOut, verifyCode, verifyErr := runSelfCLI(r.Context(), "verify", "--record", "--all", "--root", absPath)
	if verifyErr != nil && verifyCode != 1 {
		msg := verifyOut
		if msg == "" {
			msg = verifyErr.Error()
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("首次体检失败: %s", msg))
		return
	}

	var guardSt *GuardProjectStatus
	if s.guard != nil {
		enableTrue := true
		var hookPtr *bool
		if _, isGitRepo := resolveGitHookPath(absPath); isGitRepo {
			hookTrue := true
			hookPtr = &hookTrue
		}
		if st, guardErr := s.guard.SetProjectConfig(absPath, &enableTrue, hookPtr, defaultGuardDebounceSec); guardErr == nil {
			guardSt = &st
		} else if hookPtr != nil {
			if st, guardErr := s.guard.SetProjectConfig(absPath, &enableTrue, nil, defaultGuardDebounceSec); guardErr == nil {
				guardSt = &st
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"project":       absPath,
		"passed":        verifyCode == 0,
		"init_output":   initOut,
		"verify_output": verifyOut,
		"guard":         guardSt,
	})
}

func (s *Server) handleServiceToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req struct {
		Enable bool `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	sub := "uninstall"
	if req.Enable {
		sub = "install"
	}
	args := []string{"service", sub}
	if req.Enable && s.port > 0 {
		args = append(args, "--port", fmt.Sprintf("%d", s.port))
	}
	out, code, err := runSelfCLI(r.Context(), args...)
	if err != nil || code != 0 {
		msg := out
		if err != nil && msg == "" {
			msg = err.Error()
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("后台服务操作失败: %s", msg))
		return
	}

	installed := false
	if home := s.resolveHomeDir(); home != "" && runtime.GOOS == "darwin" {
		plist := filepath.Join(home, "Library", "LaunchAgents", "com.slopguard.ui.plist")
		_, statErr := os.Stat(plist)
		installed = statErr == nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"installed": installed,
		"message":   out,
	})
}

func (s *Server) handleBaselineTighten(w http.ResponseWriter, r *http.Request) {
	s.runBaselineAction(w, r, "--tighten")
}

func (s *Server) handleBaselineReset(w http.ResponseWriter, r *http.Request) {
	s.runBaselineAction(w, r, "--reset")
}

func (s *Server) runBaselineAction(w http.ResponseWriter, r *http.Request, modeFlag string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req struct {
		Project string `json:"project"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	project := strings.TrimSpace(req.Project)
	if project == "" {
		writeError(w, http.StatusBadRequest, "请先选择项目")
		return
	}
	out, code, err := runSelfCLI(r.Context(), "baseline", modeFlag, "--yes", "--root", project)
	if err != nil || code != 0 {
		msg := out
		if err != nil && msg == "" {
			msg = err.Error()
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("基线更新失败: %s", msg))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"output": out,
	})
}

type targetMutationRequest struct {
	ClientID  string `json:"client_id"`
	TargetID  string `json:"target_id"`
	Project   string `json:"project"`
	Action    string `json:"action"`
	Confirmed bool   `json:"confirmed"`
}

func (r targetMutationRequest) effectiveID() string {
	if strings.TrimSpace(r.TargetID) != "" {
		return strings.TrimSpace(r.TargetID)
	}
	return strings.TrimSpace(r.ClientID)
}

func (s *Server) handleMCPClients(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	home := s.resolveHomeDir()
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	clients := discoverMCPClients(home, project)
	skillTargets := discoverSkillTargets(home, project)
	exe, _ := resolveSelfBinary()
	writeJSON(w, http.StatusOK, map[string]any{
		"binary_path": exe,
		"clients":     clients,
		"skills":      skillTargets,
	})
}

func findMCPClientTarget(home, project, id string) *mcpClientStatus {
	clients := discoverMCPClients(home, project)
	for i := range clients {
		if clients[i].ID == id {
			return &clients[i]
		}
	}
	return nil
}

func findSkillTarget(home, project, id string) *skillTargetStatus {
	targets := discoverSkillTargets(home, project)
	for i := range targets {
		if targets[i].ID == id {
			return &targets[i]
		}
	}
	return nil
}

func readFileTextOrEmpty(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

type targetDiffSummary struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Scope      string `json:"scope"`
	ConfigPath string `json:"config_path"`
	Action     string `json:"action"`
	Existed    bool   `json:"existed"`
	HasChanges bool   `json:"has_changes"`
	HasBackup  bool   `json:"has_backup"`
	BackupAt   string `json:"backup_at,omitempty"`
	Diff       string `json:"diff"`
}

func (s *Server) previewMCPTarget(target mcpClientStatus, project, action string) (targetDiffSummary, int, error) {
	var oldText, newText string
	var existed bool
	var err error
	switch action {
	case "install":
		exe, exeErr := resolveSelfBinary()
		if exeErr != nil {
			return targetDiffSummary{}, http.StatusInternalServerError, exeErr
		}
		oldText, newText, existed, err = renderMCPUpsert(target.ConfigPath, exe, mcpArgsForClient(target.ID, project))
	case "restore":
		oldText, existed, err = readFileTextOrEmpty(target.ConfigPath)
		if err == nil {
			if _, rec, ok := lookupBackup(s.backupsDir(), "mcp", target.ID, target.ConfigPath); ok {
				if rec.Existed {
					newText = string(rec.OriginalBytes)
				} else {
					newText = ""
				}
			} else {
				_, newText, _, err = renderMCPRemove(target.ConfigPath)
			}
		}
	default:
		return targetDiffSummary{}, http.StatusBadRequest, errors.New("不支持的预览操作类型")
	}
	if err != nil {
		return targetDiffSummary{}, http.StatusBadRequest, err
	}
	diff := buildUnifiedDiff(target.ConfigPath, oldText, newText, 3)
	return targetDiffSummary{
		Kind:       "mcp",
		ID:         target.ID,
		Name:       target.Name,
		Scope:      target.Scope,
		ConfigPath: target.ConfigPath,
		Action:     action,
		Existed:    existed,
		HasChanges: diff != "",
		HasBackup:  target.HasBackup,
		BackupAt:   target.BackupAt,
		Diff:       diff,
	}, http.StatusOK, nil
}

func (s *Server) previewSkillTarget(home string, target skillTargetStatus, action string) (targetDiffSummary, int, error) {
	oldText, existed, err := readFileTextOrEmpty(target.ConfigPath)
	if err != nil {
		return targetDiffSummary{}, http.StatusInternalServerError, err
	}
	var newText string
	switch action {
	case "install":
		newText = renderSkillContent(home, target.ID)
	case "restore":
		if _, rec, ok := lookupBackup(s.backupsDir(), "skill", target.ID, target.ConfigPath); ok && rec.Existed {
			newText = string(rec.OriginalBytes)
		} else {
			newText = ""
		}
	default:
		return targetDiffSummary{}, http.StatusBadRequest, errors.New("不支持的预览操作类型")
	}
	diff := buildUnifiedDiff(target.ConfigPath, oldText, newText, 3)
	return targetDiffSummary{
		Kind:       "skill",
		ID:         target.ID,
		Name:       target.Name,
		Scope:      target.Scope,
		ConfigPath: target.ConfigPath,
		Action:     action,
		Existed:    existed,
		HasChanges: diff != "",
		HasBackup:  target.HasBackup,
		BackupAt:   target.BackupAt,
		Diff:       diff,
	}, http.StatusOK, nil
}

func (s *Server) applyMCPTarget(target mcpClientStatus, project, action string) (map[string]any, int, error) {
	if action == "restore" {
		restoredFromBackup, removedFile, err := restoreFromBackup(s.backupsDir(), "mcp", target.ID, target.ConfigPath)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		if !restoredFromBackup {
			_, newText, shouldRemove, remErr := renderMCPRemove(target.ConfigPath)
			if remErr != nil {
				return nil, http.StatusBadRequest, remErr
			}
			if shouldRemove {
				if err := os.Remove(target.ConfigPath); err != nil && !os.IsNotExist(err) {
					return nil, http.StatusInternalServerError, err
				}
				removedFile = true
			} else if newText != "" {
				if err := writeFileAtomic(target.ConfigPath, []byte(newText), 0o644); err != nil {
					return nil, http.StatusInternalServerError, err
				}
			}
		}
		return map[string]any{
			"ok":                   true,
			"client_id":            target.ID,
			"config_path":          target.ConfigPath,
			"configured":           isSlopGuardConfigured(target.ConfigPath),
			"restored_from_backup": restoredFromBackup,
			"removed_file":         removedFile,
		}, http.StatusOK, nil
	}

	exe, err := resolveSelfBinary()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	oldText, newText, _, err := renderMCPUpsert(target.ConfigPath, exe, mcpArgsForClient(target.ID, project))
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("生成 MCP 配置失败: %v", err)
	}
	backupPath, _, err := ensureBackup(s.backupsDir(), "mcp", target.ID, target.ConfigPath)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("备份原配置失败: %v", err)
	}
	if err := writeFileAtomic(target.ConfigPath, []byte(newText), 0o644); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("写入 MCP 配置失败: %v", err)
	}
	return map[string]any{
		"ok":          true,
		"client_id":   target.ID,
		"config_path": target.ConfigPath,
		"configured":  true,
		"backup_path": backupPath,
		"diff":        buildUnifiedDiff(target.ConfigPath, oldText, newText, 3),
	}, http.StatusOK, nil
}

func (s *Server) applySkillTarget(home string, target skillTargetStatus, action string) (map[string]any, int, error) {
	if action == "restore" {
		restoredFromBackup, removedFile, err := restoreFromBackup(s.backupsDir(), "skill", target.ID, target.ConfigPath)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		if !restoredFromBackup && isSkillConfigured(target.ConfigPath) {
			if err := os.Remove(target.ConfigPath); err != nil && !os.IsNotExist(err) {
				return nil, http.StatusInternalServerError, err
			}
			removedFile = true
			cleanupEmptySkillLeafDir("skill", target.ConfigPath)
		}
		return map[string]any{
			"ok":                   true,
			"target_id":            target.ID,
			"config_path":          target.ConfigPath,
			"configured":           isSkillConfigured(target.ConfigPath),
			"restored_from_backup": restoredFromBackup,
			"removed_file":         removedFile,
		}, http.StatusOK, nil
	}

	oldText, _, err := readFileTextOrEmpty(target.ConfigPath)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	newText := renderSkillContent(home, target.ID)
	backupPath, _, err := ensureBackup(s.backupsDir(), "skill", target.ID, target.ConfigPath)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("备份原 Skill 失败: %v", err)
	}
	if err := writeFileAtomic(target.ConfigPath, []byte(newText), 0o644); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("写入 Skill 失败: %v", err)
	}
	return map[string]any{
		"ok":          true,
		"target_id":   target.ID,
		"config_path": target.ConfigPath,
		"configured":  true,
		"backup_path": backupPath,
		"diff":        buildUnifiedDiff(target.ConfigPath, oldText, newText, 3),
	}, http.StatusOK, nil
}

func writeTargetDiffJSON(w http.ResponseWriter, d targetDiffSummary) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"kind":        d.Kind,
		"client_id":   d.ID,
		"target_id":   d.ID,
		"name":        d.Name,
		"scope":       d.Scope,
		"config_path": d.ConfigPath,
		"action":      d.Action,
		"existed":     d.Existed,
		"has_changes": d.HasChanges,
		"has_backup":  d.HasBackup,
		"backup_at":   d.BackupAt,
		"diff":        d.Diff,
	})
}

func (s *Server) handlePreviewMCPClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req targetMutationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	project := strings.TrimSpace(req.Project)
	target := findMCPClientTarget(s.resolveHomeDir(), project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Agent 客户端标识")
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action == "" {
		action = "install"
	}
	d, status, err := s.previewMCPTarget(*target, project, action)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeTargetDiffJSON(w, d)
}

func (s *Server) handleInstallMCPClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req targetMutationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	project := strings.TrimSpace(req.Project)
	target := findMCPClientTarget(s.resolveHomeDir(), project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Agent 客户端标识")
		return
	}
	if target.Scope == "global" && !req.Confirmed {
		writeError(w, http.StatusBadRequest, "修改全局配置前必须先预览 Diff 并确认")
		return
	}
	res, status, err := s.applyMCPTarget(*target, project, "install")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRestoreMCPClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req targetMutationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	project := strings.TrimSpace(req.Project)
	target := findMCPClientTarget(s.resolveHomeDir(), project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Agent 客户端标识")
		return
	}
	res, status, err := s.applyMCPTarget(*target, project, "restore")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handlePreviewSkill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req targetMutationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	home := s.resolveHomeDir()
	project := strings.TrimSpace(req.Project)
	target := findSkillTarget(home, project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Skill 目标标识")
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action == "" {
		action = "install"
	}
	d, status, err := s.previewSkillTarget(home, *target, action)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeTargetDiffJSON(w, d)
}

func (s *Server) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req targetMutationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	home := s.resolveHomeDir()
	project := strings.TrimSpace(req.Project)
	target := findSkillTarget(home, project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Skill 目标标识")
		return
	}
	if target.Scope == "global" && !req.Confirmed {
		writeError(w, http.StatusBadRequest, "修改全局规则前必须先预览 Diff 并确认")
		return
	}
	res, status, err := s.applySkillTarget(home, *target, "install")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRestoreSkill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req targetMutationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	home := s.resolveHomeDir()
	project := strings.TrimSpace(req.Project)
	target := findSkillTarget(home, project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Skill 目标标识")
		return
	}
	res, status, err := s.applySkillTarget(home, *target, "restore")
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func findAgentProfileByID(home, project, agentID string) *agentProfile {
	profiles := discoverAgentProfiles(home, project)
	for i := range profiles {
		if profiles[i].ID == agentID {
			return &profiles[i]
		}
	}
	return nil
}

func (s *Server) handleSharedSkill(w http.ResponseWriter, r *http.Request) {
	home := s.resolveHomeDir()
	path := sharedSkillPath(home)
	switch r.Method {
	case http.MethodGet:
		content, isCustom, updatedAt := loadSharedSkill(home)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              true,
			"path":            path,
			"content":         content,
			"default_content": defaultSharedSkillContent(),
			"is_custom":       isCustom,
			"updated_at":      updatedAt,
		})
	case http.MethodPost:
		var req struct {
			Content     string `json:"content"`
			Reset       bool   `json:"reset"`
			SyncEnabled *bool  `json:"sync_enabled"`
			Project     string `json:"project"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON payload")
			return
		}
		if path == "" {
			writeError(w, http.StatusInternalServerError, "无法定位用户主目录")
			return
		}
		if req.Reset {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("恢复默认指令失败: %v", err))
				return
			}
		} else {
			trimmed := strings.TrimSpace(req.Content)
			if trimmed == "" {
				writeError(w, http.StatusBadRequest, "护栏指令内容不能为空")
				return
			}
			if err := writeFileAtomic(path, []byte(trimmed+"\n"), 0o644); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("保存共享护栏指令失败: %v", err))
				return
			}
		}

		syncEnabled := true
		if req.SyncEnabled != nil {
			syncEnabled = *req.SyncEnabled
		}
		synced := make([]string, 0)
		if syncEnabled {
			for _, sk := range discoverSkillTargets(home, strings.TrimSpace(req.Project)) {
				if !sk.Configured {
					continue
				}
				if _, _, err := s.applySkillTarget(home, sk, "install"); err == nil {
					synced = append(synced, sk.ConfigPath)
				}
			}
		}

		content, isCustom, updatedAt := loadSharedSkill(home)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              true,
			"path":            path,
			"content":         content,
			"default_content": defaultSharedSkillContent(),
			"is_custom":       isCustom,
			"updated_at":      updatedAt,
			"synced":          synced,
		})
	default:
		methodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
	}
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	home := s.resolveHomeDir()
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	exe, exeErr := resolveSelfBinary()
	if exeErr != nil {
		exe = "slopguard"
	}
	content, isCustom, updatedAt := loadSharedSkill(home)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"binary_path": exe,
		"agents":      discoverAgentProfiles(home, project),
		"shared_skill": map[string]any{
			"path":            sharedSkillPath(home),
			"content":         content,
			"default_content": defaultSharedSkillContent(),
			"is_custom":       isCustom,
			"updated_at":      updatedAt,
		},
	})
}

func normalizeAgentAction(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "disable", "restore", "remove":
		return "restore"
	default:
		return "install"
	}
}

func (s *Server) handlePreviewAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req struct {
		AgentID string `json:"agent_id"`
		Project string `json:"project"`
		Action  string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	home := s.resolveHomeDir()
	project := strings.TrimSpace(req.Project)
	prof := findAgentProfileByID(home, project, strings.TrimSpace(req.AgentID))
	if prof == nil {
		writeError(w, http.StatusBadRequest, "未知的 Agent 标识")
		return
	}
	if prof.NeedsProject {
		writeError(w, http.StatusBadRequest, "请先在左侧工作区添加或选择具体项目")
		return
	}

	action := normalizeAgentAction(req.Action)
	var diffs []targetDiffSummary
	var combined []string
	var paths []string
	var anyChanges, anyBackup bool
	var backupAt string

	if prof.MCPTarget != nil {
		d, status, err := s.previewMCPTarget(*prof.MCPTarget, project, action)
		if err != nil {
			writeError(w, status, err.Error())
			return
		}
		diffs = append(diffs, d)
		paths = append(paths, d.ConfigPath)
		if d.HasChanges {
			anyChanges = true
		}
		if d.HasBackup {
			anyBackup = true
			if backupAt == "" {
				backupAt = d.BackupAt
			}
		}
		if d.Diff != "" {
			combined = append(combined, d.Diff)
		}
	}
	if prof.SkillTarget != nil {
		d, status, err := s.previewSkillTarget(home, *prof.SkillTarget, action)
		if err != nil {
			writeError(w, status, err.Error())
			return
		}
		diffs = append(diffs, d)
		paths = append(paths, d.ConfigPath)
		if d.HasChanges {
			anyChanges = true
		}
		if d.HasBackup {
			anyBackup = true
			if backupAt == "" {
				backupAt = d.BackupAt
			}
		}
		if d.Diff != "" {
			combined = append(combined, d.Diff)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"kind":        "agent",
		"agent_id":    prof.ID,
		"client_id":   prof.ID,
		"target_id":   prof.ID,
		"name":        prof.Name,
		"scope":       prof.Scope,
		"action":      action,
		"config_path": strings.Join(paths, " + "),
		"has_changes": anyChanges,
		"has_backup":  anyBackup,
		"backup_at":   backupAt,
		"targets":     diffs,
		"diff":        strings.Join(combined, "\n"),
	})
}

func (s *Server) handleToggleAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req struct {
		AgentID   string `json:"agent_id"`
		Project   string `json:"project"`
		Enable    *bool  `json:"enable"`
		Confirmed bool   `json:"confirmed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	home := s.resolveHomeDir()
	project := strings.TrimSpace(req.Project)
	prof := findAgentProfileByID(home, project, strings.TrimSpace(req.AgentID))
	if prof == nil {
		writeError(w, http.StatusBadRequest, "未知的 Agent 标识")
		return
	}
	if prof.NeedsProject {
		writeError(w, http.StatusBadRequest, "请先在左侧工作区添加或选择具体项目")
		return
	}

	enable := !prof.Enabled
	if req.Enable != nil {
		enable = *req.Enable
	}
	if prof.Scope == "global" && !req.Confirmed {
		writeError(w, http.StatusBadRequest, "修改全局 Agent 配置前必须先预览 Diff 并确认")
		return
	}

	action := "restore"
	if enable {
		action = "install"
	}
	results := make([]map[string]any, 0, 2)
	if prof.MCPTarget != nil {
		res, status, err := s.applyMCPTarget(*prof.MCPTarget, project, action)
		if err != nil {
			writeError(w, status, err.Error())
			return
		}
		results = append(results, res)
	}
	if prof.SkillTarget != nil {
		res, status, err := s.applySkillTarget(home, *prof.SkillTarget, action)
		if err != nil {
			writeError(w, status, err.Error())
			return
		}
		results = append(results, res)
	}

	updated := findAgentProfileByID(home, project, prof.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"agent":   updated,
		"results": results,
	})
}

func (s *Server) handleAnalyzeAllMistakes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	var req struct {
		Project string `json:"project"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	project := strings.TrimSpace(req.Project)

	pending, err := s.db.ListInboxMistakes(r.Context(), project, "pending_ai")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	analyzed, recommended, skipped := 0, 0, 0
	for _, m := range pending {
		decision, reason, ruleDraft := analyzeMistakeDeterministic(m)
		if err := s.db.SubmitMistakeAnalysis(r.Context(), m.Fingerprint, decision, reason, ruleDraft); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("写入分析结果失败: %v", err))
			return
		}
		analyzed++
		if decision == "recommend" {
			recommended++
		} else {
			skipped++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"analyzed":    analyzed,
		"recommended": recommended,
		"skipped":     skipped,
	})
}
