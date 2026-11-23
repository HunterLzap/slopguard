package cli

import (
	"context"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// checkTask couples one adapter check with the adapter that owns it so output
// can be parsed by the right implementation after concurrent execution.
type checkTask struct {
	AdapterName string
	Check       contract.Check
}

// checkOutcome is the result of executing one check task.
type checkOutcome struct {
	Task   checkTask
	Result contract.RunResult
}

// baselineState is the project baseline as seen by the CLI: metric upper
// bounds plus a predicate for suppressed files.
type baselineState struct {
	Metrics         map[string]int
	IsSuppressed    func(file string) bool
	HasSuppressions bool
}

// Suppresses reports whether file is excluded by baseline.suppressed_files.
func (b baselineState) Suppresses(file string) bool {
	return b.IsSuppressed != nil && b.IsSuppressed(file)
}

// engineClient is the narrow port the CLI needs from the core engine. Keeping
// this interface here lets the CLI layer stay stable while the engine package
// fixes its exported API, and keeps command code free of engine internals.
type engineClient interface {
	// ChangedFiles returns files changed in the working tree (including
	// untracked files) relative to root.
	ChangedFiles(ctx context.Context, root string) ([]string, error)
	// ListFiles returns all files under root matching the given extensions.
	ListFiles(root string, extensions []string) ([]string, error)
	// RunChecks executes check tasks with a bounded worker pool.
	RunChecks(ctx context.Context, root string, jobs int, tasks []checkTask) ([]checkOutcome, error)
	// LoadBaseline reads .slopguard/baseline.json metric upper bounds.
	LoadBaseline(path string) (map[string]int, error)
	// SaveBaseline writes a new baseline document with the given metrics.
	SaveBaseline(path string, metrics map[string]int) error
	// ComputeEscapes counts escape-hatch matches across files.
	ComputeEscapes(root string, files []string, patterns []contract.EscapePattern) (map[string]int, error)
	// Ratchet compares current escape counts with the baseline and returns
	// findings for any metric that increased.
	Ratchet(escapes, baseline map[string]int) []contract.Finding
}

// baselineProvider is an optional extension implemented by the production
// engine to expose suppressed_files alongside baseline metrics. Keeping it
// optional preserves compatibility with simpler test doubles.
type baselineProvider interface {
	LoadBaselineState(path string) (baselineState, error)
}

// lastCommitProvider is an optional extension for resolving files touched by
// HEAD (used by post-commit hooks and background auto-guard).
type lastCommitProvider interface {
	LastCommitFiles(ctx context.Context, root string) ([]string, error)
}

// commitFilesProvider resolves files touched by an explicit commit SHA.
type commitFilesProvider interface {
	CommitFiles(ctx context.Context, root, rev string) ([]string, error)
}

// signalContextProvider is an optional engine capability for graceful
// Ctrl+C/SIGTERM cancellation.
type signalContextProvider interface {
	SignalContext(parent context.Context) (context.Context, context.CancelFunc)
}

// checkPlan is the result of probing tool availability before execution.
type checkPlan struct {
	Runnable       []contract.Check
	Skipped        []string
	SkippedDetails []contract.SkippedDetail
}

// adapterProvider is the narrow port the CLI needs from the adapter registry.
type adapterProvider interface {
	// Detect returns adapters applicable to the given project root.
	Detect(root string) ([]contract.Adapter, error)
	// PlanChecks resolves local tools and splits checks into runnable and
	// gracefully skipped ones. A missing required tool is an error.
	PlanChecks(root string, checks []contract.Check) (checkPlan, error)
	// ParseFindings converts one check's raw output into findings, using the
	// owning adapter's parser and a generic fallback.
	ParseFindings(a contract.Adapter, checkID string, result contract.RunResult) []contract.Finding
}

// radarHotspot is the CLI-facing view of one radar hotspot.
type radarHotspot struct {
	File       string `json:"file"`
	RiskScore  int    `json:"risk_score"`
	References int    `json:"references"`
	Complexity int    `json:"complexity"`
	Escapes    int    `json:"escapes"`
	Rework     int    `json:"rework"`
	DeadCode   bool   `json:"dead_code"`
	Duplicate  bool   `json:"duplicate"`
}

type radarSkipped struct {
	Dimension string `json:"dimension"`
	Tool      string `json:"tool,omitempty"`
	Reason    string `json:"reason"`
}

type radarResult struct {
	Hotspots []radarHotspot `json:"hotspots"`
	Skipped  []radarSkipped `json:"skipped,omitempty"`
}

// radarProvider is the narrow port the CLI needs from the radar package.
type radarProvider interface {
	Scan(ctx context.Context, root string, top int) (radarResult, error)
}

// scanRecorder records a completed verify run into the global store.
type scanRecorder interface {
	RecordScan(ctx context.Context, projectPath string, durationMs int64, escapes int, passed bool) error
}

// presetMergeItem is the CLI-facing view of one global rule preset merge.
type presetMergeItem struct {
	Engine     string
	RuleID     string
	ConfigPath string
	BackupPath string
	Diff       string
	Reason     string
}

// presetMergePlan is a preview of a global preset sync; Raw keeps the
// provider-specific plan that ApplyPresets consumes.
type presetMergePlan struct {
	Written []presetMergeItem
	Skipped []presetMergeItem
	Diffs   []string
	Raw     any
}

// presetMergeResult summarizes a global preset sync.
type presetMergeResult struct {
	Written []presetMergeItem
	Skipped []presetMergeItem
	Backups []string
	Diffs   []string
}

// presetProvider previews and applies ~/.slopguard/presets merges.
type presetProvider interface {
	PlanPresets(globalDir, projectDir string) (presetMergePlan, error)
	ApplyPresets(plan presetMergePlan) (presetMergeResult, error)
}

// uiOptions is the CLI-facing input for a Playwright UI smoke run.
type uiOptions struct {
	ProjectRoot string
}

// uiProvider runs the optional Playwright UI smoke validation.
type uiProvider interface {
	RunUISmoke(ctx context.Context, opts uiOptions) (contract.UISmokeResult, error)
}

// mistakeRecorder is an optional extension of scanRecorder for auto-capturing
// recurring verify failures into the mistakes inbox.
type mistakeRecorder interface {
	RecordMistake(ctx context.Context, m contract.Mistake) (int, error)
}

type dependencies struct {
	isProduction bool
	engine       engineClient
	adapters     adapterProvider
	radar        radarProvider
	recorder     scanRecorder
	presets      presetProvider
	ui           uiProvider
	runner       contract.Runner
}

// newDependencies is replaced in wire.go with the production wiring. Tests can
// swap it for fakes without touching command code.
var newDependencies = defaultDependencies
