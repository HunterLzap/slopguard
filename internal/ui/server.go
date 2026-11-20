package ui

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
	"github.com/HunterLzap/slopguard/internal/radar"
	"github.com/HunterLzap/slopguard/internal/store"
	"gopkg.in/yaml.v3"
)

//go:embed assets
var assets embed.FS

// Options controls the local dashboard server.
type Options struct {
	// Port is the local TCP port; 0 means the default 7788.
	Port int
	// NoOpen disables opening the system browser.
	NoOpen bool
}

// Server exposes the dashboard API, local action handlers, background guard, and embedded single-page app.
type Server struct {
	db               *store.Store
	runner           contract.Runner
	mux              *http.ServeMux
	port             int
	homeDir          string
	skipRealAgentCLI bool
	instanceID       string
	startedAt        time.Time
	guard            *GuardManager
	authMu           sync.RWMutex
	authToken        string
	authTickets      map[string]authTicket
	authSessions     map[string]authSession
	mutationMu       sync.Mutex
	previewMu        sync.Mutex
	previews         map[string]*previewRecord
}

// NewServer creates a dashboard server backed by the given store and runner.
func NewServer(db *store.Store, runner contract.Runner) *Server {
	s := &Server{
		db:               db,
		runner:           runner,
		skipRealAgentCLI: true,
		authToken:        generateRandomToken(),
		authTickets:      make(map[string]authTicket),
		authSessions:     make(map[string]authSession),
		previews:         make(map[string]*previewRecord),
	}
	s.guard = newGuardManager(s)
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(mustSubFS(assets, "assets")))))
	mux.HandleFunc("/api/identity", s.handleIdentity)
	mux.HandleFunc("/api/auth/ticket", s.handleAuthTicket)
	mux.HandleFunc("/api/overview", s.handleOverview)
	mux.HandleFunc("/api/summary", s.handleSummary)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/mistakes", s.handleMistakes)
	mux.HandleFunc("/api/mistakes/", s.handleMistakeAction)
	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/radar", s.handleRadar)
	mux.HandleFunc("/api/project-meta", s.handleProjectMeta)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/screenshot", s.handleScreenshot)
	mux.HandleFunc("/api/service-status", s.handleServiceStatus)
	s.registerActionRoutes(mux)
	s.mux = mux
	return s
}

func (s *Server) resolveHomeDir() string {
	if s != nil && strings.TrimSpace(s.homeDir) != "" {
		return s.homeDir
	}
	home, _ := os.UserHomeDir()
	return home
}

func (s *Server) backupsDir() string {
	home := s.resolveHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".slopguard", "backups")
}

// Handler returns the dashboard HTTP handler wrapped with localhost security middleware.
func (s *Server) Handler() http.Handler {
	return s.withSecurity(s.mux)
}

// Run starts the local dashboard on 127.0.0.1 and blocks until ctx is cancelled
// or the server stops. The browser is opened unless Options.NoOpen is set.
func Run(ctx context.Context, db *store.Store, runner contract.Runner, opts Options, stdout io.Writer) error {
	if db == nil {
		return errors.New("ui: nil store")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Port == 0 {
		opts.Port = 7788
	}
	if opts.Port < 1 || opts.Port > 65535 {
		return fmt.Errorf("ui: port out of range: %d", opts.Port)
	}
	server := NewServer(db, runner)
	home := server.resolveHomeDir()
	if strings.TrimSpace(home) == "" {
		return errors.New("ui: cannot locate user home directory")
	}
	server.homeDir = home
	// Real agent CLI registration and probes are enabled only for the actual
	// dashboard process, never by direct NewServer construction in tests.
	server.skipRealAgentCLI = false

	// Take ownership before touching the persistent token. A failed start must
	// never rotate the token of an already-running dashboard.
	lock, err := acquireInstanceLock(home, opts.Port)
	if err != nil {
		return err
	}
	defer lock.release()

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opts.Port))
	if err != nil {
		return fmt.Errorf("ui: listen 127.0.0.1:%d: %w", opts.Port, err)
	}
	defer listener.Close()

	actualPort := opts.Port
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		actualPort = tcpAddr.Port
	}
	server.port = actualPort
	server.instanceID = lock.record.InstanceID
	server.startedAt = time.Now().UTC()
	if err := lock.setPort(actualPort); err != nil {
		return err
	}

	token, err := loadOrCreateAuthToken(home)
	if err != nil {
		return fmt.Errorf("ui: initialize session token: %w", err)
	}
	server.SetAuthToken(token)

	address := listener.Addr().String()
	url := "http://" + address + "/"
	if stdout != nil {
		fmt.Fprintf(stdout, "SlopGuard dashboard: %s\n", url)
	}
	if !opts.NoOpen {
		ticket, _, ticketErr := server.IssueAuthTicket(authTicketTTL)
		if ticketErr != nil {
			return fmt.Errorf("ui: issue browser ticket: %w", ticketErr)
		}
		authURL := url + "?ticket=" + neturl.QueryEscape(ticket)
		if err := openBrowser(authURL); err != nil && stdout != nil {
			fmt.Fprintf(stdout, "slopguard: warning: cannot open browser: %v\n", err)
		}
	}

	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      180 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	if server.guard != nil {
		server.guard.Start(ctx)
		defer server.guard.Stop()
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("ui: shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func mustSubFS(source fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(source, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "dashboard asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	overview, err := s.db.OverviewStats(r.Context(), r.URL.Query().Get("project"), r.URL.Query().Get("granularity"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	summary := map[string]any{
		"pending_user":      0,
		"pending_ai":        0,
		"total_runs":        int64(0),
		"has_scans":         false,
		"last_scan_passed":  true,
		"last_scan_project": "",
		"last_scan_at":      "",
		"guarded_projects":  0,
	}
	if s.guard != nil {
		summary["guarded_projects"] = s.guard.CountEnabled()
	}
	if s.db == nil {
		writeJSON(w, http.StatusOK, summary)
		return
	}

	overview, err := s.db.OverviewStats(r.Context(), "", "day")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mistakes, err := s.db.QueryMistakes(r.Context(), store.MistakeQuery{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var pendingUser, pendingAI int
	for _, m := range mistakes {
		if isFilesystemRootPath(m.ProjectPath) {
			continue
		}
		switch m.ReviewStatus {
		case "pending_user":
			pendingUser++
		case "pending_ai":
			pendingAI++
		}
	}
	summary["pending_user"] = pendingUser
	summary["pending_ai"] = pendingAI
	summary["total_runs"] = overview.TotalRuns
	if len(overview.RecentRuns) > 0 {
		latest := overview.RecentRuns[0]
		summary["has_scans"] = true
		summary["last_scan_passed"] = latest.Passed
		summary["last_scan_project"] = filepath.Base(latest.ProjectPath)
		summary["last_scan_at"] = latest.CreatedAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, summary)
}

type projectInfoResponse struct {
	store.ProjectInfo
	GuardEnabled bool `json:"guard_enabled"`
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	projects, err := s.db.ListProjects(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	guarded := map[string]bool{}
	if s.guard != nil {
		guarded = s.guard.EnabledMap()
	}
	hidden := loadHiddenProjects(s.resolveHomeDir())
	out := make([]projectInfoResponse, 0, len(projects))
	for _, p := range projects {
		if isFilesystemRootPath(p.ProjectPath) || isProjectHidden(hidden, p.ProjectPath) {
			continue
		}
		out = append(out, projectInfoResponse{
			ProjectInfo:  p,
			GuardEnabled: guarded[filepath.Clean(p.ProjectPath)],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func isFilesystemRootPath(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return true
	}
	cleaned := filepath.Clean(trimmed)
	return filepath.Dir(cleaned) == cleaned
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	q := r.URL.Query()
	limit := 0
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = parsed
	}
	reqProj := strings.TrimSpace(q.Get("project"))
	events, err := s.db.ListMCPEvents(r.Context(), reqProj, q.Get("tool"), limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filtered := make([]store.MCPEvent, 0, len(events))
	for _, ev := range events {
		if reqProj == "" && isFilesystemRootPath(ev.ProjectPath) {
			continue
		}
		filtered = append(filtered, ev)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": filtered})
}

type mistakeResponse struct {
	store.MistakeRecord
	LocalHint string `json:"local_hint,omitempty"`
}

func buildLocalMistakeHint(m store.MistakeRecord) string {
	hay := strings.ToLower(m.Summary + "\n" + m.BadSnippet)
	switch {
	case strings.Contains(hay, "ratchet:"):
		return "新增了跳过检查的注释且超过当前基线上限，建议直接修复问题而不是继续绕过检查。"
	case strings.Contains(hay, "reportoptionalmemberaccess") || strings.Contains(hay, `known attribute of "none"`) || strings.Contains(hay, "nonetype"):
		return "对象可能为空（None），直接访问属性或方法会在运行时崩溃，建议先判空再访问。"
	case strings.Contains(hay, "reportoptionalcall") || strings.Contains(hay, "reportoptionalsubscript"):
		return "对象可能为空（None），直接调用或下标取值存在空指针崩溃风险。"
	case strings.Contains(hay, "f401") || strings.Contains(hay, "imported but unused") || strings.Contains(hay, "no-unused-vars"):
		return "存在未使用的导入或变量，清理后可减少代码噪音与循环依赖风险。"
	case strings.Contains(hay, "f841") || strings.Contains(hay, "assigned to but never used"):
		return "局部变量赋值后从未使用，请确认是否遗漏业务逻辑或清理多余变量。"
	case strings.Contains(hay, "invalid `# noqa`") || strings.Contains(hay, "invalid `# type: ignore"):
		return "忽略注释写法不规范，建议补充具体规则编号或直接修复源码问题。"
	case strings.Contains(hay, "no-explicit-any") || strings.Contains(hay, "reportunknown"):
		return "存在未收敛的 any 或未知类型，容易绕过静态类型保护，建议补全明确类型。"
	case strings.Contains(hay, "no-console"):
		return "代码保留了调试用 console 输出，建议清理或改用规范日志。"
	case strings.Contains(hay, "ts2322") || strings.Contains(hay, "ts2345") || strings.Contains(hay, "is not assignable to type"):
		return "变量或参数类型与声明不匹配，建议核对类型定义与实际传入结构。"
	case strings.Contains(hay, "importerror") || strings.Contains(hay, "modulenotfounderror") || strings.Contains(hay, "cannot find module"):
		return "模块或依赖路径无法解析，请检查依赖安装状态与导入路径。"
	case strings.EqualFold(m.Source, "git_fix"):
		return "来自近期 fix 提交的返工问题，建议评估是否提炼为自动防线规则避免再犯。"
	}
	switch strings.ToLower(strings.TrimSpace(m.Category)) {
	case "lint":
		return "代码风格或静态质量检查未通过，建议确认是否为高频坏习惯。"
	case "type":
		return "静态类型检查发现潜在类型安全隐患，修复可减少运行时异常。"
	case "escape":
		return "存在跳过静态检查的注释，建议收紧规则避免检查形同虚设。"
	case "ast":
		return "抽象语法树规则命中了不推荐的代码结构模式。"
	default:
		return "建议检查该问题是否具有普遍性，若多次出现可沉淀为自动规则。"
	}
}

func extractRuleTokenAfterColon(summary, prefix string) string {
	lower := strings.ToLower(summary)
	idx := strings.Index(lower, prefix)
	if idx < 0 {
		return ""
	}
	rest := summary[idx+len(prefix):]
	end := strings.IndexAny(rest, "] \t\r\n:")
	if end > 0 {
		return strings.TrimSpace(rest[:end])
	}
	return ""
}

func deriveRuleFromMistake(m store.MistakeRecord) (engineName, ruleID, payload string) {
	summary := strings.TrimSpace(m.Summary)
	lower := strings.ToLower(summary)
	shortFP := m.Fingerprint
	if len(shortFP) > 8 {
		shortFP = shortFP[:8]
	}

	switch {
	case strings.Contains(lower, "[pyright"):
		engineName = "pyright"
		ruleID = extractRuleTokenAfterColon(summary, "[pyright:")
		if ruleID == "" {
			ruleID = "pyright-" + shortFP
		}
		payload = fmt.Sprintf("{\n  %q: \"error\"\n}", ruleID)
	case strings.Contains(lower, "[ruff"):
		engineName = "ruff"
		ruleID = extractRuleTokenAfterColon(summary, "[ruff:")
		if ruleID == "" {
			for _, code := range []string{"F401", "F841", "F821", "E711", "E712", "B006", "UP"} {
				if strings.Contains(summary, code) {
					ruleID = code
					break
				}
			}
		}
		if ruleID == "" && strings.Contains(lower, "noqa") {
			ruleID = "RUF100"
		}
		if ruleID == "" {
			ruleID = "ruff-" + shortFP
		}
		payload = fmt.Sprintf("[tool.ruff.lint]\nextend-select = [%q]", ruleID)
	case strings.Contains(lower, "[eslint") || strings.Contains(lower, "no-explicit-any") || strings.Contains(lower, "no-unused-vars") || strings.Contains(lower, "no-console"):
		engineName = "eslint"
		ruleID = extractRuleTokenAfterColon(summary, "[eslint:")
		if ruleID == "" {
			for _, code := range []string{"@typescript-eslint/no-explicit-any", "no-unused-vars", "no-console"} {
				if strings.Contains(lower, code) {
					ruleID = code
					break
				}
			}
		}
		if ruleID == "" {
			ruleID = "eslint-" + shortFP
		}
		payload = fmt.Sprintf("{\n  \"rules\": {\n    %q: \"error\"\n  }\n}", ruleID)
	case strings.Contains(lower, "ts2322") || strings.Contains(lower, "ts2345") || strings.Contains(lower, "[tsc"):
		engineName = "tsc"
		ruleID = extractRuleTokenAfterColon(summary, "[tsc:")
		if ruleID == "" {
			if strings.Contains(lower, "ts2322") {
				ruleID = "TS2322"
			} else if strings.Contains(lower, "ts2345") {
				ruleID = "TS2345"
			} else {
				ruleID = "strict-types-" + shortFP
			}
		}
		payload = fmt.Sprintf("// TypeScript guard for %s", ruleID)
	case strings.Contains(lower, "ratchet:") || strings.EqualFold(m.Category, "escape"):
		engineName = "slopguard"
		ruleID = "no-new-escapes-" + shortFP
		payload = "ratchet: disallow new ignore/noqa comments"
	default:
		engineName = "slopguard"
		ruleID = "mistake-" + shortFP
		if summary != "" {
			payload = summary
		} else {
			payload = m.BadSnippet
		}
	}
	return engineName, ruleID, payload
}

func ruleFromApprovedMistake(m store.MistakeRecord) (store.Rule, error) {
	engineName, ruleID, defaultPayload := deriveRuleFromMistake(m)
	payload := strings.TrimSpace(m.AIRuleDraft)
	if payload == "" {
		payload = defaultPayload
	}
	rationale := strings.TrimSpace(m.AIReason)
	if rationale == "" {
		rationale = buildLocalMistakeHint(m)
	}
	source := "manual"
	if strings.TrimSpace(m.AIReason) != "" {
		source = "agent"
	}
	return store.Rule{
		Scope:       "project",
		ProjectPath: m.ProjectPath,
		Engine:      engineName,
		RuleID:      ruleID,
		Payload:     payload,
		Rationale:   rationale,
		Source:      source,
		Status:      "active",
		AppliedAt:   time.Now().UTC(),
	}, nil
}

func (s *Server) handleMistakes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	q := r.URL.Query()
	records, err := s.db.QueryMistakes(r.Context(), store.MistakeQuery{
		ProjectPath:  q.Get("project"),
		Status:       q.Get("status"),
		ReviewStatus: q.Get("review_status"),
		Category:     q.Get("category"),
		Source:       q.Get("source"),
		Q:            q.Get("q"),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]mistakeResponse, 0, len(records))
	for _, rec := range records {
		out = append(out, mistakeResponse{
			MistakeRecord: rec,
			LocalHint:     buildLocalMistakeHint(rec),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"mistakes": out})
}

func (s *Server) handleMistakeAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/mistakes/")
	var fingerprint, action, targetStatus string
	switch {
	case strings.HasSuffix(path, "/approve"):
		fingerprint = strings.TrimSuffix(path, "/approve")
		action = "approve"
	case strings.HasSuffix(path, "/ignore"):
		fingerprint = strings.TrimSuffix(path, "/ignore")
		action = "ignore"
		targetStatus = "ignored"
	case strings.HasSuffix(path, "/restore"):
		fingerprint = strings.TrimSuffix(path, "/restore")
		action = "restore"
		targetStatus = "pending"
	default:
		http.NotFound(w, r)
		return
	}
	if fingerprint == "" {
		writeError(w, http.StatusBadRequest, "fingerprint is required")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	if action == "approve" {
		ruleCreated, err := s.db.ApproveMistakeAndCreateRule(r.Context(), fingerprint, ruleFromApprovedMistake)
		if err != nil {
			if errors.Is(err, store.ErrMistakeNotFound) {
				writeError(w, http.StatusNotFound, "mistake not found")
				return
			}
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("原子入库失败（已回滚）: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"fingerprint":   fingerprint,
			"status":        "promoted",
			"review_status": "approved",
			"rule_created":  ruleCreated,
		})
		return
	}
	if err := s.db.UpdateMistakeStatus(r.Context(), fingerprint, targetStatus); err != nil {
		if errors.Is(err, store.ErrMistakeNotFound) {
			writeError(w, http.StatusNotFound, "mistake not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "fingerprint": fingerprint, "status": targetStatus})
}

type ruleResponse struct {
	store.Rule
	RuleKey string `json:"rule_key"`
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	query := store.RuleLedgerQuery{
		ProjectPath: r.URL.Query().Get("project"),
		Engine:      r.URL.Query().Get("engine"),
		Scope:       r.URL.Query().Get("scope"),
		Status:      r.URL.Query().Get("status"),
	}
	rules, err := s.db.ListRuleLedger(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]ruleResponse, 0, len(rules))
	for _, rule := range rules {
		out = append(out, ruleResponse{
			Rule:    rule,
			RuleKey: rule.RuleID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

func (s *Server) handleRadar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	top := 5
	if raw := strings.TrimSpace(r.URL.Query().Get("top")); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &top); err != nil || top <= 0 {
			writeError(w, http.StatusBadRequest, "top must be a positive integer")
			return
		}
	}
	if project == "" {
		writeJSON(w, http.StatusOK, emptyRadarResponse(""))
		return
	}
	info, err := os.Stat(project)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, emptyRadarResponse(project))
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusOK, emptyRadarResponse(project))
		return
	}
	baseline, err := radar.LoadBaselineStatus(project)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	report, err := radar.ScanReport(r.Context(), radar.Options{Root: project, Top: top, Runner: s.runner})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	screenshots, err := adapter.ListUIScreenshots(project)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hotspots := report.Hotspots
	if hotspots == nil {
		hotspots = []radar.Hotspot{}
	}
	skipped := report.Skipped
	if skipped == nil {
		skipped = []radar.Skipped{}
	}
	delta := make(map[string]int, len(baseline.Metrics))
	for _, m := range baseline.Metrics {
		delta[m.Name] = m.Delta
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project":     project,
		"baseline":    baseline,
		"delta":       delta,
		"hotspots":    hotspots,
		"skipped":     skipped,
		"screenshots": screenshots,
	})
}

func emptyRadarResponse(project string) map[string]any {
	return map[string]any{
		"project": project,
		"baseline": map[string]any{
			"available": false,
			"metrics":   []any{},
		},
		"delta":       map[string]int{},
		"hotspots":    []radar.Hotspot{},
		"skipped":     []radar.Skipped{},
		"screenshots": []adapter.UIScreenshot{},
	}
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if project == "" || path == "" {
		writeError(w, http.StatusBadRequest, "project and path are required")
		return
	}
	fullPath, err := safeProjectFile(project, path)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	http.ServeFile(w, r, fullPath)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

type projectConfigSummary struct {
	Exists   bool     `json:"exists"`
	Path     string   `json:"path,omitempty"`
	Version  int      `json:"version,omitempty"`
	Adapters []string `json:"adapters,omitempty"`
	Verify   struct {
		Jobs int `json:"jobs,omitempty"`
	} `json:"verify,omitempty"`
	Error string `json:"error,omitempty"`
}

type uiConfigSummary struct {
	Exists        bool     `json:"exists"`
	Path          string   `json:"path,omitempty"`
	BaseURL       string   `json:"base_url,omitempty"`
	Routes        []string `json:"routes,omitempty"`
	ScreenshotDir string   `json:"screenshot_dir,omitempty"`
	TimeoutSec    int      `json:"timeout_sec,omitempty"`
	Error         string   `json:"error,omitempty"`
}

type adapterCheckMeta struct {
	ID         string   `json:"id"`
	Cmd        []string `json:"cmd"`
	Optional   bool     `json:"optional"`
	TimeoutSec int      `json:"timeout_sec"`
}

type adapterEscapePatternMeta struct {
	Name  string `json:"name"`
	Regex string `json:"regex"`
}

type adapterMeta struct {
	Name           string                     `json:"name"`
	Extensions     []string                   `json:"extensions"`
	Checks         []adapterCheckMeta         `json:"checks"`
	EscapePatterns []adapterEscapePatternMeta `json:"escape_patterns"`
}

func (s *Server) handleProjectMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"project":   "",
			"exists":    false,
			"config":    projectConfigSummary{Exists: false},
			"ui_config": uiConfigSummary{Exists: false},
			"adapters":  []adapterMeta{},
		})
		return
	}
	info, err := os.Stat(project)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{
				"project":   project,
				"exists":    false,
				"config":    projectConfigSummary{Exists: false},
				"ui_config": uiConfigSummary{Exists: false},
				"adapters":  []adapterMeta{},
			})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "project path is not a directory")
		return
	}

	cfgSummary := loadProjectConfigSummary(project)
	uiSummary := loadUIConfigSummary(project)

	registry, err := adapter.LoadMergedRegistry(project)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	detected := registry.Detect(project)
	adaptersOut := make([]adapterMeta, 0, len(detected))
	for _, a := range detected {
		checks := a.Checks(project, nil)
		checkMetas := make([]adapterCheckMeta, 0, len(checks))
		for _, c := range checks {
			checkMetas = append(checkMetas, adapterCheckMeta{
				ID:         c.ID,
				Cmd:        c.Cmd,
				Optional:   c.Optional,
				TimeoutSec: int(c.Timeout.Seconds()),
			})
		}
		patterns := a.EscapePatterns()
		patternMetas := make([]adapterEscapePatternMeta, 0, len(patterns))
		for _, p := range patterns {
			patternMetas = append(patternMetas, adapterEscapePatternMeta{
				Name:  p.Name,
				Regex: p.Regex,
			})
		}
		exts := a.Extensions()
		if exts == nil {
			exts = []string{}
		}
		adaptersOut = append(adaptersOut, adapterMeta{
			Name:           a.Name(),
			Extensions:     exts,
			Checks:         checkMetas,
			EscapePatterns: patternMetas,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"project":   project,
		"exists":    true,
		"config":    cfgSummary,
		"ui_config": uiSummary,
		"adapters":  adaptersOut,
	})
}

func loadProjectConfigSummary(project string) projectConfigSummary {
	rel := filepath.FromSlash(".slopguard/config.yaml")
	full := filepath.Join(project, rel)
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return projectConfigSummary{Exists: false}
		}
		return projectConfigSummary{Exists: true, Path: full, Error: err.Error()}
	}
	var raw struct {
		Version  int      `yaml:"version"`
		Adapters []string `yaml:"adapters"`
		Verify   struct {
			Jobs int `yaml:"jobs"`
		} `yaml:"verify"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return projectConfigSummary{Exists: true, Path: full, Error: err.Error()}
	}
	summary := projectConfigSummary{
		Exists:   true,
		Path:     full,
		Version:  raw.Version,
		Adapters: raw.Adapters,
	}
	summary.Verify.Jobs = raw.Verify.Jobs
	return summary
}

func loadUIConfigSummary(project string) uiConfigSummary {
	for _, rel := range []string{filepath.FromSlash(".slopguard/ui.yaml"), filepath.FromSlash(".slopguard/ui.yml")} {
		full := filepath.Join(project, rel)
		data, err := os.ReadFile(full)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return uiConfigSummary{Exists: true, Path: full, Error: err.Error()}
		}
		var raw struct {
			BaseURL       string   `yaml:"base_url"`
			Routes        []string `yaml:"routes"`
			ScreenshotDir string   `yaml:"screenshot_dir"`
			TimeoutSec    int      `yaml:"timeout_sec"`
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return uiConfigSummary{Exists: true, Path: full, Error: err.Error()}
		}
		return uiConfigSummary{
			Exists:        true,
			Path:          full,
			BaseURL:       raw.BaseURL,
			Routes:        raw.Routes,
			ScreenshotDir: raw.ScreenshotDir,
			TimeoutSec:    raw.TimeoutSec,
		}
	}
	return uiConfigSummary{Exists: false}
}

type verifyRequest struct {
	Project    string `json:"project"`
	All        bool   `json:"all"`
	LastCommit bool   `json:"last_commit"`
	UI         bool   `json:"ui"`
	Record     bool   `json:"record"`
	Source     string `json:"source"`
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req verifyRequest
	if !decodeJSONBody(w, r, &req, false) {
		return
	}
	project := strings.TrimSpace(req.Project)
	if project == "" {
		writeError(w, http.StatusBadRequest, "project is required")
		return
	}
	absProject, err := filepath.Abs(project)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := os.Stat(absProject)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusBadRequest, "project directory does not exist")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "project path is not a directory")
		return
	}

	runner := s.runner
	if runner == nil {
		runner = engine.NewExecRunner()
	}

	result, err := s.runVerify(r.Context(), runner, absProject, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) runVerify(ctx context.Context, runner contract.Runner, project string, req verifyRequest) (contract.VerifyResult, error) {
	started := time.Now()
	cfg := loadProjectConfigSummary(project)
	jobs := cfg.Verify.Jobs
	if jobs <= 0 {
		jobs = 2
	}

	registry, err := adapter.LoadMergedRegistry(project)
	if err != nil {
		return contract.VerifyResult{}, fmt.Errorf("load adapters: %w", err)
	}
	detected := registry.Detect(project)
	adapters := filterVerifyAdapters(detected, cfg.Adapters)
	if len(adapters) == 0 {
		if len(cfg.Adapters) > 0 {
			return contract.VerifyResult{}, fmt.Errorf("none of the configured adapters %v apply to this project", cfg.Adapters)
		}
		result := contract.VerifyResult{
			Passed:     true,
			Findings:   []contract.Finding{},
			Escapes:    map[string]int{},
			DurationMs: time.Since(started).Milliseconds(),
		}
		if req.UI {
			uiRes, uiErr := adapter.RunUISmoke(ctx, runner, adapter.UIOptions{ProjectRoot: project})
			if uiErr != nil {
				return contract.VerifyResult{}, fmt.Errorf("run ui smoke: %w", uiErr)
			}
			result.UI = &uiRes
			if !uiRes.Skipped && !uiRes.Passed {
				result.Passed = false
			}
			result.DurationMs = time.Since(started).Milliseconds()
		}
		if req.Record && s.db != nil {
			_ = s.db.RecordScan(ctx, project, result.DurationMs, 0, result.Passed)
		}
		return result, nil
	}

	var patterns []contract.EscapePattern
	var extensions []string
	extSeen := make(map[string]struct{})
	for _, a := range adapters {
		patterns = append(patterns, a.EscapePatterns()...)
		for _, ext := range a.Extensions() {
			if _, ok := extSeen[ext]; !ok {
				extSeen[ext] = struct{}{}
				extensions = append(extensions, ext)
			}
		}
	}

	baseline, err := engine.LoadBaselineOrEmpty(filepath.Join(project, ".slopguard", "baseline.json"))
	if err != nil {
		return contract.VerifyResult{}, fmt.Errorf("load baseline: %w", err)
	}

	var files []string
	if req.All {
		files, err = engine.ListFiles(project, extensions)
	} else {
		git := engine.NewGit(runner, project)
		if git.IsRepository(ctx) {
			if req.LastCommit {
				files, err = git.LastCommitFiles(ctx)
				if err == nil && len(files) == 0 {
					files, err = git.ChangedFiles(ctx)
				}
			} else {
				files, err = git.ChangedFiles(ctx)
			}
			if err == nil {
				files = filterVerifyFilesByExt(files, extensions)
			}
		} else {
			files, err = engine.ListFiles(project, extensions)
		}
	}
	if err != nil {
		return contract.VerifyResult{}, fmt.Errorf("resolve scan scope: %w", err)
	}

	files, suppressedChecks := filterVerifySuppressed(files, baseline)

	type plannedTask struct {
		adapter contract.Adapter
		check   contract.Check
	}
	var tasks []plannedTask
	var skippedIDs []string
	var skippedDetails []contract.SkippedDetail

	for _, a := range adapters {
		subset := filterVerifyFilesByExt(files, a.Extensions())
		if len(subset) == 0 {
			continue
		}
		plan, err := adapter.PlanChecks(runner, project, a.Checks(project, subset))
		if err != nil {
			return contract.VerifyResult{}, err
		}
		for _, sk := range plan.Skipped {
			skippedIDs = append(skippedIDs, sk.ID)
			skippedDetails = append(skippedDetails, contract.SkippedDetail{
				ID:     sk.ID,
				Tool:   sk.Tool,
				Reason: sk.Reason,
			})
		}
		for _, c := range plan.Runnable {
			if len(c.Cmd) == 0 {
				continue
			}
			tasks = append(tasks, plannedTask{adapter: a, check: c})
		}
	}

	checks := make([]contract.Check, len(tasks))
	for i, t := range tasks {
		checks[i] = t.check
	}
	outcomes := engine.RunChecksConcurrent(ctx, runner, project, checks, jobs)

	findings := make([]contract.Finding, 0)
	for i, outcome := range outcomes {
		res := outcome.Result
		if res.Err != nil {
			return contract.VerifyResult{}, fmt.Errorf("check %s: %w", outcome.Check.ID, res.Err)
		}
		if res.Skipped {
			tool := ""
			if len(outcome.Check.Cmd) > 0 {
				tool = outcome.Check.Cmd[0]
			}
			skippedIDs = append(skippedIDs, outcome.Check.ID)
			skippedDetails = append(skippedDetails, contract.SkippedDetail{
				ID:     outcome.Check.ID,
				Tool:   tool,
				Reason: "tool missing at execution time",
			})
			continue
		}
		if res.ExitCode == 0 {
			continue
		}
		findings = append(findings, adapter.ParseFindings(tasks[i].adapter, outcome.Check.ID, res)...)
	}

	escapes := map[string]int{}
	escapeFiles := files
	if !req.All && (len(patterns) > 0 || len(baseline.SuppressedFiles) > 0) {
		escapeFiles, err = engine.ListFiles(project, extensions)
		if err != nil {
			return contract.VerifyResult{}, fmt.Errorf("list files for escape counting: %w", err)
		}
	}
	escapeFiles, suppressedEscapes := filterVerifySuppressed(escapeFiles, baseline)
	if len(patterns) > 0 && len(escapeFiles) > 0 {
		escapes, err = engine.CountEscapes(project, escapeFiles, patterns)
		if err != nil {
			return contract.VerifyResult{}, fmt.Errorf("count escapes: %w", err)
		}
	}
	if len(patterns) > 0 && len(baseline.Metrics) > 0 {
		cmp := engine.Compare(escapes, baseline)
		for _, v := range cmp.Regressions {
			findings = append(findings, contract.Finding{
				Rule:     "ratchet:" + v.Name,
				Message:  fmt.Sprintf("escape hatch %s increased from baseline %d to %d (+%d)", v.Name, v.Baseline, v.Current, v.Delta),
				Severity: contract.SeverityError,
			})
		}
	}

	suppressedSet := make(map[string]struct{}, len(suppressedChecks)+len(suppressedEscapes))
	for _, f := range suppressedChecks {
		suppressedSet[f] = struct{}{}
	}
	for _, f := range suppressedEscapes {
		suppressedSet[f] = struct{}{}
	}

	result := contract.VerifyResult{
		Passed:         len(findings) == 0,
		Findings:       findings,
		Escapes:        escapes,
		Baseline:       baseline.Metrics,
		Suppressed:     len(suppressedSet),
		DurationMs:     time.Since(started).Milliseconds(),
		Skipped:        skippedIDs,
		SkippedDetails: skippedDetails,
	}
	if req.UI && result.Passed {
		uiRes, uiErr := adapter.RunUISmoke(ctx, runner, adapter.UIOptions{ProjectRoot: project})
		if uiErr != nil {
			return contract.VerifyResult{}, fmt.Errorf("run ui smoke: %w", uiErr)
		}
		result.UI = &uiRes
		if !uiRes.Skipped && !uiRes.Passed {
			result.Passed = false
		}
		result.DurationMs = time.Since(started).Milliseconds()
	}
	if req.Record && s.db != nil {
		totalEscapes := 0
		for _, v := range result.Escapes {
			totalEscapes += v
		}
		_ = s.db.RecordScan(ctx, project, result.DurationMs, totalEscapes, result.Passed)
		if !result.Passed && len(result.Findings) > 0 {
			autoCaptureVerifyMistakesUI(ctx, s.db, project, result.Findings, req.Source)
		}
	}
	return result, nil
}

const maxAutoCapturedMistakesUI = 5

func autoCaptureVerifyMistakesUI(ctx context.Context, db *store.Store, root string, findings []contract.Finding, source string) {
	if db == nil {
		return
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "auto_verify"
	}
	seen := make(map[string]struct{}, len(findings))
	captured := 0
	for _, f := range findings {
		if captured >= maxAutoCapturedMistakesUI {
			break
		}
		category := classifyFindingCategoryUI(f)
		summary := buildFindingSummaryUI(f)
		if summary == "" {
			continue
		}
		fp := mistakeFingerprintUI(root, category, summary)
		if _, dup := seen[fp]; dup {
			continue
		}
		seen[fp] = struct{}{}
		_, err := db.RecordMistake(ctx, contract.Mistake{
			ProjectPath: root,
			Fingerprint: fp,
			Category:    category,
			Summary:     summary,
			BadSnippet:  formatFindingSnippetUI(f),
			Source:      source,
			Status:      "pending",
		})
		if err != nil {
			return
		}
		captured++
	}
}

func mistakeFingerprintUI(project, category, summary string) string {
	h := sha256.Sum256([]byte(project + "\x00" + strings.TrimSpace(category) + "\x00" + strings.TrimSpace(summary)))
	return hex.EncodeToString(h[:])
}

func classifyFindingCategoryUI(f contract.Finding) string {
	rule := strings.ToLower(strings.TrimSpace(f.Rule))
	msg := strings.ToLower(strings.TrimSpace(f.Message))
	switch {
	case strings.Contains(rule, "ratchet") || strings.Contains(rule, "escape") || strings.Contains(msg, "baseline"):
		return "escape"
	case strings.Contains(rule, "import") || strings.Contains(msg, "import"):
		return "import"
	case strings.Contains(rule, "type") || rule == "tsc" || strings.Contains(rule, "mypy") || strings.Contains(rule, "pyright"):
		return "type"
	case strings.Contains(rule, "syntax") || strings.Contains(msg, "syntax"):
		return "syntax"
	default:
		return "lint"
	}
}

func buildFindingSummaryUI(f contract.Finding) string {
	rule := strings.TrimSpace(f.Rule)
	msg := strings.TrimSpace(f.Message)
	if len(msg) > 140 {
		msg = msg[:140] + "..."
	}
	if rule != "" && msg != "" {
		return fmt.Sprintf("[%s] %s", rule, msg)
	}
	if rule != "" {
		return rule
	}
	return msg
}

func formatFindingSnippetUI(f contract.Finding) string {
	loc := strings.TrimSpace(f.File)
	if loc != "" && f.Line > 0 {
		if f.Column > 0 {
			loc = fmt.Sprintf("%s:%d:%d", loc, f.Line, f.Column)
		} else {
			loc = fmt.Sprintf("%s:%d", loc, f.Line)
		}
	}
	msg := strings.TrimSpace(f.Message)
	if loc != "" && msg != "" {
		return loc + " — " + msg
	}
	if loc != "" {
		return loc
	}
	return msg
}

func filterVerifyAdapters(adapters []contract.Adapter, allow []string) []contract.Adapter {
	if len(allow) == 0 {
		return adapters
	}
	want := make(map[string]struct{}, len(allow))
	for _, name := range allow {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			want[trimmed] = struct{}{}
		}
	}
	out := make([]contract.Adapter, 0, len(adapters))
	for _, a := range adapters {
		if _, ok := want[a.Name()]; ok {
			out = append(out, a)
		}
	}
	return out
}

func filterVerifyFilesByExt(files, extensions []string) []string {
	if len(extensions) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		allowed[strings.ToLower(ext)] = struct{}{}
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		if _, ok := allowed[strings.ToLower(filepath.Ext(f))]; ok {
			out = append(out, f)
		}
	}
	return out
}

func filterVerifySuppressed(files []string, baseline *engine.Baseline) (kept, suppressed []string) {
	kept = make([]string, 0, len(files))
	for _, f := range files {
		if baseline != nil && baseline.IsSuppressed(f) {
			suppressed = append(suppressed, f)
			continue
		}
		kept = append(kept, f)
	}
	return kept, suppressed
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func safeProjectFile(project, path string) (string, error) {
	root, err := filepath.Abs(project)
	if err != nil {
		return "", err
	}
	joined := path
	if !filepath.IsAbs(path) {
		joined = filepath.Join(root, path)
	}
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", errors.New("screenshot path is outside the project")
	}
	return abs, nil
}

type serviceStatusResponse struct {
	Platform  string `json:"platform"`
	Installed bool   `json:"installed"`
	Port      int    `json:"port"`
	PIDHint   int    `json:"pid_hint"`
}

func (s *Server) handleServiceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	port := s.port
	if port == 0 {
		port = 7788
	}
	homeDir, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, detectServiceStatus(runtime.GOOS, homeDir, port, fileExists))
}

func detectServiceStatus(goos, homeDir string, port int, exists func(string) bool) serviceStatusResponse {
	installed := false
	if goos == "darwin" && homeDir != "" && exists != nil {
		plistPath := filepath.Join(homeDir, "Library", "LaunchAgents", "com.slopguard.ui.plist")
		installed = exists(plistPath)
	}
	return serviceStatusResponse{
		Platform:  goos,
		Installed: installed,
		Port:      port,
		PIDHint:   os.Getpid(),
	}
}

func openBrowser(url string) error {
	name, args := openBrowserCommand(runtime.GOOS, url, fileExists, exec.LookPath)
	return exec.Command(name, args...).Start()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func openBrowserCommand(goos, targetURL string, exists func(string) bool, lookPath func(string) (string, error)) (string, []string) {
	appArgs := []string{"--app=" + targetURL, "--window-size=1180,780"}

	switch goos {
	case "darwin":
		if parsed, err := neturl.Parse(targetURL); err == nil && parsed.Port() == "7788" {
			var appBundles []string
			appBundles = append(appBundles, "/Applications/SlopGuard.app")
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				appBundles = append(appBundles, filepath.Join(home, "Applications", "SlopGuard.app"))
			}
			appBundles = append(appBundles, filepath.Join("build", "SlopGuard.app"))
			for _, bundle := range appBundles {
				if exists(filepath.Join(bundle, "Contents", "MacOS", "SlopGuard")) {
					return "open", []string{bundle}
				}
			}
		}

		var candidates []string
		appRelPaths := []string{
			"Google Chrome.app/Contents/MacOS/Google Chrome",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"Brave Browser.app/Contents/MacOS/Brave Browser",
			"Arc.app/Contents/MacOS/Arc",
			"Chromium.app/Contents/MacOS/Chromium",
		}
		for _, rel := range appRelPaths {
			candidates = append(candidates, filepath.Join("/Applications", rel))
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			for _, rel := range appRelPaths {
				candidates = append(candidates, filepath.Join(home, "Applications", rel))
			}
		}
		for _, candidate := range candidates {
			if exists(candidate) {
				return candidate, appArgs
			}
		}
		return "open", []string{targetURL}

	case "windows":
		for _, bin := range []string{"chrome.exe", "msedge.exe", "brave.exe", "chromium.exe"} {
			if path, err := lookPath(bin); err == nil && path != "" {
				return path, appArgs
			}
		}
		relPaths := []string{
			`Google\Chrome\Application\chrome.exe`,
			`Microsoft\Edge\Application\msedge.exe`,
			`BraveSoftware\Brave-Browser\Application\brave.exe`,
			`Chromium\Application\chrome.exe`,
		}
		for _, envKey := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if base := os.Getenv(envKey); base != "" {
				for _, rel := range relPaths {
					candidate := filepath.Join(base, rel)
					if exists(candidate) {
						return candidate, appArgs
					}
				}
			}
		}
		return "rundll32", []string{"url.dll,FileProtocolHandler", targetURL}

	default:
		for _, bin := range []string{
			"google-chrome",
			"google-chrome-stable",
			"chromium",
			"chromium-browser",
			"microsoft-edge",
			"microsoft-edge-stable",
			"brave-browser",
			"brave",
		} {
			if path, err := lookPath(bin); err == nil && path != "" {
				return path, appArgs
			}
		}
		return "xdg-open", []string{targetURL}
	}
}
