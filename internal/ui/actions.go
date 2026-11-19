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
	mux.HandleFunc("/api/guard/status", s.handleGuardStatus)
	mux.HandleFunc("/api/guard/toggle", s.handleGuardToggle)
	mux.HandleFunc("/api/sessions/scan", s.handleScanSessions)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"project":       absPath,
		"passed":        verifyCode == 0,
		"init_output":   initOut,
		"verify_output": verifyOut,
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

	var oldText, newText string
	var existed bool
	var err error
	switch action {
	case "install":
		exe, exeErr := resolveSelfBinary()
		if exeErr != nil {
			writeError(w, http.StatusInternalServerError, exeErr.Error())
			return
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
		writeError(w, http.StatusBadRequest, "不支持的预览操作类型")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	diff := buildUnifiedDiff(target.ConfigPath, oldText, newText, 3)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"kind":        "mcp",
		"client_id":   target.ID,
		"target_id":   target.ID,
		"name":        target.Name,
		"scope":       target.Scope,
		"config_path": target.ConfigPath,
		"action":      action,
		"existed":     existed,
		"has_changes": diff != "",
		"has_backup":  target.HasBackup,
		"backup_at":   target.BackupAt,
		"diff":        diff,
	})
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

	exe, err := resolveSelfBinary()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	oldText, newText, _, err := renderMCPUpsert(target.ConfigPath, exe, mcpArgsForClient(target.ID, project))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("生成 MCP 配置失败: %v", err))
		return
	}
	backupPath, _, err := ensureBackup(s.backupsDir(), "mcp", target.ID, target.ConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("备份原配置失败: %v", err))
		return
	}
	if err := writeFileAtomic(target.ConfigPath, []byte(newText), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("写入 MCP 配置失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"client_id":   target.ID,
		"config_path": target.ConfigPath,
		"configured":  true,
		"backup_path": backupPath,
		"diff":        buildUnifiedDiff(target.ConfigPath, oldText, newText, 3),
	})
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

	restoredFromBackup, removedFile, err := restoreFromBackup(s.backupsDir(), "mcp", target.ID, target.ConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !restoredFromBackup {
		_, newText, shouldRemove, remErr := renderMCPRemove(target.ConfigPath)
		if remErr != nil {
			writeError(w, http.StatusBadRequest, remErr.Error())
			return
		}
		if shouldRemove {
			if err := os.Remove(target.ConfigPath); err != nil && !os.IsNotExist(err) {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			removedFile = true
		} else if newText != "" {
			if err := writeFileAtomic(target.ConfigPath, []byte(newText), 0o644); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"client_id":            target.ID,
		"config_path":          target.ConfigPath,
		"configured":           isSlopGuardConfigured(target.ConfigPath),
		"restored_from_backup": restoredFromBackup,
		"removed_file":         removedFile,
	})
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
	project := strings.TrimSpace(req.Project)
	target := findSkillTarget(s.resolveHomeDir(), project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Skill 目标标识")
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action == "" {
		action = "install"
	}

	oldText, existed, err := readFileTextOrEmpty(target.ConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var newText string
	switch action {
	case "install":
		newText = renderSkillContent(target.ID)
	case "restore":
		if _, rec, ok := lookupBackup(s.backupsDir(), "skill", target.ID, target.ConfigPath); ok && rec.Existed {
			newText = string(rec.OriginalBytes)
		} else {
			newText = ""
		}
	default:
		writeError(w, http.StatusBadRequest, "不支持的预览操作类型")
		return
	}

	diff := buildUnifiedDiff(target.ConfigPath, oldText, newText, 3)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"kind":        "skill",
		"target_id":   target.ID,
		"client_id":   target.ID,
		"name":        target.Name,
		"scope":       target.Scope,
		"config_path": target.ConfigPath,
		"action":      action,
		"existed":     existed,
		"has_changes": diff != "",
		"has_backup":  target.HasBackup,
		"backup_at":   target.BackupAt,
		"diff":        diff,
	})
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
	project := strings.TrimSpace(req.Project)
	target := findSkillTarget(s.resolveHomeDir(), project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Skill 目标标识")
		return
	}
	if target.Scope == "global" && !req.Confirmed {
		writeError(w, http.StatusBadRequest, "修改全局规则前必须先预览 Diff 并确认")
		return
	}

	oldText, _, err := readFileTextOrEmpty(target.ConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	newText := renderSkillContent(target.ID)
	backupPath, _, err := ensureBackup(s.backupsDir(), "skill", target.ID, target.ConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("备份原 Skill 失败: %v", err))
		return
	}
	if err := writeFileAtomic(target.ConfigPath, []byte(newText), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("写入 Skill 失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"target_id":   target.ID,
		"config_path": target.ConfigPath,
		"configured":  true,
		"backup_path": backupPath,
		"diff":        buildUnifiedDiff(target.ConfigPath, oldText, newText, 3),
	})
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
	project := strings.TrimSpace(req.Project)
	target := findSkillTarget(s.resolveHomeDir(), project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Skill 目标标识")
		return
	}

	restoredFromBackup, removedFile, err := restoreFromBackup(s.backupsDir(), "skill", target.ID, target.ConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !restoredFromBackup && isSkillConfigured(target.ConfigPath) {
		if err := os.Remove(target.ConfigPath); err != nil && !os.IsNotExist(err) {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		removedFile = true
		cleanupEmptySkillLeafDir("skill", target.ConfigPath)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"target_id":            target.ID,
		"config_path":          target.ConfigPath,
		"configured":           isSkillConfigured(target.ConfigPath),
		"restored_from_backup": restoredFromBackup,
		"removed_file":         removedFile,
	})
}
