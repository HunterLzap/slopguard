package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/engine"
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
	mux.HandleFunc("/api/shared-skill/preview", s.handlePreviewSharedSkill)
	mux.HandleFunc("/api/agents", s.handleAgents)
	mux.HandleFunc("/api/agents/preview", s.handlePreviewAgent)
	mux.HandleFunc("/api/agents/toggle", s.handleToggleAgent)
	mux.HandleFunc("/api/agents/verify", s.handleVerifyAgents)
	mux.HandleFunc("/api/guard/status", s.handleGuardStatus)
	mux.HandleFunc("/api/guard/toggle", s.handleGuardToggleSecure)
	mux.HandleFunc("/api/guard/hook/preview", s.handlePreviewGuardHook)
	mux.HandleFunc("/api/mistakes/apply-recommendations", s.handleApplyAIRecommendations)
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
	res := engine.RunProcess(ctx, engine.ProcessSpec{
		Command:        exe,
		Args:           args,
		Timeout:        120 * time.Second,
		MaxOutputBytes: engine.MaxOutputBytes,
	})
	combined := strings.TrimSpace(res.Stdout)
	if stderr := strings.TrimSpace(res.Stderr); stderr != "" {
		if combined != "" {
			combined += "\n" + stderr
		} else {
			combined = stderr
		}
	}
	return combined, res.ExitCode, res.Err
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
	script := `POSIX path of (choose folder with prompt "选择要接入 SlopGuard 的项目目录")`
	res := engine.RunProcess(r.Context(), engine.ProcessSpec{
		Command:        "osascript",
		Args:           []string{"-e", script},
		Timeout:        60 * time.Second,
		MaxOutputBytes: 64 * 1024,
	})
	if res.Err != nil || res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" && res.Err != nil {
			msg = res.Err.Error()
		}
		if strings.Contains(msg, "-128") || strings.Contains(strings.ToLower(msg), "cancel") {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "supported": true, "cancelled": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "supported": true, "error": msg})
		return
	}
	picked := strings.TrimRight(strings.TrimSpace(res.Stdout), "/")
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
	if !decodeJSONBody(w, r, &req, false) {
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
		// Git Hook installation is a configuration mutation and must be previewed
		// separately; project onboarding only enables the background watcher.
		if st, guardErr := s.guard.SetProjectConfig(absPath, &enableTrue, nil, defaultGuardDebounceSec); guardErr == nil {
			guardSt = &st
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
	if !decodeJSONBody(w, r, &req, false) {
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
	if !decodeJSONBody(w, r, &req, false) {
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
	ClientID       string            `json:"client_id"`
	TargetID       string            `json:"target_id"`
	Project        string            `json:"project"`
	Action         string            `json:"action"`
	PreviewID      string            `json:"preview_id"`
	Confirmed      bool              `json:"confirmed"`
	ExpectedHashes map[string]string `json:"expected_hashes,omitempty"`
}

func verifyExpectedHashes(expected map[string]string, paths ...string) error {
	if len(expected) == 0 {
		return nil
	}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		wantHash, has := expected[p]
		if !has {
			continue
		}
		currHash, existed, err := currentFileSHA256(p)
		if err != nil {
			return fmt.Errorf("检查目标文件版本失败 (%s): %w", p, err)
		}
		if !existed {
			currHash = ""
		}
		if currHash != wantHash {
			return fmt.Errorf("配置文件 %s 在预览后已被外部修改，请重新预览 Diff", p)
		}
	}
	return nil
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
	BaseSHA256 string `json:"base_sha256,omitempty"`
	NewSHA256  string `json:"new_sha256,omitempty"`
	PreviewID  string `json:"preview_id,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	HasChanges bool   `json:"has_changes"`
	HasBackup  bool   `json:"has_backup"`
	BackupAt   string `json:"backup_at,omitempty"`
	Diff       string `json:"diff"`
}

func backupTargetForMCP(target mcpClientStatus) backupTarget {
	return backupTarget{Kind: "mcp", ID: target.ID, Scope: target.Scope, Path: target.ConfigPath}
}

func backupTargetForSkill(target skillTargetStatus) backupTarget {
	return backupTarget{Kind: "skill", ID: target.ID, Scope: target.Scope, Path: target.ConfigPath}
}

func (s *Server) attachPreview(d targetDiffSummary, kind, targetID, project, scope, action string) (targetDiffSummary, error) {
	rec, err := s.createPreview(previewRecord{
		Kind:      kind,
		TargetID:  targetID,
		Project:   project,
		Scope:     scope,
		Action:    action,
		Paths:     map[string]string{d.ConfigPath: d.BaseSHA256},
		NewHashes: map[string]string{d.ConfigPath: d.NewSHA256},
	})
	if err != nil {
		return d, err
	}
	d.PreviewID = rec.ID
	d.ExpiresAt = rec.ExpiresAt.UTC().Format(time.RFC3339)
	return d, nil
}

func mutationStatus(status int, err error) int {
	if err == nil {
		return status
	}
	switch {
	case errors.Is(err, ErrPreviewRequired),
		errors.Is(err, ErrPreviewExpired),
		errors.Is(err, ErrPreviewUsed),
		errors.Is(err, ErrPreviewScopeMismatch),
		errors.Is(err, ErrPreviewConflict),
		errors.Is(err, ErrPreviewContentChanged),
		errors.Is(err, ErrManagedConfigModified),
		errors.Is(err, ErrLegacyBackupNeedsReview),
		errors.Is(err, ErrManagedTargetMissing),
		errors.Is(err, ErrBackupMetadataMismatch):
		return http.StatusConflict
	}
	return status
}

func (s *Server) previewMCPTarget(target mcpClientStatus, project, action string) (targetDiffSummary, int, error) {
	var oldText, newText string
	var existed, deleteFile bool
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
			_, rec, ok, checkErr := lookupBackupChecked(s.backupsDir(), "mcp", target.ID, target.ConfigPath)
			if checkErr != nil {
				return targetDiffSummary{}, mutationStatus(http.StatusConflict, checkErr), checkErr
			}
			if ok {
				plan, planErr := planManagedUninstall(s.backupsDir(), backupTargetForMCP(target), rec)
				if planErr != nil {
					return targetDiffSummary{}, mutationStatus(http.StatusConflict, planErr), planErr
				}
				switch plan.Action {
				case uninstallDeleteFile:
					newText = ""
					deleteFile = true
				case uninstallNoop:
					newText = oldText
				default:
					newText = string(plan.NewData)
				}
			} else {
				var removeFile bool
				_, newText, removeFile, err = renderMCPRemove(target.ConfigPath)
				deleteFile = removeFile
			}
		}
	default:
		return targetDiffSummary{}, http.StatusBadRequest, errors.New("不支持的预览操作类型")
	}
	if err != nil {
		return targetDiffSummary{}, http.StatusBadRequest, err
	}
	baseHash := ""
	if existed {
		baseHash = fileSHA256Hex([]byte(oldText))
	}
	newHash := ""
	if !deleteFile {
		newHash = fileSHA256Hex([]byte(newText))
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
		BaseSHA256: baseHash,
		NewSHA256:  newHash,
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
	deleteFile := false
	switch action {
	case "install":
		rendered := renderSkillContent(home, target.ID)
		newText, err = mergeSkillInstallContent(oldText, rendered)
		if err != nil {
			return targetDiffSummary{}, mutationStatus(http.StatusConflict, err), err
		}
	case "restore":
		_, rec, ok, checkErr := lookupBackupChecked(s.backupsDir(), "skill", target.ID, target.ConfigPath)
		if checkErr != nil {
			return targetDiffSummary{}, mutationStatus(http.StatusConflict, checkErr), checkErr
		}
		if ok {
			plan, planErr := planManagedUninstall(s.backupsDir(), backupTargetForSkill(target), rec)
			if planErr != nil {
				return targetDiffSummary{}, mutationStatus(http.StatusConflict, planErr), planErr
			}
			switch plan.Action {
			case uninstallDeleteFile:
				newText = ""
				deleteFile = true
			case uninstallNoop:
				newText = oldText
			default:
				newText = string(plan.NewData)
			}
		} else if strings.TrimSpace(oldText) != "" {
			// Without a trustworthy backup, only remove a managed block if it is
			// explicitly present; never delete an unknown user-owned file.
			if extractSkillManagedBlock(oldText) == "" {
				return targetDiffSummary{}, http.StatusConflict, ErrLegacyBackupNeedsReview
			}
			newText = removeSkillManagedBlock(oldText)
			deleteFile = strings.TrimSpace(newText) == ""
		}
	default:
		return targetDiffSummary{}, http.StatusBadRequest, errors.New("不支持的预览操作类型")
	}
	baseHash := ""
	if existed {
		baseHash = fileSHA256Hex([]byte(oldText))
	}
	newHash := ""
	if !deleteFile {
		newHash = fileSHA256Hex([]byte(newText))
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
		BaseSHA256: baseHash,
		NewSHA256:  newHash,
		HasChanges: diff != "",
		HasBackup:  target.HasBackup,
		BackupAt:   target.BackupAt,
		Diff:       diff,
	}, http.StatusOK, nil
}

func (s *Server) applyMCPTarget(target mcpClientStatus, project, action string, lease *applyLease) (map[string]any, int, error) {
	if action == "restore" {
		_, rec, hasBackup, checkErr := lookupBackupChecked(s.backupsDir(), "mcp", target.ID, target.ConfigPath)
		if checkErr != nil {
			return nil, mutationStatus(http.StatusConflict, checkErr), checkErr
		}
		removedFile := false
		restoredFromBackup := false
		if hasBackup {
			plan, planErr := planManagedUninstall(s.backupsDir(), backupTargetForMCP(target), rec)
			if planErr != nil {
				return nil, mutationStatus(http.StatusConflict, planErr), planErr
			}
			if lease != nil {
				if err := plan.verify(lease, target.ConfigPath); err != nil {
					return nil, mutationStatus(http.StatusConflict, err), err
				}
			}
			if err := applyManagedUninstall(s.backupsDir(), backupTargetForMCP(target), plan); err != nil {
				return nil, mutationStatus(http.StatusConflict, err), err
			}
			restoredFromBackup = true
			removedFile = plan.Action == uninstallDeleteFile
		} else {
			oldText, newText, shouldRemove, remErr := renderMCPRemove(target.ConfigPath)
			if remErr != nil {
				return nil, mutationStatus(http.StatusConflict, remErr), remErr
			}
			if shouldRemove {
				if lease != nil {
					if err := lease.checkDeletion(target.ConfigPath); err != nil {
						return nil, mutationStatus(http.StatusConflict, err), err
					}
				}
				if err := os.Remove(target.ConfigPath); err != nil && !os.IsNotExist(err) {
					return nil, http.StatusInternalServerError, err
				}
				removedFile = true
			} else if newText != oldText {
				if lease != nil {
					if err := lease.checkNewContent(target.ConfigPath, []byte(newText)); err != nil {
						return nil, mutationStatus(http.StatusConflict, err), err
					}
				}
				mode := existingFileModeOrDefault(target.ConfigPath, defaultModeForTarget(target.Scope, "mcp"))
				if err := writeFileAtomic(target.ConfigPath, []byte(newText), mode); err != nil {
					return nil, http.StatusInternalServerError, err
				}
			}
		}
		if target.ID == "antigravity" && !s.skipRealAgentCLI {
			if syncErr := syncAgyMCPRegistry(s.resolveHomeDir(), "", false); syncErr != nil {
				return nil, http.StatusInternalServerError, fmt.Errorf("已移除配置文件，但同步 Antigravity CLI 失败: %w", syncErr)
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
	if lease != nil {
		if err := lease.checkNewContent(target.ConfigPath, []byte(newText)); err != nil {
			return nil, mutationStatus(http.StatusConflict, err), err
		}
	}
	backupPath, _, err := ensureBackup(s.backupsDir(), "mcp", target.ID, target.ConfigPath)
	if err != nil {
		return nil, mutationStatus(http.StatusConflict, err), fmt.Errorf("备份原配置失败: %v", err)
	}
	mode := existingFileModeOrDefault(target.ConfigPath, defaultModeForTarget(target.Scope, "mcp"))
	if err := writeFileAtomic(target.ConfigPath, []byte(newText), mode); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("写入 MCP 配置失败: %v", err)
	}
	if err := recordBackupInstalled(s.backupsDir(), backupTargetForMCP(target), []byte(newText), mode); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("记录配置摘要失败: %v", err)
	}
	if target.ID == "antigravity" && !s.skipRealAgentCLI {
		if syncErr := syncAgyMCPRegistry(s.resolveHomeDir(), exe, true); syncErr != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("已写入配置文件，但同步 Antigravity CLI 失败: %w", syncErr)
		}
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

func (s *Server) applySkillTarget(home string, target skillTargetStatus, action string, lease *applyLease) (map[string]any, int, error) {
	if action == "restore" {
		_, rec, hasBackup, checkErr := lookupBackupChecked(s.backupsDir(), "skill", target.ID, target.ConfigPath)
		if checkErr != nil {
			return nil, mutationStatus(http.StatusConflict, checkErr), checkErr
		}
		removedFile := false
		restoredFromBackup := false
		if hasBackup {
			plan, planErr := planManagedUninstall(s.backupsDir(), backupTargetForSkill(target), rec)
			if planErr != nil {
				return nil, mutationStatus(http.StatusConflict, planErr), planErr
			}
			if lease != nil {
				if err := plan.verify(lease, target.ConfigPath); err != nil {
					return nil, mutationStatus(http.StatusConflict, err), err
				}
			}
			if err := applyManagedUninstall(s.backupsDir(), backupTargetForSkill(target), plan); err != nil {
				return nil, mutationStatus(http.StatusConflict, err), err
			}
			restoredFromBackup = true
			removedFile = plan.Action == uninstallDeleteFile
		} else {
			oldText, _, readErr := readFileTextOrEmpty(target.ConfigPath)
			if readErr != nil {
				return nil, http.StatusInternalServerError, readErr
			}
			if strings.TrimSpace(oldText) != "" {
				if extractSkillManagedBlock(oldText) == "" {
					return nil, http.StatusConflict, ErrLegacyBackupNeedsReview
				}
				newText := removeSkillManagedBlock(oldText)
				deleteFile := strings.TrimSpace(newText) == ""
				if lease != nil {
					if deleteFile {
						if err := lease.checkDeletion(target.ConfigPath); err != nil {
							return nil, mutationStatus(http.StatusConflict, err), err
						}
					} else if err := lease.checkNewContent(target.ConfigPath, []byte(newText)); err != nil {
						return nil, mutationStatus(http.StatusConflict, err), err
					}
				}
				if deleteFile {
					if err := os.Remove(target.ConfigPath); err != nil && !os.IsNotExist(err) {
						return nil, http.StatusInternalServerError, err
					}
					removedFile = true
					cleanupEmptySkillLeafDir("skill", target.ConfigPath)
				} else {
					mode := existingFileModeOrDefault(target.ConfigPath, defaultModeForTarget(target.Scope, "skill"))
					if err := writeFileAtomic(target.ConfigPath, []byte(newText), mode); err != nil {
						return nil, http.StatusInternalServerError, err
					}
				}
			}
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
	rendered := renderSkillContent(home, target.ID)
	newText, err := mergeSkillInstallContent(oldText, rendered)
	if err != nil {
		return nil, mutationStatus(http.StatusConflict, err), err
	}
	if lease != nil {
		if err := lease.checkNewContent(target.ConfigPath, []byte(newText)); err != nil {
			return nil, mutationStatus(http.StatusConflict, err), err
		}
	}
	backupPath, _, err := ensureBackup(s.backupsDir(), "skill", target.ID, target.ConfigPath)
	if err != nil {
		return nil, mutationStatus(http.StatusConflict, err), fmt.Errorf("备份原 Skill 失败: %v", err)
	}
	mode := existingFileModeOrDefault(target.ConfigPath, defaultModeForTarget(target.Scope, "skill"))
	if err := writeFileAtomic(target.ConfigPath, []byte(newText), mode); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("写入 Skill 失败: %v", err)
	}
	if err := recordBackupInstalled(s.backupsDir(), backupTargetForSkill(target), []byte(newText), mode); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("记录 Skill 摘要失败: %v", err)
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
		"base_sha256": d.BaseSHA256,
		"base_hashes": map[string]string{d.ConfigPath: d.BaseSHA256},
		"new_sha256":  d.NewSHA256,
		"preview_id":  d.PreviewID,
		"expires_at":  d.ExpiresAt,
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
	if !decodeJSONBody(w, r, &req, false) {
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
	d, err = s.attachPreview(d, "mcp", target.ID, project, target.Scope, action)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeTargetDiffJSON(w, d)
}

func (s *Server) handleInstallMCPClient(w http.ResponseWriter, r *http.Request) {
	s.handleMCPMutation(w, r, "install")
}

func (s *Server) handleRestoreMCPClient(w http.ResponseWriter, r *http.Request) {
	s.handleMCPMutation(w, r, "restore")
}

func (s *Server) handleMCPMutation(w http.ResponseWriter, r *http.Request, action string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req targetMutationRequest
	if !decodeJSONBody(w, r, &req, false) {
		return
	}
	project := strings.TrimSpace(req.Project)
	target := findMCPClientTarget(s.resolveHomeDir(), project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Agent 客户端标识")
		return
	}
	if !req.Confirmed || strings.TrimSpace(req.PreviewID) == "" {
		writeError(w, http.StatusConflict, ErrPreviewRequired.Error())
		return
	}
	lease, err := s.beginApply(req.PreviewID, previewExpectation{
		Kind:           "mcp",
		TargetID:       target.ID,
		Project:        project,
		Scope:          target.Scope,
		Action:         action,
		Paths:          []string{target.ConfigPath},
		ExpectedHashes: req.ExpectedHashes,
	})
	if err != nil {
		writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
		return
	}
	defer lease.Close()
	res, status, err := s.applyMCPTarget(*target, project, action, lease)
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
	if !decodeJSONBody(w, r, &req, false) {
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
	d, err = s.attachPreview(d, "skill", target.ID, project, target.Scope, action)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeTargetDiffJSON(w, d)
}

func (s *Server) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	s.handleSkillMutation(w, r, "install")
}

func (s *Server) handleRestoreSkill(w http.ResponseWriter, r *http.Request) {
	s.handleSkillMutation(w, r, "restore")
}

func (s *Server) handleSkillMutation(w http.ResponseWriter, r *http.Request, action string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req targetMutationRequest
	if !decodeJSONBody(w, r, &req, false) {
		return
	}
	home := s.resolveHomeDir()
	project := strings.TrimSpace(req.Project)
	target := findSkillTarget(home, project, req.effectiveID())
	if target == nil {
		writeError(w, http.StatusBadRequest, "未知的 Skill 目标标识")
		return
	}
	if !req.Confirmed || strings.TrimSpace(req.PreviewID) == "" {
		writeError(w, http.StatusConflict, ErrPreviewRequired.Error())
		return
	}
	lease, err := s.beginApply(req.PreviewID, previewExpectation{
		Kind:           "skill",
		TargetID:       target.ID,
		Project:        project,
		Scope:          target.Scope,
		Action:         action,
		Paths:          []string{target.ConfigPath},
		ExpectedHashes: req.ExpectedHashes,
	})
	if err != nil {
		writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
		return
	}
	defer lease.Close()
	res, status, err := s.applySkillTarget(home, *target, action, lease)
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

func renderSkillContentWithDoc(targetID, doc string) string {
	doc = strings.TrimSpace(doc)
	if doc == "" {
		doc = "# SlopGuard\n\nUse sg_verify before finishing edits."
	}
	if targetID == "project_cursor_rule" {
		if strings.HasPrefix(doc, "---\n") {
			return wrapSkillManagedBlock(doc + "\n")
		}
		return wrapSkillManagedBlock("---\ndescription: SlopGuard deterministic code-quality guardrail MCP workflow\nalwaysApply: false\n---\n\n" + doc + "\n")
	}
	if strings.HasPrefix(doc, "---\n") {
		return wrapSkillManagedBlock(doc + "\n")
	}
	return wrapSkillManagedBlock("---\nname: slopguard\ndescription: SlopGuard deterministic code-quality guardrail MCP workflow and mistake inbox analysis guide.\n---\n\n" + doc + "\n")
}

type sharedSkillPreviewRequest struct {
	Content     string `json:"content"`
	Reset       bool   `json:"reset"`
	SyncEnabled *bool  `json:"sync_enabled"`
	Project     string `json:"project"`
}

func sharedSkillPreviewPlan(home string, req sharedSkillPreviewRequest) (targetDiffSummary, map[string]string, map[string]string, []string, string, error) {
	path := sharedSkillPath(home)
	if path == "" {
		return targetDiffSummary{}, nil, nil, nil, "", errors.New("无法定位用户主目录")
	}
	oldText, existed, err := readFileTextOrEmpty(path)
	if err != nil {
		return targetDiffSummary{}, nil, nil, nil, "", err
	}
	newText := ""
	if req.Reset {
		newText = ""
	} else {
		trimmed := strings.TrimSpace(req.Content)
		if trimmed == "" {
			return targetDiffSummary{}, nil, nil, nil, "", errors.New("护栏指令内容不能为空")
		}
		newText = trimmed + "\n"
	}
	docForSkills := newText
	if req.Reset {
		docForSkills = defaultSharedSkillContent()
	}

	baseHashes := map[string]string{path: hashIfText(oldText, existed)}
	newHashes := map[string]string{path: hashIfNewText(newText)}
	paths := []string{path}
	diffs := []string{buildUnifiedDiff(path, oldText, newText, 3)}
	syncEnabled := true
	if req.SyncEnabled != nil {
		syncEnabled = *req.SyncEnabled
	}
	if syncEnabled {
		for _, sk := range discoverSkillTargets(home, strings.TrimSpace(req.Project)) {
			if !sk.Configured {
				continue
			}
			oldSkill, skillExisted, readErr := readFileTextOrEmpty(sk.ConfigPath)
			if readErr != nil {
				return targetDiffSummary{}, nil, nil, nil, "", readErr
			}
			renderedSkill := renderSkillContentWithDoc(sk.ID, docForSkills)
			newSkill, mergeErr := mergeSkillInstallContent(oldSkill, renderedSkill)
			if mergeErr != nil {
				return targetDiffSummary{}, nil, nil, nil, "", mergeErr
			}
			baseHashes[sk.ConfigPath] = hashIfText(oldSkill, skillExisted)
			newHashes[sk.ConfigPath] = fileSHA256Hex([]byte(newSkill))
			paths = append(paths, sk.ConfigPath)
			diffs = append(diffs, buildUnifiedDiff(sk.ConfigPath, oldSkill, newSkill, 3))
		}
	}
	action := "install"
	if req.Reset {
		action = "restore"
	}
	diff := strings.Join(diffs, "\n")
	return targetDiffSummary{
		Kind:       "shared_skill",
		ID:         "shared-skill",
		Name:       "共享护栏指令",
		Scope:      "global",
		ConfigPath: strings.Join(paths, " + "),
		Action:     action,
		Existed:    existed,
		BaseSHA256: baseHashes[path],
		NewSHA256:  newHashes[path],
		HasChanges: diff != "",
		Diff:       diff,
	}, baseHashes, newHashes, paths, newText, nil
}

func hashIfText(text string, existed bool) string {
	if !existed {
		return ""
	}
	return fileSHA256Hex([]byte(text))
}

func hashIfNewText(text string) string {
	if text == "" {
		return ""
	}
	return fileSHA256Hex([]byte(text))
}

func (s *Server) handlePreviewSharedSkill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req sharedSkillPreviewRequest
	if !decodeJSONBody(w, r, &req, false) {
		return
	}
	home := s.resolveHomeDir()
	project := strings.TrimSpace(req.Project)
	d, baseHashes, newHashes, paths, newText, err := sharedSkillPreviewPlan(home, req)
	if err != nil {
		writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
		return
	}
	action := "install"
	if req.Reset {
		action = "restore"
	}
	rec, err := s.createPreview(previewRecord{
		Kind:      "shared_skill",
		TargetID:  "shared-skill",
		Project:   project,
		Scope:     "global",
		Action:    action,
		Paths:     baseHashes,
		NewHashes: newHashes,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	syncEnabled := true
	if req.SyncEnabled != nil {
		syncEnabled = *req.SyncEnabled
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"kind":         d.Kind,
		"target_id":    d.ID,
		"name":         d.Name,
		"scope":        d.Scope,
		"action":       d.Action,
		"config_path":  d.ConfigPath,
		"existed":      d.Existed,
		"base_sha256":  d.BaseSHA256,
		"new_sha256":   d.NewSHA256,
		"base_hashes":  baseHashes,
		"new_hashes":   newHashes,
		"preview_id":   rec.ID,
		"expires_at":   rec.ExpiresAt.UTC().Format(time.RFC3339),
		"has_changes":  d.HasChanges,
		"has_backup":   false,
		"diff":         d.Diff,
		"content":      newText,
		"reset":        req.Reset,
		"sync_enabled": syncEnabled,
		"project":      project,
		"paths":        paths,
	})
}

type guardHookPreviewRequest struct {
	Project     string `json:"project"`
	HookEnabled bool   `json:"hook_enabled"`
}

func (s *Server) guardHookPreviewPlan(project string, hookEnabled bool) (targetDiffSummary, map[string]string, map[string]string, string, string, bool, error) {
	absProject, err := filepath.Abs(strings.TrimSpace(project))
	if err != nil {
		return targetDiffSummary{}, nil, nil, "", "", false, err
	}
	hookPath, isGit := resolveGitHookPath(absProject)
	if !isGit || hookPath == "" {
		return targetDiffSummary{}, nil, nil, "", "", false, errors.New("当前目录不是 Git 仓库，无法修改 post-commit 钩子")
	}
	oldText, existed, err := readFileTextOrEmpty(hookPath)
	if err != nil {
		return targetDiffSummary{}, nil, nil, "", "", false, err
	}
	newText := oldText
	deleteFile := false
	if hookEnabled {
		exe, exeErr := resolveSelfBinary()
		if exeErr != nil {
			exe = "slopguard"
		}
		newText = upsertHookMarkerBlock(oldText, buildGitHookBlock(exe))
	} else {
		backupsDir := s.backupsDir()
		_, rec, ok, checkErr := lookupBackupChecked(backupsDir, "hook", "post-commit", hookPath)
		if checkErr != nil {
			return targetDiffSummary{}, nil, nil, "", "", false, checkErr
		}
		if ok {
			plan, planErr := planManagedUninstall(backupsDir, backupTarget{Kind: "hook", ID: "post-commit", Scope: "project", Path: hookPath}, rec)
			if planErr != nil {
				return targetDiffSummary{}, nil, nil, "", "", false, planErr
			}
			switch plan.Action {
			case uninstallDeleteFile:
				newText = ""
				deleteFile = true
			case uninstallNoop:
				newText = oldText
			default:
				newText = string(plan.NewData)
			}
		} else {
			stripped := stripHookMarkerBlock(oldText)
			trimmed := strings.TrimSpace(stripped)
			hasManagedBlock := extractHookManagedBlock(oldText) != ""
			switch {
			case trimmed == "":
				newText = ""
				deleteFile = existed && hasManagedBlock
			case trimmed == "#!/bin/sh" || trimmed == "#!/usr/bin/env sh":
				if hasManagedBlock {
					newText = ""
					deleteFile = true
				} else {
					newText = oldText
					deleteFile = false
				}
			default:
				newText = stripped
			}
		}
	}
	baseHashes := map[string]string{hookPath: hashIfText(oldText, existed)}
	newHashes := map[string]string{hookPath: ""}
	if !deleteFile {
		newHashes[hookPath] = fileSHA256Hex([]byte(newText))
	}
	diff := buildUnifiedDiff(hookPath, oldText, newText, 3)
	action := "install"
	if !hookEnabled {
		action = "restore"
	}
	return targetDiffSummary{
		Kind:       "guard_hook",
		ID:         "post-commit",
		Name:       "Git post-commit hook",
		Scope:      "project",
		ConfigPath: hookPath,
		Action:     action,
		Existed:    existed,
		BaseSHA256: baseHashes[hookPath],
		NewSHA256:  newHashes[hookPath],
		HasChanges: diff != "",
		Diff:       diff,
	}, baseHashes, newHashes, hookPath, newText, deleteFile, nil
}

func (s *Server) handlePreviewGuardHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req guardHookPreviewRequest
	if !decodeJSONBody(w, r, &req, false) {
		return
	}
	d, baseHashes, newHashes, _, _, _, err := s.guardHookPreviewPlan(req.Project, req.HookEnabled)
	if err != nil {
		writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
		return
	}
	project := normalizePreviewProject(req.Project)
	action := "install"
	if !req.HookEnabled {
		action = "restore"
	}
	rec, err := s.createPreview(previewRecord{
		Kind:      "guard_hook",
		TargetID:  "post-commit",
		Project:   project,
		Scope:     "project",
		Action:    action,
		Paths:     baseHashes,
		NewHashes: newHashes,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"kind":        d.Kind,
		"target_id":   d.ID,
		"name":        d.Name,
		"scope":       d.Scope,
		"action":      d.Action,
		"config_path": d.ConfigPath,
		"existed":     d.Existed,
		"base_sha256": d.BaseSHA256,
		"new_sha256":  d.NewSHA256,
		"base_hashes": baseHashes,
		"new_hashes":  newHashes,
		"preview_id":  rec.ID,
		"expires_at":  rec.ExpiresAt.UTC().Format(time.RFC3339),
		"has_changes": d.HasChanges,
		"has_backup":  false,
		"diff":        d.Diff,
		"project":     project,
	})
}

func (s *Server) handleGuardToggleSecure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req struct {
		Project     string `json:"project"`
		Enabled     *bool  `json:"enabled"`
		HookEnabled *bool  `json:"hook_enabled"`
		DebounceSec int    `json:"debounce_sec"`
		PreviewID   string `json:"preview_id"`
		Confirmed   bool   `json:"confirmed"`
	}
	if !decodeJSONBody(w, r, &req, false) {
		return
	}
	if req.HookEnabled != nil {
		if !req.Confirmed || strings.TrimSpace(req.PreviewID) == "" {
			writeError(w, http.StatusConflict, ErrPreviewRequired.Error())
			return
		}
		absProject, err := filepath.Abs(strings.TrimSpace(req.Project))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		hookPath, isGit := resolveGitHookPath(absProject)
		if !isGit || hookPath == "" {
			writeError(w, http.StatusBadRequest, "当前目录不是 Git 仓库，无法修改 post-commit 钩子")
			return
		}
		d, _, _, _, newText, deleteFile, planErr := s.guardHookPreviewPlan(absProject, *req.HookEnabled)
		if planErr != nil {
			writeError(w, mutationStatus(http.StatusConflict, planErr), planErr.Error())
			return
		}
		action := "install"
		if !*req.HookEnabled {
			action = "restore"
		}
		lease, err := s.beginApply(req.PreviewID, previewExpectation{
			Kind:     "guard_hook",
			TargetID: "post-commit",
			Project:  absProject,
			Scope:    "project",
			Action:   action,
			Paths:    []string{hookPath},
		})
		if err != nil {
			writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
			return
		}
		defer lease.Close()
		if deleteFile {
			if err := lease.checkDeletion(hookPath); err != nil {
				writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
				return
			}
		} else if err := lease.checkNewContent(hookPath, []byte(newText)); err != nil {
			writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
			return
		}
		verifiedHookContent := newText
		st, err := s.guard.SetProjectConfigVerified(absProject, req.Enabled, req.HookEnabled, req.DebounceSec, &verifiedHookContent)
		if err != nil {
			writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": st, "diff": d.Diff})
		return
	}
	st, err := s.guard.SetProjectConfig(req.Project, req.Enabled, nil, req.DebounceSec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": st})
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
			PreviewID   string `json:"preview_id"`
			Confirmed   bool   `json:"confirmed"`
		}
		if !decodeJSONBody(w, r, &req, false) {
			return
		}
		if path == "" {
			writeError(w, http.StatusInternalServerError, "无法定位用户主目录")
			return
		}
		if !req.Confirmed || strings.TrimSpace(req.PreviewID) == "" {
			writeError(w, http.StatusConflict, ErrPreviewRequired.Error())
			return
		}
		previewReq := sharedSkillPreviewRequest{
			Content:     req.Content,
			Reset:       req.Reset,
			SyncEnabled: req.SyncEnabled,
			Project:     strings.TrimSpace(req.Project),
		}
		_, _, previewNewHashes, previewPaths, newText, planErr := sharedSkillPreviewPlan(home, previewReq)
		if planErr != nil {
			writeError(w, mutationStatus(http.StatusConflict, planErr), planErr.Error())
			return
		}
		action := "install"
		if req.Reset {
			action = "restore"
		}
		lease, err := s.beginApply(req.PreviewID, previewExpectation{
			Kind:     "shared_skill",
			TargetID: "shared-skill",
			Project:  strings.TrimSpace(req.Project),
			Scope:    "global",
			Action:   action,
			Paths:    previewPaths,
		})
		if err != nil {
			writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
			return
		}
		defer lease.Close()
		for targetPath, hash := range previewNewHashes {
			var checkErr error
			if strings.TrimSpace(hash) == "" {
				checkErr = lease.checkDeletion(targetPath)
			} else {
				checkErr = lease.checkNewHash(targetPath, hash)
			}
			if checkErr != nil {
				writeError(w, mutationStatus(http.StatusConflict, checkErr), checkErr.Error())
				return
			}
		}
		if req.Reset {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("恢复默认指令失败: %v", err))
				return
			}
		} else {
			if err := lease.checkNewContent(path, []byte(newText)); err != nil {
				writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
				return
			}
			if err := writeFileAtomic(path, []byte(newText), 0o644); err != nil {
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
				if _, planned := previewNewHashes[sk.ConfigPath]; !planned {
					continue
				}
				if _, _, err := s.applySkillTarget(home, sk, "install", lease); err == nil {
					synced = append(synced, sk.ConfigPath)
				} else {
					writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
					return
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
	profiles := discoverAgentProfiles(home, project)
	var lastEventAt string
	if s.db != nil {
		if events, err := s.db.ListMCPEvents(r.Context(), project, "", 1); err == nil && len(events) > 0 {
			lastEventAt = events[0].CreatedAt.UTC().Format(time.RFC3339)
		}
	}
	agyCLIRegistered := false
	if !s.skipRealAgentCLI {
		agyCLIRegistered = isSlopGuardInAgyMCPList(home)
	}
	for i := range profiles {
		p := &profiles[i]
		if p.HealthStatus != "pending_activation" {
			continue
		}
		if p.ID == "antigravity" && agyCLIRegistered {
			p.HealthStatus = "active"
			p.HealthDetail = "MCP 配置完好 · Antigravity CLI 已注册（可点自检实时握手验证）"
		} else if lastEventAt != "" {
			p.LastEventAt = lastEventAt
			p.HealthStatus = "active"
			p.HealthDetail = "配置完好 · 已检测到真实 MCP 调用记录"
		}
	}

	content, isCustom, updatedAt := loadSharedSkill(home)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"binary_path": exe,
		"agents":      profiles,
		"shared_skill": map[string]any{
			"path":            sharedSkillPath(home),
			"content":         content,
			"default_content": defaultSharedSkillContent(),
			"is_custom":       isCustom,
			"updated_at":      updatedAt,
		},
	})
}

func (s *Server) handleVerifyAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req struct {
		AgentID string `json:"agent_id"`
		Project string `json:"project"`
	}
	if !decodeJSONBody(w, r, &req, true) {
		return
	}
	home := s.resolveHomeDir()
	project := strings.TrimSpace(req.Project)
	agentID := strings.TrimSpace(req.AgentID)
	checkRealCLI := !s.skipRealAgentCLI

	profiles := discoverAgentProfiles(home, project)
	reports := make([]AgentVerifyReport, 0, len(profiles))
	for _, p := range profiles {
		if agentID != "" && agentID != "all" && p.ID != agentID {
			continue
		}
		if agentID == "" || agentID == "all" {
			if !p.Enabled && !p.Partial && !p.Detected {
				continue
			}
		}
		reports = append(reports, verifyAgentProfile(r.Context(), home, project, p, s.db, checkRealCLI))
	}
	if len(reports) == 0 && agentID != "" && agentID != "all" {
		writeError(w, http.StatusBadRequest, "未知的 Agent 标识")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"reports": reports,
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
	if !decodeJSONBody(w, r, &req, false) {
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
	baseHashes := make(map[string]string, 2)
	newHashes := make(map[string]string, 2)
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
		baseHashes[d.ConfigPath] = d.BaseSHA256
		newHashes[d.ConfigPath] = d.NewSHA256
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
		baseHashes[d.ConfigPath] = d.BaseSHA256
		newHashes[d.ConfigPath] = d.NewSHA256
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

	rec, err := s.createPreview(previewRecord{
		Kind:      "agent",
		TargetID:  prof.ID,
		Project:   project,
		Scope:     prof.Scope,
		Action:    action,
		Paths:     baseHashes,
		NewHashes: newHashes,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
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
		"base_hashes": baseHashes,
		"new_hashes":  newHashes,
		"preview_id":  rec.ID,
		"expires_at":  rec.ExpiresAt.UTC().Format(time.RFC3339),
		"has_changes": anyChanges,
		"has_backup":  anyBackup,
		"backup_at":   backupAt,
		"targets":     diffs,
		"diff":        strings.Join(combined, "\n"),
	})
}

func (s *Server) preflightAgentTargets(home string, prof *agentProfile, project, action string, lease *applyLease) error {
	if prof == nil {
		return nil
	}
	check := func(d targetDiffSummary, err error) error {
		if err != nil {
			return err
		}
		if strings.TrimSpace(d.NewSHA256) == "" {
			return lease.checkDeletion(d.ConfigPath)
		}
		return lease.checkNewHash(d.ConfigPath, d.NewSHA256)
	}
	if prof.MCPTarget != nil {
		d, _, err := s.previewMCPTarget(*prof.MCPTarget, project, action)
		if err := check(d, err); err != nil {
			return err
		}
	}
	if prof.SkillTarget != nil {
		d, _, err := s.previewSkillTarget(home, *prof.SkillTarget, action)
		if err := check(d, err); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleToggleAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req struct {
		AgentID        string            `json:"agent_id"`
		Project        string            `json:"project"`
		Enable         *bool             `json:"enable"`
		PreviewID      string            `json:"preview_id"`
		Confirmed      bool              `json:"confirmed"`
		ExpectedHashes map[string]string `json:"expected_hashes,omitempty"`
	}
	if !decodeJSONBody(w, r, &req, false) {
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
	if !req.Confirmed || strings.TrimSpace(req.PreviewID) == "" {
		writeError(w, http.StatusConflict, ErrPreviewRequired.Error())
		return
	}
	var expectedPaths []string
	if prof.MCPTarget != nil {
		expectedPaths = append(expectedPaths, prof.MCPTarget.ConfigPath)
	}
	if prof.SkillTarget != nil {
		expectedPaths = append(expectedPaths, prof.SkillTarget.ConfigPath)
	}

	action := "restore"
	if enable {
		action = "install"
	}
	lease, err := s.beginApply(req.PreviewID, previewExpectation{
		Kind:           "agent",
		TargetID:       prof.ID,
		Project:        project,
		Scope:          prof.Scope,
		Action:         action,
		Paths:          expectedPaths,
		ExpectedHashes: req.ExpectedHashes,
	})
	if err != nil {
		writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
		return
	}
	defer lease.Close()
	if err := s.preflightAgentTargets(home, prof, project, action, lease); err != nil {
		writeError(w, mutationStatus(http.StatusConflict, err), err.Error())
		return
	}

	results := make([]map[string]any, 0, 2)
	wroteMCP := false
	if prof.MCPTarget != nil {
		res, status, err := s.applyMCPTarget(*prof.MCPTarget, project, action, lease)
		if err != nil {
			writeError(w, status, err.Error())
			return
		}
		wroteMCP = true
		results = append(results, res)
	}
	if prof.SkillTarget != nil {
		res, status, err := s.applySkillTarget(home, *prof.SkillTarget, action, lease)
		if err != nil {
			if wroteMCP && action == "install" && prof.MCPTarget != nil {
				_, _, _ = s.applyMCPTarget(*prof.MCPTarget, project, "restore", nil)
			}
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

func (s *Server) handleApplyAIRecommendations(w http.ResponseWriter, r *http.Request) {
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
	if !decodeJSONBody(w, r, &req, true) {
		return
	}
	project := strings.TrimSpace(req.Project)

	appliedRules, ignoredCount, err := s.db.ApplyAIRecommendationsBatch(r.Context(), project, ruleFromApprovedMistake)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("批量采纳事务失败（已整体回滚）: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"applied_rules": appliedRules,
		"ignored":       ignoredCount,
		"total":         appliedRules + ignoredCount,
	})
}
