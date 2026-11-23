// Package contract 定义 SlopGuard 各模块共享的核心接口与类型。
// 所有 Phase 1 子模块必须基于这些契约实现，严禁各自另立冲突定义。
package contract

import (
	"context"
	"io"
	"time"
)

// ---------------------------------------------------------------------------
// Runner：外部命令执行抽象（Mock 友好）
// ---------------------------------------------------------------------------

// RunSpec 描述一次外部命令调用。
type RunSpec struct {
	Name    string            // 可执行文件名，如 "npx"、"ruff"
	Args    []string          // 参数
	Dir     string            // 工作目录（空 = 当前）
	Env     map[string]string // 额外环境变量（可选）
	Timeout time.Duration     // 超时；0 = 使用调用方默认值
}

// RunResult 是一次外部命令的结构化结果。
type RunResult struct {
	Stdout   string        // 截断后的 stdout（≤2MB，首尾保留摘要）
	Stderr   string        // 截断后的 stderr
	ExitCode int           // 进程退出码；超时/信号终止时为 -1
	Duration time.Duration // 实际耗时
	Skipped  bool          // true = 工具缺失被优雅跳过
	Err      error         // 非 nil 表示调度/系统级错误（非业务失败）
}

// Runner 是外部命令的唯一执行入口。
// 实现必须：流式读取输出（禁 io.ReadAll 全量读）、独立进程组、
// 超时/中断连带清理子孙进程。
type Runner interface {
	Run(ctx context.Context, spec RunSpec) RunResult
	// LookPath 探测工具是否存在，用于优雅降级。
	LookPath(name string) (path string, ok bool)
}

// ---------------------------------------------------------------------------
// Adapter：技术栈适配器
// ---------------------------------------------------------------------------

// Check 是 Adapter 声明的一项确定性检查。
type Check struct {
	ID       string   // 唯一标识，如 "tsc"、"eslint"、"ruff"
	Cmd      []string // 完整命令（含参数），经 Runner 执行
	Optional bool     // true = 工具缺失时优雅跳过而非报错
	Timeout  time.Duration
}

// EscapePattern 描述一个"逃生舱"指标（如 @ts-ignore、eslint-disable）。
type EscapePattern struct {
	Name   string // 指标 key，如 "ts_ignore"、"eslint_disable"
	Regex  string // 用于在变更文件里计数的正则
	Target string // "code" | "comment" | "raw"（为空时由引擎自动推断）
}

// Adapter 适配一种技术栈：探测项目、提供检查与逃生舱规则。
type Adapter interface {
	Name() string
	// Detect 判断该 Adapter 是否适用于给定项目根目录。
	Detect(projectRoot string) bool
	// Extensions 返回该 Adapter 关心的文件扩展名（含点，如 ".ts"）。
	Extensions() []string
	// Checks 返回要对变更文件执行的确定性检查列表。
	Checks(projectRoot string, files []string) []Check
	// EscapePatterns 返回该栈的逃生舱正则列表。
	EscapePatterns() []EscapePattern
	// ParseOutput 将某次 Check 的原始输出解析为结构化 Findings。
	// checkID 对应 Check.ID，output 为 Runner 截断后的 Stdout/Stderr 合并。
	ParseOutput(checkID string, output string) []Finding
}

// ---------------------------------------------------------------------------
// VerifyResult / Finding：检查输出统一模型
// ---------------------------------------------------------------------------

// Severity 表示问题严重级别。
type Severity string

const (
	SeverityError Severity = "error"
	SeverityWarn  Severity = "warn"
)

// Finding 是一条具体的检查问题。
type Finding struct {
	File       string   `json:"file"`
	Line       int      `json:"line,omitempty"`
	Column     int      `json:"column,omitempty"`
	Rule       string   `json:"rule"` // 来源检查，如 "tsc"、"eslint:no-restricted-syntax"
	Message    string   `json:"message"`
	Severity   Severity `json:"severity"`
	ActionHint string   `json:"action_hint,omitempty"`
}

// SkippedDetail 描述一项因工具缺失等原因被跳过的检查。
type SkippedDetail struct {
	ID     string `json:"id"`
	Tool   string `json:"tool,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// UISmokeResult 是一次 Playwright UI 烟雾验证的结果。
type UISmokeResult struct {
	// Skipped 表示项目未配置 UI 或 Playwright 未安装，验证被优雅跳过。
	Skipped bool `json:"skipped"`
	// Reason 是跳过原因或失败摘要。
	Reason string `json:"reason,omitempty"`
	// Passed 表示烟雾验证是否全部通过；Skipped 时无意义。
	Passed bool `json:"passed"`
	// Findings 是 console.error、pageerror、5xx 等结构化问题。
	Findings []Finding `json:"findings,omitempty"`
	// Screenshots 是失败截图路径（成功时可为空）。
	Screenshots []string `json:"screenshots,omitempty"`
	// DurationMs 是本次 UI 烟雾验证耗时（毫秒）。
	DurationMs int64 `json:"duration_ms"`
}

// MetricDelta 描述单个逃生舱指标的基线/当前值差异。
type MetricDelta struct {
	Name     string `json:"name"`
	Baseline int    `json:"baseline"`
	Current  int    `json:"current"`
	Delta    int    `json:"delta"`
}

// AblationResult 记录 --ablation 自动消融验证的结果（RED -> GREEN）。
type AblationResult struct {
	Status          string   `json:"status"` // "passed" | "failed" | "skipped"
	Reason          string   `json:"reason,omitempty"`
	TestFiles       []string `json:"test_files,omitempty"`
	ProdFiles       []string `json:"prod_files,omitempty"`
	RedDurationMs   int64    `json:"red_duration_ms,omitempty"`
	GreenDurationMs int64    `json:"green_duration_ms,omitempty"`
}

// VerifyResult 是一次 verify 运行的聚合结果。
type VerifyResult struct {
	Passed   bool      `json:"passed"`
	Findings []Finding `json:"findings"`
	// Escapes 是各逃生舱指标的当前总数（key = EscapePattern.Name）。
	Escapes map[string]int `json:"escapes"`
	// Baseline 是基线里对应指标的上限（无基线项省略）。
	Baseline map[string]int `json:"baseline,omitempty"`
	// Improvements 记录低于基线或净减少的逃生舱指标，提示可收紧基线。
	Improvements []MetricDelta `json:"improvements,omitempty"`
	// Hint 是非阻断性的后续建议（例如提示运行 slopguard baseline --tighten）。
	Hint string `json:"hint,omitempty"`
	// Suppressed 是被 baseline.suppressed_files 过滤掉的文件数。
	Suppressed int `json:"suppressed,omitempty"`
	// DurationMs 是本次 verify 的实际耗时（毫秒）。
	DurationMs int64 `json:"duration_ms"`
	// Skipped 保留为检查 ID 数组，兼容既有消费者。
	Skipped []string `json:"skipped,omitempty"`
	// SkippedDetails 提供跳过检查的工具与原因（Phase 1 新增）。
	SkippedDetails []SkippedDetail `json:"skipped_details,omitempty"`
	// Warnings 记录非阻断提示（如缺少 baseline.json、持久化告警等）。
	Warnings []string `json:"warnings,omitempty"`
	// Ablation 是启用 --ablation 时的自动消融验证结果；未启用时省略。
	Ablation *AblationResult `json:"ablation,omitempty"`
	// UI 是启用 --ui 时的 Playwright 烟雾验证结果；未启用时省略。
	UI *UISmokeResult `json:"ui,omitempty"`
}

// ---------------------------------------------------------------------------
// Store：持久化抽象（Phase 2 由 SQLite 实现，此处先锁契约）
// ---------------------------------------------------------------------------

// Mistake 是一条错题记录。
type Mistake struct {
	ID          string    `json:"id"`
	ProjectPath string    `json:"project_path"`
	Fingerprint string    `json:"fingerprint"`
	Category    string    `json:"category"` // import | syntax | type | architecture | ui
	Summary     string    `json:"summary"`
	BadSnippet  string    `json:"bad_snippet,omitempty"`
	GoodSnippet string    `json:"good_snippet,omitempty"`
	Source      string    `json:"source"` // manual | agent | git_fix
	Status      string    `json:"status"` // pending | promoted | ignored
	CreatedAt   time.Time `json:"created_at"`
}

// Store 是全局持久化接口（Phase 2 用 modernc.org/sqlite 实现）。
type Store interface {
	io.Closer
	RecordMistake(ctx context.Context, m Mistake) (count int, err error)
	ListCandidates(ctx context.Context, minCount int) ([]Mistake, error)
	// RecordScan 记录一次扫描运行（用于趋势）。
	RecordScan(ctx context.Context, projectPath string, durationMs int64, escapes int, passed bool) error
}
