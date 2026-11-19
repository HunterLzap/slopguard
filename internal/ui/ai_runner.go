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

	"github.com/HunterLzap/slopguard/internal/store"
)

const aiAnalysisJSONSchema = `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"fingerprint":{"type":"string"},"decision":{"type":"string","enum":["recommend","skip"]},"reason":{"type":"string"},"rule_draft":{"type":"string"}},"required":["fingerprint","decision","reason"]}}},"required":["items"]}`

// AgentCLIRunner describes an installed host Agent CLI usable for headless AI analysis.
type AgentCLIRunner struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	BinaryPath string `json:"binary_path"`
	Available  bool   `json:"available"`
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
	status     AIRuntimeStatus
	analyzeFn  aiAnalyzeFunc
}

func newAIManager(server *Server) *AIManager {
	return &AIManager{
		server:    server,
		analyzeFn: invokeHostAgentCLI,
	}
}

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
	} else {
		extra = append(extra, "/usr/local/bin", "/usr/bin")
	}

	env := os.Environ()
	pathUpdated := false
	prefix := strings.Join(extra, string(os.PathListSeparator))
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			curr := strings.TrimPrefix(kv, "PATH=")
			if curr != "" {
				env[i] = "PATH=" + prefix + string(os.PathListSeparator) + curr
			} else {
				env[i] = "PATH=" + prefix
			}
			pathUpdated = true
			break
		}
	}
	if !pathUpdated {
		env = append(env, "PATH="+prefix)
	}
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
		id   string
		name string
		bin  string
	}{
		{id: "agy", name: "Antigravity CLI (agy)", bin: "agy"},
		{id: "claude", name: "Claude Code CLI (claude)", bin: "claude"},
		{id: "codex", name: "Codex CLI (codex)", bin: "codex"},
	}
	out := make([]AgentCLIRunner, 0, len(specs))
	for _, spec := range specs {
		p := lookupAgentCLIBinary(home, spec.bin)
		out = append(out, AgentCLIRunner{
			ID:         spec.id,
			Name:       spec.name,
			BinaryPath: p,
			Available:  p != "",
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
	// Default AutoAnalyze to true when a host CLI runner is present so new mistakes
	// captured by Guard / Scan get analyzed automatically without manual clicks.
	runners := discoverAgentCLIRunners(home)
	hasRunner := false
	for _, r := range runners {
		if r.Available {
			hasRunner = true
			break
		}
	}
	m.cfg = AIConfig{AutoAnalyze: hasRunner}

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
	encoded, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(path, encoded, 0o644)
}

func (m *AIManager) selectRunner(home, preferredID string) (AgentCLIRunner, bool) {
	runners := discoverAgentCLIRunners(home)
	if preferredID != "" {
		for _, r := range runners {
			if r.ID == preferredID && r.Available {
				return r, true
			}
		}
	}
	for _, r := range runners {
		if r.Available {
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
	m.mu.Lock()
	m.ensureLoadedLocked()
	if autoAnalyze != nil {
		m.cfg.AutoAnalyze = *autoAnalyze
	}
	if runnerID != nil {
		m.cfg.RunnerID = strings.TrimSpace(*runnerID)
	}
	m.cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return m.Status(), err
	}
	return m.Status(), nil
}

// TriggerAsync schedules non-blocking real Agent CLI analysis when AutoAnalyze is enabled.
func (m *AIManager) TriggerAsync(project string) {
	if m == nil || m.server == nil || m.server.db == nil {
		return
	}
	st := m.Status()
	if !st.AutoAnalyze || st.Running {
		return
	}
	home := m.server.resolveHomeDir()
	if _, ok := m.selectRunner(home, st.RunnerID); !ok {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 95*time.Second)
		defer cancel()
		_, _, _, _ = m.AnalyzePending(ctx, project)
	}()
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
	preferred := m.cfg.RunnerID
	analyzeFn := m.analyzeFn
	runner, ok := m.selectRunner(home, preferred)
	if !ok && analyzeFn == nil {
		m.mu.Unlock()
		return 0, 0, 0, errors.New("未探测到可用的本机 Agent CLI（如 agy / claude），请先安装并登录对应客户端，或在编辑器内通过 MCP 触发分析")
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

	if !ok && analyzeFn != nil {
		// Allows unit tests with injected analyzeFn when no real CLI exists in test HOME.
		runner = AgentCLIRunner{ID: "mock", Name: "Mock Agent", Available: true}
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

	for _, item := range items {
		fp := strings.TrimSpace(item.Fingerprint)
		if fp == "" {
			continue
		}
		if _, exists := validFPs[fp]; !exists {
			continue
		}
		decision := strings.ToLower(strings.TrimSpace(item.Decision))
		if decision != "recommend" && decision != "skip" {
			decision = "recommend"
		}
		reason := strings.TrimSpace(item.Reason)
		if reason == "" {
			continue
		}
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
		err = errors.New("Agent 模型未返回可匹配当前错题的分析结果")
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

func invokeHostAgentCLI(ctx context.Context, runner AgentCLIRunner, project string, mistakes []store.MistakeRecord) ([]AIAnalysisItem, error) {
	if !runner.Available || strings.TrimSpace(runner.BinaryPath) == "" {
		return nil, errors.New("未找到可用的 Agent CLI")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
	}

	prompt := buildMistakeAnalysisPrompt(mistakes)
	var cmd *exec.Cmd
	switch runner.ID {
	case "agy":
		cmd = exec.CommandContext(ctx, runner.BinaryPath,
			"--output-format", "json",
			"--mode=plan",
			"--disable-slash-commands",
			"--print-timeout=85s",
			"--json-schema", aiAnalysisJSONSchema,
			"-p="+prompt,
		)
	case "claude":
		cmd = exec.CommandContext(ctx, runner.BinaryPath,
			"-p", prompt,
			"--output-format", "json",
			"--permission-mode", "plan",
			"--json-schema", aiAnalysisJSONSchema,
		)
	case "codex":
		cmd = exec.CommandContext(ctx, runner.BinaryPath,
			"exec", prompt,
			"--output-format", "json",
		)
	default:
		return nil, fmt.Errorf("不支持的模型 CLI 类型: %s", runner.ID)
	}

	if project != "" {
		if info, err := os.Stat(project); err == nil && info.IsDir() {
			cmd.Dir = project
		}
	}
	home, _ := os.UserHomeDir()
	cmd.Env = augmentedCLIEnv(home)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 280 {
			msg = msg[:280] + "..."
		}
		return nil, fmt.Errorf("%s 调用失败: %s", runner.Name, msg)
	}

	items, parseErr := parseAgentAnalysisOutput(stdout.Bytes())
	if parseErr != nil {
		return nil, fmt.Errorf("%s 返回解析失败: %w", runner.Name, parseErr)
	}
	return items, nil
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
