package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
)

const maxAutoCapturedMistakesPerRun = 5

// BaselineState 描述 verify 读取到的基线指标与忽略文件规则。
type BaselineState struct {
	Metrics         map[string]int
	SuppressedFiles []string
	IsSuppressed    func(file string) bool
	HasSuppressions bool
}

// Suppresses 判断指定文件是否命中 baseline.suppressed_files。
func (b BaselineState) Suppresses(file string) bool {
	return b.IsSuppressed != nil && b.IsSuppressed(file)
}

// CheckTask 将适配器名称与具体检查绑定，便于运行后按原适配器解析输出。
type CheckTask struct {
	AdapterName string
	Check       contract.Check
}

// CheckOutcome 是单次检查执行的结果。
type CheckOutcome struct {
	Task   CheckTask
	Result contract.RunResult
}

// CheckPlan 是探测本地工具后的检查计划。
type CheckPlan struct {
	Runnable       []contract.Check
	Skipped        []string
	SkippedDetails []contract.SkippedDetail
}

// Recorder 封装可选的扫描日志与错题自动捕获持久化能力。
type Recorder interface {
	RecordScan(ctx context.Context, projectPath string, durationMs int64, escapes int, passed bool) error
	RecordMistake(ctx context.Context, m contract.Mistake) (int, error)
}

// Hooks 允许 CLI 等调用方在测试中注入替身实现；为 nil 时全部使用生产默认引擎与适配器。
type Hooks struct {
	SignalContext   func(parent context.Context) (context.Context, context.CancelFunc)
	DetectAdapters  func(root string) ([]contract.Adapter, error)
	LoadBaseline    func(path string) (BaselineState, error)
	ListFiles       func(root string, extensions []string) ([]string, error)
	ChangedFiles    func(ctx context.Context, root string) ([]string, error)
	StagedFiles     func(ctx context.Context, root string) ([]string, error)
	LastCommitFiles func(ctx context.Context, root string) ([]string, error)
	CommitFiles     func(ctx context.Context, root, rev string) ([]string, error)
	PlanChecks      func(root string, checks []contract.Check, strictTools bool, requiredTools []string) (CheckPlan, error)
	RunChecks       func(ctx context.Context, root string, jobs int, tasks []CheckTask) ([]CheckOutcome, error)
	ParseFindings   func(a contract.Adapter, checkID string, result contract.RunResult) []contract.Finding
	ComputeEscapes  func(root string, files []string, patterns []contract.EscapePattern) (map[string]int, error)
	Ratchet         func(escapes, baseline map[string]int) ([]contract.Finding, []contract.MetricDelta)
	RunUISmoke      func(ctx context.Context, root string) (contract.UISmokeResult, error)
}

// Options 控制一次统一 verify 编排的输入参数。
type Options struct {
	ProjectRoot           string
	All                   bool
	Staged                bool
	LastCommit            bool
	CommitSHA             string
	Paths                 []string
	Jobs                  int
	StrictBaseline        bool
	StrictTools           bool
	Ablation              bool
	FallbackAllWhenNotGit bool
	UI                    bool
	Record                bool
	AutoCapture           bool
	CaptureSource         string

	Runner   contract.Runner
	Recorder Recorder
	Hooks    *Hooks
}

type annotatedFinding struct {
	finding contract.Finding
	adapter string
}

// Run 执行统一的 SlopGuard verify 编排流程：
// 配置加载 → 适配器探测 → 基线解析 → 变更作用域解析 → 工具探测与并发执行 →
// 逃生舱棘轮 → 可选 UI 烟雾验证 → 可选扫描记录与错题聚类捕获。
func Run(ctx context.Context, opts Options) (contract.VerifyResult, error) {
	started := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}

	root, err := resolveProjectRoot(opts.ProjectRoot)
	if err != nil {
		return contract.VerifyResult{}, err
	}

	runner := opts.Runner
	if runner == nil {
		runner = engine.NewExecRunner()
	}

	var cancel context.CancelFunc
	if opts.Hooks != nil && opts.Hooks.SignalContext != nil {
		ctx, cancel = opts.Hooks.SignalContext(ctx)
	} else {
		ctx, cancel = signalContext(ctx)
	}
	defer cancel()

	cfg, cfgErr := LoadProjectConfig(root)
	if cfgErr != nil && !errors.Is(cfgErr, os.ErrNotExist) {
		return contract.VerifyResult{}, cfgErr
	}

	if opts.Ablation {
		if opts.All || opts.LastCommit || strings.TrimSpace(opts.CommitSHA) != "" {
			return contract.VerifyResult{}, errors.New("--ablation cannot be combined with --all, --last-commit, or --commit")
		}
		if strings.TrimSpace(cfg.Verify.TestCmd) == "" {
			return contract.VerifyResult{}, fmt.Errorf("verify.test_cmd is not configured; set verify.test_cmd in %s to run --ablation", ConfigRelPath)
		}
	}

	if opts.Jobs < 0 {
		return contract.VerifyResult{}, errors.New("--jobs must be >= 1")
	}
	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = cfg.Verify.Jobs
	}
	if jobs <= 0 {
		jobs = 2
	}

	detected, err := detectAdapters(opts.Hooks, root)
	if err != nil {
		return contract.VerifyResult{}, fmt.Errorf("detect adapters: %w", err)
	}
	adapters := filterAdaptersByName(detected, cfg.Adapters)

	var (
		annotated             []annotatedFinding
		escapes               = map[string]int{}
		baselineMetrics       map[string]int
		improvements          []contract.MetricDelta
		warnings              []string
		skippedIDs            = make([]string, 0)
		skippedDetails        = make([]contract.SkippedDetail, 0)
		suppressedFromChecks  []string
		suppressedFromEscapes []string
	)

	if len(adapters) == 0 {
		if len(cfg.Adapters) > 0 {
			return contract.VerifyResult{}, fmt.Errorf("none of the configured adapters %v apply to this project; run slopguard init", cfg.Adapters)
		}
		if !opts.All && len(opts.Paths) == 0 && opts.Hooks == nil && cfg.Probes.IsEnabled() {
			git := engine.NewGit(runner, root)
			if git.IsRepository(ctx) {
				if diffFiles, loader, diffErr := collectDiffFiles(ctx, git, root, opts); diffErr == nil {
					sev, _ := cfg.Probes.EffectiveSeverity()
					for _, pf := range engine.ScanDiffProbes(diffFiles, sev, nil, loader) {
						annotated = append(annotated, annotatedFinding{finding: pf, adapter: "slopguard"})
					}
				}
			}
		}
	} else {
		patterns := collectEscapePatterns(adapters)

		baseline, bErr := loadBaselineState(opts.Hooks, BaselinePath(root))
		if bErr != nil {
			if errors.Is(bErr, os.ErrNotExist) {
				if opts.StrictBaseline && len(patterns) > 0 {
					return contract.VerifyResult{}, fmt.Errorf("%s not found; run slopguard init first", BaselineRelPath)
				}
				baseline = BaselineState{Metrics: map[string]int{}}
				if len(patterns) > 0 {
					warnings = append(warnings, fmt.Sprintf("%s not found; using empty baseline (run slopguard init to create one)", BaselineRelPath))
				}
			} else {
				return contract.VerifyResult{}, fmt.Errorf("load baseline: %w", bErr)
			}
		}
		baselineMetrics = baseline.Metrics

		extensions := collectExtensions(adapters)
		var files []string
		var usedGitScope bool
		files, usedGitScope, err = resolveScopeFiles(ctx, opts, runner, root, extensions)
		if err != nil {
			return contract.VerifyResult{}, fmt.Errorf("resolve scan scope: %w", err)
		}
		files, suppressedFromChecks = filterSuppressedFiles(files, baseline)

		strictTools := opts.StrictTools || cfg.Verify.StrictTools
		var tasks []CheckTask

		for _, adp := range adapters {
			subset := filterFilesByExtensions(files, adp.Extensions())
			if len(subset) == 0 {
				continue
			}
			plan, planErr := planAdapterChecks(opts.Hooks, runner, root, adp.Checks(root, subset), strictTools, cfg.Verify.RequiredTools)
			if planErr != nil {
				return contract.VerifyResult{}, fmt.Errorf("check toolchain: %w", planErr)
			}
			skippedIDs = append(skippedIDs, plan.Skipped...)
			if len(plan.SkippedDetails) > 0 {
				skippedDetails = append(skippedDetails, plan.SkippedDetails...)
			} else {
				for _, id := range plan.Skipped {
					skippedDetails = append(skippedDetails, contract.SkippedDetail{ID: id})
				}
			}
			for _, check := range plan.Runnable {
				if len(check.Cmd) == 0 {
					continue
				}
				tasks = append(tasks, CheckTask{AdapterName: adp.Name(), Check: check})
			}
		}

		var outcomes []CheckOutcome
		if len(tasks) > 0 {
			outcomes, err = executeChecks(ctx, opts.Hooks, runner, root, jobs, tasks)
			if err != nil {
				return contract.VerifyResult{}, fmt.Errorf("run checks: %w", err)
			}
		}

		for _, outcome := range outcomes {
			res := outcome.Result
			if res.Err != nil {
				return contract.VerifyResult{}, fmt.Errorf("check %s: %w", outcome.Task.Check.ID, res.Err)
			}
			if res.Skipped {
				detail := contract.SkippedDetail{
					ID:     outcome.Task.Check.ID,
					Reason: "tool missing at execution time",
				}
				if len(outcome.Task.Check.Cmd) > 0 {
					detail.Tool = outcome.Task.Check.Cmd[0]
				}
				skippedIDs = append(skippedIDs, detail.ID)
				skippedDetails = append(skippedDetails, detail)
				continue
			}
			if res.ExitCode == 0 {
				continue
			}

			var parsed []contract.Finding
			if adp := findAdapter(adapters, outcome.Task.AdapterName); adp != nil {
				parsed = parseAdapterFindings(opts.Hooks, adp, outcome.Task.Check.ID, res)
			}
			if len(parsed) == 0 {
				msg := firstNonEmptyLine(res.Stderr, res.Stdout)
				if msg == "" {
					msg = fmt.Sprintf("check failed with exit code %d", res.ExitCode)
				}
				parsed = []contract.Finding{{
					Rule:     outcome.Task.Check.ID,
					Message:  msg,
					Severity: contract.SeverityError,
				}}
			}
			for _, f := range parsed {
				annotated = append(annotated, annotatedFinding{
					finding: f,
					adapter: outcome.Task.AdapterName,
				})
			}
		}

		useDiffMode := usedGitScope && !opts.All && len(opts.Paths) == 0 && (opts.Hooks == nil || opts.Hooks.ComputeEscapes == nil)
		if useDiffMode {
			git := engine.NewGit(runner, root)
			diffFiles, loader, diffErr := collectDiffFiles(ctx, git, root, opts)
			if diffErr != nil {
				return contract.VerifyResult{}, fmt.Errorf("collect git diff: %w", diffErr)
			}
			baselineObj := baselineStateToEngine(baseline)
			for _, df := range diffFiles {
				if baselineObj.IsSuppressed(df.Path) {
					suppressedFromEscapes = append(suppressedFromEscapes, df.Path)
				}
			}
			if len(patterns) > 0 {
				rules := buildAdapterEscapeRules(adapters)
				diffRes, rErr := engine.EvaluateDiffRatchet(diffFiles, rules, baselineObj, loader)
				if rErr != nil {
					return contract.VerifyResult{}, fmt.Errorf("evaluate diff escapes: %w", rErr)
				}
				escapes = diffRes.NetEscapes
				improvements = diffRes.Improvements
				for _, rf := range diffRes.Findings {
					annotated = append(annotated, annotatedFinding{finding: rf, adapter: "slopguard"})
				}
			}
			if cfg.Probes.IsEnabled() {
				sev, _ := cfg.Probes.EffectiveSeverity()
				for _, pf := range engine.ScanDiffProbes(diffFiles, sev, baselineObj, loader) {
					annotated = append(annotated, annotatedFinding{finding: pf, adapter: "slopguard"})
				}
			}
		} else {
			escapeFiles := files
			if !opts.All && len(opts.Paths) == 0 && (len(patterns) > 0 || baseline.HasSuppressions) {
				escapeFiles, err = listRepoFiles(opts.Hooks, root, extensions)
				if err != nil {
					return contract.VerifyResult{}, fmt.Errorf("list files for escape counting: %w", err)
				}
			}
			escapeFiles, suppressedFromEscapes = filterSuppressedFiles(escapeFiles, baseline)
			if len(patterns) > 0 && len(escapeFiles) > 0 {
				escapes, err = computeEscapes(opts.Hooks, root, escapeFiles, patterns)
				if err != nil {
					return contract.VerifyResult{}, fmt.Errorf("count escape hatches: %w", err)
				}
			}
			if len(patterns) > 0 {
				ratchetFindings, ratchetImprovements := evaluateRatchet(opts.Hooks, escapes, baseline.Metrics)
				for _, rf := range ratchetFindings {
					annotated = append(annotated, annotatedFinding{
						finding: rf,
						adapter: "slopguard",
					})
				}
				improvements = ratchetImprovements
			}
		}
	}

	var ablationRes *contract.AblationResult
	if opts.Ablation {
		abOut, abErr := engine.RunAblation(ctx, runner, engine.AblationOptions{
			ProjectRoot:  root,
			TestCmd:      cfg.Verify.TestCmd,
			TestPatterns: cfg.Verify.TestPatterns,
			Staged:       opts.Staged,
		})
		if abErr != nil {
			return contract.VerifyResult{}, abErr
		}
		ablationRes = &abOut.Result
		warnings = append(warnings, abOut.Warnings...)
		for _, af := range abOut.Findings {
			annotated = append(annotated, annotatedFinding{finding: af, adapter: "slopguard"})
		}
	}

	findings := make([]contract.Finding, len(annotated))
	for i, item := range annotated {
		findings[i] = item.finding
	}

	var hint string
	if len(improvements) > 0 {
		hint = "escape hatches decreased; run 'slopguard baseline --tighten' to tighten baseline"
	}

	result := contract.VerifyResult{
		Passed:         !hasBlockingFinding(findings),
		Findings:       findings,
		Escapes:        escapes,
		Baseline:       baselineMetrics,
		Improvements:   improvements,
		Hint:           hint,
		Suppressed:     countUniqueFiles(suppressedFromChecks, suppressedFromEscapes),
		DurationMs:     time.Since(started).Milliseconds(),
		Skipped:        skippedIDs,
		SkippedDetails: skippedDetails,
		Warnings:       warnings,
		Ablation:       ablationRes,
	}

	if opts.UI && result.Passed {
		uiRes, uiErr := runUISmoke(ctx, opts.Hooks, runner, root)
		if uiErr != nil {
			return contract.VerifyResult{}, fmt.Errorf("run ui smoke: %w", uiErr)
		}
		result.UI = &uiRes
		if !uiRes.Skipped && !uiRes.Passed {
			result.Passed = false
		}
		result.DurationMs = time.Since(started).Milliseconds()
	}

	finalizePersistence(ctx, opts, root, annotated, &result)
	return result, nil
}

func finalizePersistence(ctx context.Context, opts Options, root string, annotated []annotatedFinding, result *contract.VerifyResult) {
	if result == nil || opts.Recorder == nil {
		return
	}
	if opts.Record {
		if err := opts.Recorder.RecordScan(ctx, root, result.DurationMs, sumEscapes(result.Escapes), result.Passed); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("record scan failed: %v", err))
		}
	}
	if (opts.Record || opts.AutoCapture) && !result.Passed && len(result.Findings) > 0 {
		autoCaptureMistakes(ctx, opts.Recorder, root, annotated, result.Findings, opts.CaptureSource, result)
	}
}

func autoCaptureMistakes(ctx context.Context, recorder Recorder, root string, annotated []annotatedFinding, findings []contract.Finding, source string, result *contract.VerifyResult) {
	if recorder == nil {
		return
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "auto_verify"
	}
	items := annotated
	if len(items) == 0 && len(findings) > 0 {
		items = make([]annotatedFinding, len(findings))
		for i, f := range findings {
			items[i] = annotatedFinding{finding: f}
		}
	}

	seen := make(map[string]struct{}, len(items))
	captured := 0
	for _, item := range items {
		if captured >= maxAutoCapturedMistakesPerRun {
			break
		}
		f := item.finding
		category := classifyFindingCategory(f)
		summary := buildFindingSummary(f)
		if summary == "" {
			continue
		}
		fp := FindingFingerprint(root, f, item.adapter)
		if _, dup := seen[fp]; dup {
			continue
		}
		seen[fp] = struct{}{}
		if _, err := recorder.RecordMistake(ctx, contract.Mistake{
			ProjectPath: root,
			Fingerprint: fp,
			Category:    category,
			Summary:     summary,
			BadSnippet:  formatFindingSnippet(f),
			Source:      source,
			Status:      "pending",
		}); err != nil {
			if result != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("auto-capture mistake failed: %v", err))
			}
			return
		}
		captured++
	}
}

func hasBlockingFinding(findings []contract.Finding) bool {
	for _, f := range findings {
		if strings.HasPrefix(f.Rule, "probe:") && f.Severity == contract.SeverityWarn {
			continue
		}
		return true
	}
	return false
}

func resolveProjectRoot(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "."
	}
	root, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("project root %s: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project root %s: not a directory", root)
	}
	return root, nil
}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if parent == context.Background() || parent == context.TODO() {
		return engine.SignalContext(parent)
	}
	return signal.NotifyContext(parent, os.Interrupt)
}

func detectAdapters(hooks *Hooks, root string) ([]contract.Adapter, error) {
	if hooks != nil && hooks.DetectAdapters != nil {
		return hooks.DetectAdapters(root)
	}
	registry, err := adapter.LoadMergedRegistry(root)
	if err != nil {
		return nil, err
	}
	return registry.Detect(root), nil
}

func loadBaselineState(hooks *Hooks, path string) (BaselineState, error) {
	if hooks != nil && hooks.LoadBaseline != nil {
		return hooks.LoadBaseline(path)
	}
	b, err := engine.LoadBaseline(path)
	if err != nil {
		return BaselineState{}, err
	}
	return BaselineState{
		Metrics:         b.Metrics,
		SuppressedFiles: b.SuppressedFiles,
		IsSuppressed:    b.IsSuppressed,
		HasSuppressions: len(b.SuppressedFiles) > 0,
	}, nil
}

func resolveScopeFiles(ctx context.Context, opts Options, runner contract.Runner, root string, extensions []string) (files []string, usedGit bool, err error) {
	switch {
	case len(opts.Paths) > 0:
		return filterFilesByExtensions(resolveScopePaths(root, opts.Paths), extensions), false, nil
	case opts.All:
		files, err = listRepoFiles(opts.Hooks, root, extensions)
		return files, false, err
	case strings.TrimSpace(opts.CommitSHA) != "":
		if opts.Hooks != nil && opts.Hooks.CommitFiles != nil {
			files, err = opts.Hooks.CommitFiles(ctx, root, opts.CommitSHA)
		} else {
			git := engine.NewGit(runner, root)
			if !git.IsRepository(ctx) {
				return nil, false, errors.New("not a git repository; use --all to scan every file")
			}
			files, err = git.CommitFiles(ctx, opts.CommitSHA)
		}
		if err != nil {
			return nil, false, err
		}
		return filterFilesByExtensions(files, extensions), true, nil
	case opts.LastCommit:
		if opts.Hooks != nil && opts.Hooks.LastCommitFiles != nil {
			files, err = opts.Hooks.LastCommitFiles(ctx, root)
		} else {
			git := engine.NewGit(runner, root)
			if !git.IsRepository(ctx) {
				if opts.FallbackAllWhenNotGit {
					files, err = listRepoFiles(opts.Hooks, root, extensions)
					return files, false, err
				}
				return nil, false, errors.New("not a git repository; use --all to scan every file")
			}
			files, err = git.LastCommitFiles(ctx)
			if err == nil && len(files) == 0 {
				files, err = git.ChangedFiles(ctx)
			}
		}
		if err != nil {
			return nil, false, err
		}
		return filterFilesByExtensions(files, extensions), true, nil
	case opts.Staged:
		if opts.Hooks != nil && opts.Hooks.StagedFiles != nil {
			files, err = opts.Hooks.StagedFiles(ctx, root)
		} else {
			git := engine.NewGit(runner, root)
			if !git.IsRepository(ctx) {
				return nil, false, errors.New("not a git repository; use --all to scan every file")
			}
			files, err = git.StagedFiles(ctx)
		}
		if err != nil {
			return nil, false, err
		}
		return filterFilesByExtensions(files, extensions), true, nil
	default:
		if opts.Hooks != nil && opts.Hooks.ChangedFiles != nil {
			files, err = opts.Hooks.ChangedFiles(ctx, root)
		} else {
			git := engine.NewGit(runner, root)
			if !git.IsRepository(ctx) {
				if opts.FallbackAllWhenNotGit {
					files, err = listRepoFiles(opts.Hooks, root, extensions)
					return files, false, err
				}
				return nil, false, errors.New("not a git repository; use --all to scan every file")
			}
			files, err = git.ChangedFiles(ctx)
		}
		if err != nil {
			return nil, false, err
		}
		return filterFilesByExtensions(files, extensions), true, nil
	}
}

func collectDiffFiles(ctx context.Context, git *engine.Git, root string, opts Options) ([]engine.DiffFile, engine.DiffContentLoader, error) {
	diffText, err := git.UnifiedDiff(ctx, engine.DiffOptions{
		Staged:     opts.Staged,
		LastCommit: opts.LastCommit,
		CommitSHA:  opts.CommitSHA,
	})
	if err != nil {
		return nil, nil, err
	}
	diffFiles := engine.ParseUnifiedDiff(diffText)

	if !opts.Staged && !opts.LastCommit && strings.TrimSpace(opts.CommitSHA) == "" {
		untracked, uErr := git.UntrackedFiles(ctx)
		if uErr == nil && len(untracked) > 0 {
			seen := make(map[string]struct{}, len(diffFiles))
			for _, df := range diffFiles {
				seen[df.Path] = struct{}{}
			}
			for _, rel := range untracked {
				slash := filepath.ToSlash(filepath.Clean(rel))
				if strings.HasPrefix(slash, ".slopguard/") {
					continue
				}
				if _, ok := seen[slash]; ok {
					continue
				}
				if content, ok := engine.ReadWorkingFileIfExists(root, slash); ok {
					diffFiles = append(diffFiles, engine.BuildDiffFileFromContent(slash, content))
				}
			}
		}
	}

	loader := func(slashPath string, isNewVersion bool) (string, bool) {
		if isNewVersion {
			switch {
			case opts.Staged:
				if content, ok, err := git.ShowObject(ctx, ":"+slashPath); err == nil && ok {
					return content, true
				}
			case strings.TrimSpace(opts.CommitSHA) != "":
				if content, ok, err := git.ShowObject(ctx, strings.TrimSpace(opts.CommitSHA)+":"+slashPath); err == nil && ok {
					return content, true
				}
			case opts.LastCommit:
				if content, ok, err := git.ShowObject(ctx, "HEAD:"+slashPath); err == nil && ok {
					return content, true
				}
			default:
				return engine.ReadWorkingFileIfExists(root, slashPath)
			}
			return engine.ReadWorkingFileIfExists(root, slashPath)
		}
		switch {
		case strings.TrimSpace(opts.CommitSHA) != "":
			if content, ok, err := git.ShowObject(ctx, strings.TrimSpace(opts.CommitSHA)+"^:"+slashPath); err == nil && ok {
				return content, true
			}
		case opts.LastCommit:
			if content, ok, err := git.ShowObject(ctx, "HEAD^:"+slashPath); err == nil && ok {
				return content, true
			}
		default:
			if content, ok, err := git.ShowObject(ctx, "HEAD:"+slashPath); err == nil && ok {
				return content, true
			}
		}
		return "", false
	}
	return diffFiles, loader, nil
}

func baselineStateToEngine(b BaselineState) *engine.Baseline {
	return &engine.Baseline{
		Version:         engine.BaselineVersion,
		Metrics:         b.Metrics,
		SuppressedFiles: b.SuppressedFiles,
	}
}

func buildAdapterEscapeRules(adapters []contract.Adapter) []engine.AdapterEscapeRule {
	rules := make([]engine.AdapterEscapeRule, 0, len(adapters))
	for _, adp := range adapters {
		patterns := adp.EscapePatterns()
		if len(patterns) == 0 {
			continue
		}
		rules = append(rules, engine.AdapterEscapeRule{
			Extensions: adp.Extensions(),
			Patterns:   patterns,
		})
	}
	return rules
}

func listRepoFiles(hooks *Hooks, root string, extensions []string) ([]string, error) {
	if hooks != nil && hooks.ListFiles != nil {
		return hooks.ListFiles(root, extensions)
	}
	return engine.ListFiles(root, extensions)
}

func planAdapterChecks(hooks *Hooks, runner contract.Runner, root string, checks []contract.Check, strictTools bool, requiredTools []string) (CheckPlan, error) {
	if hooks != nil && hooks.PlanChecks != nil {
		return hooks.PlanChecks(root, checks, strictTools, requiredTools)
	}
	planned, err := adapter.PlanChecksWithOptions(runner, root, checks, adapter.PlanOptions{
		StrictTools:   strictTools,
		RequiredTools: requiredTools,
	})
	out := CheckPlan{Runnable: planned.Runnable}
	for _, sk := range planned.Skipped {
		out.Skipped = append(out.Skipped, sk.ID)
		out.SkippedDetails = append(out.SkippedDetails, contract.SkippedDetail{
			ID:     sk.ID,
			Tool:   sk.Tool,
			Reason: sk.Reason,
		})
	}
	return out, err
}

func executeChecks(ctx context.Context, hooks *Hooks, runner contract.Runner, root string, jobs int, tasks []CheckTask) ([]CheckOutcome, error) {
	if hooks != nil && hooks.RunChecks != nil {
		return hooks.RunChecks(ctx, root, jobs, tasks)
	}
	checks := make([]contract.Check, len(tasks))
	for i, task := range tasks {
		checks[i] = task.Check
	}
	results := engine.RunChecksConcurrent(ctx, runner, root, checks, jobs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	outcomes := make([]CheckOutcome, len(tasks))
	for i, task := range tasks {
		outcomes[i] = CheckOutcome{Task: task, Result: results[i].Result}
	}
	return outcomes, nil
}

func parseAdapterFindings(hooks *Hooks, adp contract.Adapter, checkID string, res contract.RunResult) []contract.Finding {
	if hooks != nil && hooks.ParseFindings != nil {
		return hooks.ParseFindings(adp, checkID, res)
	}
	return adapter.ParseFindings(adp, checkID, res)
}

func computeEscapes(hooks *Hooks, root string, files []string, patterns []contract.EscapePattern) (map[string]int, error) {
	if hooks != nil && hooks.ComputeEscapes != nil {
		return hooks.ComputeEscapes(root, files, patterns)
	}
	return engine.CountEscapes(root, files, patterns)
}

func evaluateRatchet(hooks *Hooks, escapes, baseline map[string]int) ([]contract.Finding, []contract.MetricDelta) {
	if hooks != nil && hooks.Ratchet != nil {
		return hooks.Ratchet(escapes, baseline)
	}
	base := engine.NewBaselineFromMetrics(baseline, time.Time{})
	cmp := engine.Compare(escapes, base)
	findings := make([]contract.Finding, 0, len(cmp.Regressions))
	for _, delta := range cmp.Regressions {
		findings = append(findings, contract.Finding{
			Rule: "ratchet:" + delta.Name,
			Message: fmt.Sprintf("escape hatch %s increased: %d > baseline %d (+%d)",
				delta.Name, delta.Current, delta.Baseline, delta.Delta),
			Severity: contract.SeverityError,
		})
	}
	return findings, cmp.Improvements
}

func runUISmoke(ctx context.Context, hooks *Hooks, runner contract.Runner, root string) (contract.UISmokeResult, error) {
	if hooks != nil && hooks.RunUISmoke != nil {
		return hooks.RunUISmoke(ctx, root)
	}
	return adapter.RunUISmoke(ctx, runner, adapter.UIOptions{ProjectRoot: root})
}

func emptyVerifyResult(started time.Time) contract.VerifyResult {
	return contract.VerifyResult{
		Passed:     true,
		Findings:   []contract.Finding{},
		Escapes:    map[string]int{},
		DurationMs: time.Since(started).Milliseconds(),
	}
}

func classifyFindingCategory(f contract.Finding) string {
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

func buildFindingSummary(f contract.Finding) string {
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

func formatFindingSnippet(f contract.Finding) string {
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

func firstNonEmptyLine(values ...string) string {
	for _, value := range values {
		for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				if len(line) > 300 {
					line = line[:300] + "..."
				}
				return line
			}
		}
	}
	return ""
}

func findAdapter(adapters []contract.Adapter, name string) contract.Adapter {
	for _, adp := range adapters {
		if adp.Name() == name {
			return adp
		}
	}
	return nil
}

func filterAdaptersByName(adapters []contract.Adapter, names []string) []contract.Adapter {
	if len(names) == 0 {
		return adapters
	}
	want := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			want[name] = struct{}{}
		}
	}
	out := make([]contract.Adapter, 0, len(adapters))
	for _, adp := range adapters {
		if _, ok := want[adp.Name()]; ok {
			out = append(out, adp)
		}
	}
	return out
}

func filterSuppressedFiles(files []string, baseline BaselineState) (kept, suppressed []string) {
	kept = make([]string, 0, len(files))
	for _, file := range files {
		if baseline.Suppresses(file) {
			suppressed = append(suppressed, file)
			continue
		}
		kept = append(kept, file)
	}
	return kept, suppressed
}

func countUniqueFiles(groups ...[]string) int {
	seen := make(map[string]struct{})
	for _, group := range groups {
		for _, file := range group {
			seen[file] = struct{}{}
		}
	}
	return len(seen)
}

func sumEscapes(escapes map[string]int) int {
	total := 0
	for _, count := range escapes {
		total += count
	}
	return total
}

func collectExtensions(adapters []contract.Adapter) []string {
	set := make(map[string]struct{})
	for _, adp := range adapters {
		for _, ext := range adp.Extensions() {
			ext = strings.ToLower(strings.TrimSpace(ext))
			if ext == "" {
				continue
			}
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			set[ext] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for ext := range set {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

func collectEscapePatterns(adapters []contract.Adapter) []contract.EscapePattern {
	seen := make(map[string]struct{})
	var patterns []contract.EscapePattern
	for _, adp := range adapters {
		for _, pattern := range adp.EscapePatterns() {
			if pattern.Name == "" || pattern.Regex == "" {
				continue
			}
			if _, ok := seen[pattern.Name]; ok {
				continue
			}
			seen[pattern.Name] = struct{}{}
			patterns = append(patterns, pattern)
		}
	}
	sort.Slice(patterns, func(i, j int) bool { return patterns[i].Name < patterns[j].Name })
	return patterns
}

func filterFilesByExtensions(files []string, extensions []string) []string {
	if len(extensions) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		set[strings.ToLower(ext)] = struct{}{}
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		if _, ok := set[strings.ToLower(filepath.Ext(file))]; ok {
			out = append(out, file)
		}
	}
	sort.Strings(out)
	return out
}

func resolveScopePaths(root string, paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		file := raw
		if filepath.IsAbs(file) {
			rel, err := filepath.Rel(root, file)
			if err != nil {
				continue
			}
			file = rel
		} else {
			file = filepath.Clean(filepath.FromSlash(file))
		}
		if file == "." || filepath.IsAbs(file) || strings.HasPrefix(file, ".."+string(filepath.Separator)) {
			continue
		}
		info, err := os.Stat(filepath.Join(root, file))
		if err != nil || info.IsDir() {
			continue
		}
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}
