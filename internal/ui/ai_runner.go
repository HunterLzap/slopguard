package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/HunterLzap/slopguard/internal/engine"
	"github.com/HunterLzap/slopguard/internal/store"
)

const aiAnalysisJSONSchema = `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"fingerprint":{"type":"string"},"decision":{"type":"string","enum":["recommend","skip"]},"reason":{"type":"string"},"rule_draft":{"type":"string"}},"required":["fingerprint","decision","reason"]}}},"required":["items"]}`

// AgentCLIRunner describes an installed host Agent CLI usable for headless AI analysis.
type AgentCLIRunner struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	BinaryPath   string `json:"binary_path"`
	Available    bool   `json:"available"`
	ReadOnlyMode string `json:"read_only_mode"`
}

// AIConfig persists the user's preference for automatic vs manual Agent model analysis.
type AIConfig struct {
	AutoAnalyze bool   `json:"auto_analyze"`
	RunnerID    string `json:"runner_id,omitempty"` // "" selects first available runner
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// AIAnalysisItem represents one mistake classification returned by the host Agent model.
type AIAnalysisItem struct {
	Fingerprint string `json:"fingerprint"`
	Decision    string `json:"decision"`
	Reason      string `json:"reason"`
	RuleDraft   string `json:"rule_draft,omitempty"`
}

// AIRuntimeStatus tracks live progress of headless Agent model analysis.
type AIRuntimeStatus struct {
	Running        bool             `json:"running"`
	ActiveRunner   string           `json:"active_runner,omitempty"`
	AutoAnalyze    bool             `json:"auto_analyze"`
	RunnerID       string           `json:"runner_id,omitempty"`
	Runners        []AgentCLIRunner `json:"runners"`
	LastRunAt      string           `json:"last_run_at,omitempty"`
	LastAnalyzed   int              `json:"last_analyzed"`
	LastDurationMS int64            `json:"last_duration_ms"`
	LastError      string           `json:"last_error,omitempty"`
}

type aiAnalyzeFunc func(ctx context.Context, runner AgentCLIRunner, project string, mistakes []store.MistakeRecord) ([]AIAnalysisItem, error)

// AIManager coordinates headless host-Agent CLI invocation for genuine LLM mistake analysis
// while keeping the SlopGuard binary itself free of embedded models or API keys.
type AIManager struct {
	server     *Server
	mu         sync.Mutex
	cfg        AIConfig
	loadedHome string
	running    bool
	pending    bool
	pendingPrj string
	status     AIRuntimeStatus
	analyzeFn  aiAnalyzeFunc
	rootCtx    context.Context
	wg         sync.WaitGroup
}

func newAIManager(server *Server) *AIManager {
	return &AIManager{
		server: server,
	}
}

// Start binds AIManager background tasks to the server root context.
func (m *AIManager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.rootCtx = ctx
	m.mu.Unlock()
}

// Wait waits for any active background analysis goroutine to exit during shutdown.
func (m *AIManager) Wait() {
	if m == nil {
		return
	}
	m.wg.Wait()
}

// augmentedCLIEnv constructs a minimal, isolated environment whitelist for host Agent CLI
// invocations without forwarding arbitrary parent secrets (API keys / tokens).
func augmentedCLIEnv(home string) []string {
	var extra []string
	if home != "" {
		extra = append(extra,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
		)
	}
	if runtime.GOOS == "darwin" {
		extra = append(extra, "/opt/homebrew/bin", "/usr/local/bin")
	} else if runtime.GOOS != "windows" {
		extra = append(extra, "/usr/local/bin", "/usr/bin", "/bin")
	}

	var allowKeys []string
	if runtime.GOOS == "windows" {
		allowKeys = []string{
			"USERPROFILE", "HOMEPATH", "HOMEDRIVE", "APPDATA", "LOCALAPPDATA",
			"TEMP", "TMP", "SystemRoot", "ComSpec", "OS", "PATHEXT", "LANG",
		}
	} else {
		allowKeys = []string{
			"USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
			"TERM", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
		}
	}

	env := make([]string, 0, len(allowKeys)+2)
	if home != "" {
		env = append(env, "HOME="+home)
		if runtime.GOOS == "windows" {
			env = append(env, "USERPROFILE="+home)
		}
	} else if v := os.Getenv("HOME"); v != "" {
		env = append(env, "HOME="+v)
	}

	for _, key := range allowKeys {
		if key == "USERPROFILE" && home != "" {
			continue
		}
		if val := os.Getenv(key); val != "" {
			env = append(env, key+"="+val)
		}
	}

	prefix := strings.Join(extra, string(os.PathListSeparator))
	currPath := os.Getenv("PATH")
	var mergedPath string
	switch {
	case prefix != "" && currPath != "":
		mergedPath = prefix + string(os.PathListSeparator) + currPath
	case prefix != "":
		mergedPath = prefix
	default:
		mergedPath = currPath
	}
	env = append(env, "PATH="+mergedPath)
	return env
}

func lookupAgentCLIBinary(home, name string) string {
	candidates := make([]string, 0, 5)
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", name),
			filepath.Join(home, "bin", name),
		)
	}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, filepath.Join("/opt/homebrew/bin", name), filepath.Join("/usr/local/bin", name))
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			if runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0 {
				return p
			}
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		return found
	}
	return ""
}

func discoverAgentCLIRunners(home string) []AgentCLIRunner {
	specs := []struct {
		id       string
		name     string
		bin      string
		readOnly string
	}{
		{id: "agy", name: "Antigravity CLI (agy)", bin: "agy", readOnly: "plan+sandbox"},
		{id: "claude", name: "Claude Code CLI (claude)", bin: "claude", readOnly: "plan"},
		{id: "codex", name: "Codex CLI (codex)", bin: "codex", readOnly: "read-only"},
	}
	out := make([]AgentCLIRunner, 0, len(specs))
	for _, spec := range specs {
		p := lookupAgentCLIBinary(home, spec.bin)
		out = append(out, AgentCLIRunner{
			ID:           spec.id,
			Name:         spec.name,
			BinaryPath:   p,
			Available:    p != "" && spec.readOnly != "",
			ReadOnlyMode: spec.readOnly,
		})
	}
	return out
}

func (m *AIManager) configPath() string {
	home := m.server.resolveHomeDir()
	if strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".slopguard", "ai-config.json")
}

func (m *AIManager) ensureLoadedLocked() {
	home := m.server.resolveHomeDir()
	if m.loadedHome == home {
		return
	}
	m.loadedHome = home
	// AutoAnalyze is strictly opt-in (false by default and on missing/corrupt config).
	m.cfg = AIConfig{AutoAnalyze: false}

	path := m.configPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var parsed AIConfig
	if err := json.Unmarshal(data, &parsed); err == nil {
		m.cfg = parsed
	}
}

func (m *AIManager) saveLocked() error {
	path := m.configPath()
	if path == "" {
		return errors.New("无法定位用户目录")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Dir(path), 0o700)
	encoded, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(path, encoded, 0o600)
}

func (m *AIManager) selectRunner(home, preferredID string) (AgentCLIRunner, bool) {
	runners := discoverAgentCLIRunners(home)
	preferredID = strings.TrimSpace(preferredID)
	if preferredID != "" {
		for _, r := range runners {
			if r.ID == preferredID && r.Available && r.ReadOnlyMode != "" {
				return r, true
			}
		}
		// Never silently fail over to another provider when the user selected a specific runner.
		return AgentCLIRunner{}, false
	}
	for _, r := range runners {
		if r.Available && r.ReadOnlyMode != "" {
			return r, true
		}
	}
	return AgentCLIRunner{}, false
}

// Status returns the current AI configuration, available CLI runners, and live runtime state.
func (m *AIManager) Status() AIRuntimeStatus {
	home := m.server.resolveHomeDir()
	m.mu.Lock()
	m.ensureLoadedLocked()
	st := m.status
	st.Running = m.running
	st.AutoAnalyze = m.cfg.AutoAnalyze
	st.RunnerID = m.cfg.RunnerID
	m.mu.Unlock()

	st.Runners = discoverAgentCLIRunners(home)
	if st.RunnerID == "" {
		if r, ok := m.selectRunner(home, ""); ok {
			st.ActiveRunner = r.ID
		}
	} else {
		st.ActiveRunner = st.RunnerID
	}
	return st
}

// SetConfig updates AutoAnalyze or preferred CLI runner and persists to disk.
func (m *AIManager) SetConfig(autoAnalyze *bool, runnerID *string) (AIRuntimeStatus, error) {
	home := m.server.resolveHomeDir()
	m.mu.Lock()
	m.ensureLoadedLocked()
	nextCfg := m.cfg
	if runnerID != nil {
		id := strings.TrimSpace(*runnerID)
		if id != "" && id != "agy" && id != "claude" && id != "codex" {
			m.mu.Unlock()
			return m.Status(), fmt.Errorf("未知的模型 CLI 标识: %s", id)
		}
		nextCfg.RunnerID = id
	}
	if autoAnalyze != nil {
		if *autoAnalyze {
			if _, ok := m.selectRunner(home, nextCfg.RunnerID); !ok {
				m.mu.Unlock()
				if nextCfg.RunnerID != "" {
					return m.Status(), fmt.Errorf("所选模型 CLI (%s) 当前不可用，无法开启自动调用", nextCfg.RunnerID)
				}
				return m.Status(), errors.New("未探测到可用的本机 Agent CLI，无法开启自动调用")
			}
		}
		nextCfg.AutoAnalyze = *autoAnalyze
	}
	nextCfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	m.cfg = nextCfg
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return m.Status(), err
	}
	return m.Status(), nil
}

// TriggerAsync schedules non-blocking real Agent CLI analysis when AutoAnalyze is enabled.
// If a run is already in progress, marks a pending follow-up so newly arrived mistakes are not dropped.
func (m *AIManager) TriggerAsync(project string) {
	if m == nil || m.server == nil || m.server.db == nil {
		return
	}
	home := m.server.resolveHomeDir()
	m.mu.Lock()
	m.ensureLoadedLocked()
	if !m.cfg.AutoAnalyze {
		m.mu.Unlock()
		return
	}
	if _, ok := m.selectRunner(home, m.cfg.RunnerID); !ok {
		m.mu.Unlock()
		return
	}
	if m.running {
		m.pending = true
		if strings.TrimSpace(project) != "" {
			m.pendingPrj = strings.TrimSpace(project)
		}
		m.mu.Unlock()
		return
	}
	parentCtx := m.rootCtx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	if parentCtx.Err() != nil {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()

	go func(initialProject string) {
		defer m.wg.Done()
		nextProj := initialProject
		for {
			if parentCtx.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(parentCtx, 95*time.Second)
			_, _, _, _ = m.AnalyzePending(ctx, nextProj)
			cancel()

			m.mu.Lock()
			if !m.pending || !m.cfg.AutoAnalyze || parentCtx.Err() != nil {
				m.pending = false
				m.pendingPrj = ""
				m.mu.Unlock()
				return
			}
			nextProj = m.pendingPrj
			m.pending = false
			m.pendingPrj = ""
			m.mu.Unlock()
		}
	}(project)
}

// AnalyzePending invokes the selected host Agent CLI to semantically analyze pending_ai mistakes.
func (m *AIManager) AnalyzePending(ctx context.Context, project string) (analyzed, recommended, skipped int, err error) {
	if m == nil || m.server == nil || m.server.db == nil {
		return 0, 0, 0, errors.New("store is not available")
	}

	home := m.server.resolveHomeDir()
	m.mu.Lock()
	m.ensureLoadedLocked()
	if m.running {
		m.mu.Unlock()
		return 0, 0, 0, errors.New("已有 AI 分析任务正在运行中，请稍候")
	}
	preferred := strings.TrimSpace(m.cfg.RunnerID)
	analyzeFn := m.analyzeFn
	runner, ok := m.selectRunner(home, preferred)
	if !ok {
		if analyzeFn == nil {
			m.mu.Unlock()
			if preferred != "" {
				return 0, 0, 0, fmt.Errorf("所选模型 CLI (%s) 当前不可用或不支持只读模式，已取消分析（不会静默切换至其他模型）", preferred)
			}
			return 0, 0, 0, errors.New("未探测到可用的本机 Agent CLI（如 agy / claude / codex），请先安装并登录对应客户端，或在编辑器内通过 MCP 触发分析")
		}
		// Only reachable in unit tests that inject a custom analyzeFn when no CLI binary is in test PATH.
		runner = AgentCLIRunner{ID: "mock", Name: "Mock Agent", Available: true, ReadOnlyMode: "read-only"}
	}
	if analyzeFn == nil {
		analyzeFn = invokeHostAgentCLI
	}
	m.running = true
	m.status.Running = true
	m.status.ActiveRunner = runner.ID
	m.status.LastError = ""
	m.mu.Unlock()

	startedAt := time.Now()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.status.Running = false
		m.status.LastRunAt = time.Now().UTC().Format(time.RFC3339)
		m.status.LastDurationMS = time.Since(startedAt).Milliseconds()
		m.status.LastAnalyzed = analyzed
		if err != nil {
			m.status.LastError = err.Error()
		}
		m.mu.Unlock()
	}()

	pending, qErr := m.server.db.ListInboxMistakes(ctx, strings.TrimSpace(project), "pending_ai")
	if qErr != nil {
		err = qErr
		return 0, 0, 0, err
	}
	if len(pending) == 0 {
		return 0, 0, 0, nil
	}

	const maxPerBatch = 10
	batch := pending
	if len(batch) > maxPerBatch {
		batch = pending[:maxPerBatch]
	}

	items, callErr := analyzeFn(ctx, runner, project, batch)
	if callErr != nil {
		err = callErr
		return 0, 0, 0, err
	}

	validFPs := make(map[string]store.MistakeRecord, len(batch))
	for _, rec := range batch {
		validFPs[rec.Fingerprint] = rec
	}

	seenFPs := make(map[string]struct{}, len(items))
	invalidCount := 0
	for _, item := range items {
		fp := strings.TrimSpace(item.Fingerprint)
		if fp == "" {
			continue
		}
		if _, exists := validFPs[fp]; !exists {
			continue
		}
		if _, dup := seenFPs[fp]; dup {
			continue
		}
		decision := strings.ToLower(strings.TrimSpace(item.Decision))
		if decision != "recommend" && decision != "skip" {
			invalidCount++
			continue
		}
		reason := strings.TrimSpace(item.Reason)
		if reason == "" {
			invalidCount++
			continue
		}
		seenFPs[fp] = struct{}{}
		ruleDraft := strings.TrimSpace(item.RuleDraft)
		if subErr := m.server.db.SubmitMistakeAnalysis(ctx, fp, decision, reason, ruleDraft); subErr != nil {
			err = fmt.Errorf("写入 AI 分析结果失败: %w", subErr)
			return analyzed, recommended, skipped, err
		}
		analyzed++
		if decision == "recommend" {
			recommended++
		} else {
			skipped++
		}
	}
	if analyzed == 0 {
		if invalidCount > 0 {
			err = fmt.Errorf("Agent 模型返回了 %d 条格式非法的 decision/reason（保持待分析状态）", invalidCount)
		} else {
			err = errors.New("Agent 模型未返回可匹配当前错题的分析结果")
		}
		return 0, 0, 0, err
	}
	return analyzed, recommended, skipped, nil
}

func buildMistakeAnalysisPrompt(batch []store.MistakeRecord) string {
	type promptItem struct {
		Fingerprint string `json:"fingerprint"`
		Project     string `json:"project"`
		Category    string `json:"category"`
		Source      string `json:"source"`
		Count       int    `json:"count"`
		Summary     string `json:"summary"`
		BadSnippet  string `json:"bad_snippet,omitempty"`
		GoodSnippet string `json:"good_snippet,omitempty"`
	}
	payload := make([]promptItem, 0, len(batch))
	for _, m := range batch {
		bad := m.BadSnippet
		if len(bad) > 600 {
			bad = bad[:600] + "..."
		}
		good := m.GoodSnippet
		if len(good) > 400 {
			good = good[:400] + "..."
		}
		payload = append(payload, promptItem{
			Fingerprint: m.Fingerprint,
			Project:     filepath.Base(m.ProjectPath),
			Category:    m.Category,
			Source:      m.Source,
			Count:       m.Count,
			Summary:     m.Summary,
			BadSnippet:  bad,
			GoodSnippet: good,
		})
	}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	return "你是代码质量护栏专家，请逐条评估以下 SlopGuard 捕获的代码错题，判断每条是否值得固化为长期拦截规则：\n" +
		"1) 若属于第三方缓存/构建产物误报（如 .cache/、.venv/、node_modules/）或极偶然单次拼写错误，decision 返回 \"skip\"；\n" +
		"2) 若属于容易反复出现的隐患（空值访问、类型逃逸、忽略注释、缺失导入、高频返工或重要规范违规），decision 返回 \"recommend\"；\n" +
		"3) reason 必须用简洁通俗的中文说清“哪里有问题、会带来什么后果、为什么建议入库或忽略”；\n" +
		"4) 若 decision 为 \"recommend\"，请在 rule_draft 给出精简的配置规则草案（如 ruff / pyright / eslint 配置片段或检查要点）；\n" +
		"5) 必须保留输入中的原始 fingerprint。\n\n待评估错题列表：\n" + string(raw)
}

func buildAgentCLIArgs(runner AgentCLIRunner, project, prompt, schemaFilePath string) ([]string, []byte, error) {
	if strings.TrimSpace(runner.ReadOnlyMode) == "" {
		return nil, nil, fmt.Errorf("%s 未声明最小只读权限模式，拒绝执行", runner.Name)
	}
	switch runner.ID {
	case "agy":
		return []string{
			"--output-format", "json",
			"--mode=plan",
			"--sandbox",
			"--disable-slash-commands",
			"--print-timeout=85s",
			"--json-schema", aiAnalysisJSONSchema,
			"-p=" + prompt,
		}, nil, nil
	case "claude":
		return []string{
			"-p", prompt,
			"--output-format", "json",
			"--permission-mode", "plan",
			"--json-schema", aiAnalysisJSONSchema,
		}, nil, nil
	case "codex":
		args := []string{
			"exec",
			"--sandbox", "read-only",
			"--json",
		}
		if schemaFilePath != "" {
			args = append(args, "--output-schema", schemaFilePath)
		}
		if project != "" {
			args = append(args, "-C", project)
		}
		args = append(args, "-")
		return args, []byte(prompt), nil
	default:
		return nil, nil, fmt.Errorf("不支持的模型 CLI 类型: %s", runner.ID)
	}
}

func invokeHostAgentCLI(ctx context.Context, runner AgentCLIRunner, project string, mistakes []store.MistakeRecord) ([]AIAnalysisItem, error) {
	if !runner.Available || strings.TrimSpace(runner.BinaryPath) == "" {
		return nil, errors.New("未找到可用的 Agent CLI")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
	}

	workDir := ""
	if project != "" {
		if info, err := os.Stat(project); err == nil && info.IsDir() {
			workDir = project
		}
	}

	var schemaFilePath string
	if runner.ID == "codex" {
		tmpFile, err := os.CreateTemp("", "slopguard-codex-schema-*.json")
		if err != nil {
			return nil, fmt.Errorf("创建 Codex schema 临时文件失败: %w", err)
		}
		schemaFilePath = tmpFile.Name()
		defer func() { _ = os.Remove(schemaFilePath) }()
		if _, err := tmpFile.WriteString(aiAnalysisJSONSchema); err != nil {
			_ = tmpFile.Close()
			return nil, fmt.Errorf("写入 Codex schema 失败: %w", err)
		}
		_ = tmpFile.Chmod(0o600)
		_ = tmpFile.Close()
	}

	prompt := buildMistakeAnalysisPrompt(mistakes)
	args, stdinBytes, err := buildAgentCLIArgs(runner, workDir, prompt, schemaFilePath)
	if err != nil {
		return nil, err
	}

	home, _ := os.UserHomeDir()
	res := engine.RunProcess(ctx, engine.ProcessSpec{
		Command:          runner.BinaryPath,
		Args:             args,
		Dir:              workDir,
		Env:              augmentedCLIEnv(home),
		Stdin:            stdinBytes,
		Timeout:          90 * time.Second,
		MaxOutputBytes:   engine.MaxOutputBytes,
		FailOnTruncation: true,
	})
	if res.Err != nil || res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" && res.Err != nil {
			msg = res.Err.Error()
		}
		if msg == "" {
			msg = fmt.Sprintf("exit code %d", res.ExitCode)
		}
		if len(msg) > 280 {
			msg = msg[:280] + "..."
		}
		return nil, fmt.Errorf("%s 调用失败: %s", runner.Name, msg)
	}

	if runner.ID == "codex" {
		items, parseErr := parseCodexJSONLOutput([]byte(res.Stdout))
		if parseErr != nil {
			return nil, fmt.Errorf("%s 返回解析失败: %w", runner.Name, parseErr)
		}
		return items, nil
	}

	items, parseErr := parseAgentAnalysisOutput([]byte(res.Stdout))
	if parseErr != nil {
		return nil, fmt.Errorf("%s 返回解析失败: %w", runner.Name, parseErr)
	}
	return items, nil
}

func parseCodexJSONLOutput(raw []byte) ([]AIAnalysisItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("Codex 返回空输出")
	}
	var lastAgentText string
	var lastErrMsg string
	for _, line := range bytes.Split(trimmed, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   *struct {
				Message string `json:"message"`
			} `json:"error"`
			Item *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "error", "turn.failed":
			if ev.Error != nil && strings.TrimSpace(ev.Error.Message) != "" {
				lastErrMsg = strings.TrimSpace(ev.Error.Message)
			} else if strings.TrimSpace(ev.Message) != "" {
				lastErrMsg = strings.TrimSpace(ev.Message)
			}
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" && strings.TrimSpace(ev.Item.Text) != "" {
				lastAgentText = ev.Item.Text
			}
		}
	}
	if lastAgentText != "" {
		if items := extractItemsFromText(lastAgentText); len(items) > 0 {
			return items, nil
		}
		return nil, errors.New("Codex agent_message 未包含有效的 items 结构化结果")
	}
	if lastErrMsg != "" {
		return nil, errors.New(lastErrMsg)
	}
	// Fallback if Codex returned a plain JSON object directly.
	return parseAgentAnalysisOutput(trimmed)
}

func parseAgentAnalysisOutput(raw []byte) ([]AIAnalysisItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("空输出")
	}

	var envelope struct {
		Status           string          `json:"status"`
		Error            string          `json:"error"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Response         string          `json:"response"`
		Result           string          `json:"result"`
		Items            []AIAnalysisItem `json:"items"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err == nil {
		if envelope.Error != "" {
			return nil, errors.New(envelope.Error)
		}
		if len(envelope.Items) > 0 {
			return envelope.Items, nil
		}
		if len(bytes.TrimSpace(envelope.StructuredOutput)) > 0 {
			var box struct {
				Items []AIAnalysisItem `json:"items"`
			}
			if err := json.Unmarshal(envelope.StructuredOutput, &box); err == nil && len(box.Items) > 0 {
				return box.Items, nil
			}
		}
		for _, textCandidate := range []string{envelope.Response, envelope.Result} {
			if items := extractItemsFromText(textCandidate); len(items) > 0 {
				return items, nil
			}
		}
	}

	if items := extractItemsFromText(string(trimmed)); len(items) > 0 {
		return items, nil
	}
	return nil, errors.New("未包含 items 结构化结果")
}

func extractItemsFromText(text string) []AIAnalysisItem {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	// Try direct unmarshal first, then extract first JSON object from markdown/prose.
	for _, candidate := range []string{text, extractFirstJSONObject(text)} {
		if candidate == "" {
			continue
		}
		var box struct {
			Items []AIAnalysisItem `json:"items"`
		}
		if err := json.Unmarshal([]byte(candidate), &box); err == nil && len(box.Items) > 0 {
			return box.Items
		}
		var arr []AIAnalysisItem
		if err := json.Unmarshal([]byte(candidate), &arr); err == nil && len(arr) > 0 {
			return arr
		}
	}
	return nil
}

func extractFirstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return ""
}
