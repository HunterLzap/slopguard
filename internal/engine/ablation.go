package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

const (
	// AblationMissingTestRuleID 在修改未包含测试改动时触发。
	AblationMissingTestRuleID = "ablation:missing_test"
	// AblationNotReproducingRuleID 在 RED 阶段测试仍通过时触发。
	AblationNotReproducingRuleID = "ablation:test_not_reproducing"
	// AblationFixFailedRuleID 在 GREEN 阶段测试失败时触发。
	AblationFixFailedRuleID = "ablation:fix_failed"
)

// DefaultTestPatterns 是未配置 verify.test_patterns 时使用的默认测试文件 glob 列表。
var DefaultTestPatterns = []string{
	"*_test.go",
	"*.test.ts",
	"*.spec.ts",
	"*.test.tsx",
	"*.spec.tsx",
	"*.test.js",
	"test_*.py",
	"*_test.py",
}

// AblationOptions 配置一次自动消融验证。
type AblationOptions struct {
	ProjectRoot  string
	TestCmd      string
	TestPatterns []string
	Staged       bool
	Timeout      time.Duration
}

// AblationOutcome 是 RunAblation 的结构化结果。
type AblationOutcome struct {
	Result   contract.AblationResult
	Findings []contract.Finding
	Warnings []string
}

type ablationChange struct {
	Path    string
	Deleted bool
	Staged  bool
}

// RunAblation 在完全隔离的临时 git worktree 中执行 RED -> GREEN 自动消融验证，
// 绝对不修改主仓库的工作区、暂存区或依赖目录。
func RunAblation(ctx context.Context, runner contract.Runner, opts AblationOptions) (AblationOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if runner == nil {
		runner = NewExecRunner()
	}
	root := strings.TrimSpace(opts.ProjectRoot)
	if root == "" {
		return AblationOutcome{}, errors.New("engine: ablation project root is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return AblationOutcome{}, fmt.Errorf("engine: resolve ablation root: %w", err)
	}
	testCmd := strings.TrimSpace(opts.TestCmd)
	if testCmd == "" {
		return AblationOutcome{}, errors.New("verify.test_cmd is not configured; set verify.test_cmd in .slopguard/config.yaml to run --ablation")
	}

	git := NewGit(runner, absRoot)
	if !git.IsRepository(ctx) {
		return AblationOutcome{}, errors.New("ablation requires a git repository")
	}
	if !git.hasHEAD(ctx) {
		return AblationOutcome{}, errors.New("ablation requires at least one commit (HEAD)")
	}

	patterns := opts.TestPatterns
	if len(patterns) == 0 {
		patterns = DefaultTestPatterns
	}

	changes, err := collectAblationChanges(ctx, git, opts.Staged)
	if err != nil {
		return AblationOutcome{}, fmt.Errorf("collect changes for ablation: %w", err)
	}

	var testChanges, prodChanges []ablationChange
	var testFiles, prodFiles []string
	for _, ch := range changes {
		if isAblationIgnoredPath(ch.Path) {
			continue
		}
		if matchesAnyTestPattern(ch.Path, patterns) {
			testChanges = append(testChanges, ch)
			testFiles = append(testFiles, ch.Path)
		} else {
			prodChanges = append(prodChanges, ch)
			prodFiles = append(prodFiles, ch.Path)
		}
	}

	if len(testChanges) == 0 {
		msg := "no test file changes detected; bug fixes under --ablation must include reproducing test changes"
		return AblationOutcome{
			Result: contract.AblationResult{
				Status:    "failed",
				Reason:    msg,
				ProdFiles: prodFiles,
			},
			Findings: []contract.Finding{{
				Rule:       AblationMissingTestRuleID,
				Message:    msg,
				Severity:   contract.SeverityError,
				ActionHint: "Add or update a test matching verify.test_patterns that reproduces the bug before fixing it.",
			}},
		}, nil
	}

	if len(prodChanges) == 0 {
		reason := "only test files changed (no production code changes); skipping ablation"
		return AblationOutcome{
			Result: contract.AblationResult{
				Status:    "skipped",
				Reason:    reason,
				TestFiles: testFiles,
			},
			Warnings: []string{reason},
		}, nil
	}

	parentDir, err := os.MkdirTemp("", "slopguard-ablation-*")
	if err != nil {
		return AblationOutcome{}, fmt.Errorf("create ablation temp dir: %w", err)
	}
	wtDir := filepath.Join(parentDir, "wt")

	var createdSymlinks []string
	defer func() {
		// 先移除我们创建的依赖目录软链，避免任何清理路径触碰主仓库目录。
		for _, link := range createdSymlinks {
			if info, lErr := os.Lstat(link); lErr == nil && info.Mode()&os.ModeSymlink != 0 {
				_ = os.Remove(link)
			}
		}
		cleanCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = git.raw(cleanCtx, "worktree", "remove", "--force", wtDir)
		_ = git.raw(cleanCtx, "worktree", "prune")
		_ = os.RemoveAll(parentDir)
	}()

	if _, err := git.output(ctx, "worktree", "add", "--detach", wtDir, "HEAD"); err != nil {
		return AblationOutcome{}, fmt.Errorf("create temporary git worktree: %w", err)
	}

	for _, depDir := range []string{"node_modules", ".venv"} {
		srcDep := filepath.Join(absRoot, depDir)
		dstDep := filepath.Join(wtDir, depDir)
		if info, sErr := os.Stat(srcDep); sErr == nil && info.IsDir() {
			if _, lErr := os.Lstat(dstDep); os.IsNotExist(lErr) {
				if symErr := os.Symlink(srcDep, dstDep); symErr == nil {
					createdSymlinks = append(createdSymlinks, dstDep)
				}
			}
		}
	}

	// RED phase: sync ONLY test files into temporary worktree (prod files stay at HEAD).
	for _, ch := range testChanges {
		if err := syncChangeToWorktree(ctx, absRoot, wtDir, ch); err != nil {
			return AblationOutcome{}, fmt.Errorf("sync test file %s for RED phase: %w", ch.Path, err)
		}
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	redRes := runTestCommand(ctx, runner, wtDir, testCmd, timeout)
	if redRes.Skipped {
		return AblationOutcome{}, fmt.Errorf("ablation test_cmd executable not found for %q", testCmd)
	}
	if redRes.Err != nil {
		return AblationOutcome{}, fmt.Errorf("ablation RED phase failed to execute test_cmd: %w", redRes.Err)
	}
	redMs := redRes.Duration.Milliseconds()
	if redRes.ExitCode == 0 {
		msg := fmt.Sprintf("test_cmd %q passed in RED phase without production fix (test does not reproduce the bug)", testCmd)
		return AblationOutcome{
			Result: contract.AblationResult{
				Status:        "failed",
				Reason:        msg,
				TestFiles:     testFiles,
				ProdFiles:     prodFiles,
				RedDurationMs: redMs,
			},
			Findings: []contract.Finding{{
				Rule:       AblationNotReproducingRuleID,
				Message:    msg,
				Severity:   contract.SeverityError,
				ActionHint: "Ensure the modified/added test fails on HEAD before the production fix is applied.",
			}},
		}, nil
	}

	// GREEN phase: sync production changes into temporary worktree and re-run test_cmd.
	for _, ch := range prodChanges {
		if err := syncChangeToWorktree(ctx, absRoot, wtDir, ch); err != nil {
			return AblationOutcome{}, fmt.Errorf("sync production file %s for GREEN phase: %w", ch.Path, err)
		}
	}

	greenRes := runTestCommand(ctx, runner, wtDir, testCmd, timeout)
	if greenRes.Skipped {
		return AblationOutcome{}, fmt.Errorf("ablation test_cmd executable not found for %q", testCmd)
	}
	if greenRes.Err != nil {
		return AblationOutcome{}, fmt.Errorf("ablation GREEN phase failed to execute test_cmd: %w", greenRes.Err)
	}
	greenMs := greenRes.Duration.Milliseconds()
	if greenRes.ExitCode != 0 {
		snippet := summarizeFailureOutput(greenRes.Stderr, greenRes.Stdout)
		msg := fmt.Sprintf("test_cmd failed in GREEN phase (exit %d): %s", greenRes.ExitCode, snippet)
		return AblationOutcome{
			Result: contract.AblationResult{
				Status:          "failed",
				Reason:          msg,
				TestFiles:       testFiles,
				ProdFiles:       prodFiles,
				RedDurationMs:   redMs,
				GreenDurationMs: greenMs,
			},
			Findings: []contract.Finding{{
				Rule:     AblationFixFailedRuleID,
				Message:  msg,
				Severity: contract.SeverityError,
			}},
		}, nil
	}

	return AblationOutcome{
		Result: contract.AblationResult{
			Status:          "passed",
			TestFiles:       testFiles,
			ProdFiles:       prodFiles,
			RedDurationMs:   redMs,
			GreenDurationMs: greenMs,
		},
	}, nil
}

func collectAblationChanges(ctx context.Context, git *Git, staged bool) ([]ablationChange, error) {
	byPath := make(map[string]ablationChange)
	if staged {
		out, err := git.output(ctx, "diff", "--cached", "--name-status", "-z", "--no-renames", "--")
		if err != nil {
			return nil, err
		}
		parseNameStatusZ(out, true, byPath)
	} else {
		out, err := git.output(ctx, "diff", "--name-status", "-z", "--no-renames", "HEAD", "--")
		if err != nil {
			return nil, err
		}
		parseNameStatusZ(out, false, byPath)
		untracked, err := git.UntrackedFiles(ctx)
		if err != nil {
			return nil, err
		}
		for _, u := range untracked {
			slash := filepath.ToSlash(filepath.Clean(u))
			if slash == "" || slash == "." {
				continue
			}
			if _, exists := byPath[slash]; !exists {
				byPath[slash] = ablationChange{Path: slash, Deleted: false, Staged: false}
			}
		}
	}

	keys := make([]string, 0, len(byPath))
	for k := range byPath {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	changes := make([]ablationChange, 0, len(keys))
	for _, k := range keys {
		changes = append(changes, byPath[k])
	}
	return changes, nil
}

func parseNameStatusZ(data string, staged bool, dst map[string]ablationChange) {
	tokens := splitNULTokens(data)
	for i := 0; i+1 < len(tokens); i += 2 {
		status := strings.TrimSpace(tokens[i])
		slashPath := filepath.ToSlash(filepath.Clean(tokens[i+1]))
		if slashPath == "" || slashPath == "." {
			continue
		}
		dst[slashPath] = ablationChange{
			Path:    slashPath,
			Deleted: strings.HasPrefix(status, "D"),
			Staged:  staged,
		}
	}
}

func matchesAnyTestPattern(slashPath string, patterns []string) bool {
	base := path.Base(slashPath)
	for _, rawPattern := range patterns {
		pat := strings.TrimSpace(filepath.ToSlash(rawPattern))
		if pat == "" {
			continue
		}
		if strings.Contains(pat, "/") {
			if matchGlobWithDoublestar(pat, slashPath) {
				return true
			}
			continue
		}
		if matched, err := path.Match(pat, base); err == nil && matched {
			return true
		}
	}
	return false
}

func matchGlobWithDoublestar(pattern, target string) bool {
	if !strings.Contains(pattern, "**") {
		matched, err := path.Match(pattern, target)
		return err == nil && matched
	}
	patParts := strings.Split(pattern, "/")
	tgtParts := strings.Split(target, "/")
	return matchParts(patParts, tgtParts)
}

func matchParts(pat, tgt []string) bool {
	if len(pat) == 0 {
		return len(tgt) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(tgt); i++ {
			if matchParts(pat[1:], tgt[i:]) {
				return true
			}
		}
		return false
	}
	if len(tgt) == 0 {
		return false
	}
	matched, err := path.Match(pat[0], tgt[0])
	if err != nil || !matched {
		return false
	}
	return matchParts(pat[1:], tgt[1:])
}

func isAblationIgnoredPath(slashPath string) bool {
	parts := strings.Split(slashPath, "/")
	for _, part := range parts {
		switch part {
		case ".slopguard", "node_modules", ".venv", "venv", "__pycache__", ".pytest_cache", ".ruff_cache", ".mypy_cache":
			return true
		}
	}
	return false
}

func syncChangeToWorktree(ctx context.Context, srcRoot, wtDir string, ch ablationChange) error {
	relNative := filepath.FromSlash(ch.Path)
	if filepath.IsAbs(relNative) || strings.HasPrefix(relNative, ".."+string(filepath.Separator)) || relNative == ".." {
		return fmt.Errorf("unsafe relative path %q", ch.Path)
	}
	dstPath := filepath.Join(wtDir, relNative)

	// 防御性校验：拒绝穿过指向工作区外部（如软链 node_modules/.venv）的写入或删除。
	if wtReal, err := filepath.EvalSymlinks(wtDir); err == nil {
		if dirReal, dErr := resolveNearestExistingDir(filepath.Dir(dstPath)); dErr == nil {
			relCheck, rErr := filepath.Rel(wtReal, dirReal)
			if rErr != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) || filepath.IsAbs(relCheck) {
				return fmt.Errorf("refusing to mutate path outside temporary worktree: %s", ch.Path)
			}
		}
	}

	if ch.Deleted {
		if err := os.Remove(dstPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}

	if ch.Staged {
		cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", ":"+ch.Path)
		cmd.Dir = srcRoot
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("read staged blob :%s: %w", ch.Path, err)
		}
		mode := os.FileMode(0o644)
		if info, statErr := os.Stat(filepath.Join(srcRoot, relNative)); statErr == nil && !info.IsDir() {
			mode = info.Mode().Perm()
		}
		return os.WriteFile(dstPath, out, mode)
	}

	srcPath := filepath.Join(srcRoot, relNative)
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			if rmErr := os.Remove(dstPath); rmErr != nil && !os.IsNotExist(rmErr) {
				return rmErr
			}
			return nil
		}
		return err
	}
	if srcInfo.IsDir() {
		return nil
	}
	if dstInfo, dErr := os.Stat(dstPath); dErr == nil && os.SameFile(srcInfo, dstInfo) {
		return fmt.Errorf("refusing to overwrite source file through symlink: %s", ch.Path)
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func resolveNearestExistingDir(dir string) (string, error) {
	cur := filepath.Clean(dir)
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		cur = parent
	}
}

func runTestCommand(ctx context.Context, runner contract.Runner, dir, testCmd string, timeout time.Duration) contract.RunResult {
	name, args := splitTestCommand(testCmd)
	return runner.Run(ctx, contract.RunSpec{
		Name:    name,
		Args:    args,
		Dir:     dir,
		Timeout: timeout,
	})
}

func splitTestCommand(testCmd string) (string, []string) {
	if strings.ContainsAny(testCmd, "|&;<>()$`\"'=\n*?[]~") {
		if runtime.GOOS == "windows" {
			return "cmd", []string{"/C", testCmd}
		}
		return "sh", []string{"-c", testCmd}
	}
	fields := strings.Fields(testCmd)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], fields[1:]
}

func summarizeFailureOutput(stderr, stdout string) string {
	combined := strings.TrimSpace(stderr)
	if combined == "" {
		combined = strings.TrimSpace(stdout)
	} else if out := strings.TrimSpace(stdout); out != "" {
		combined = out + "\n" + combined
	}
	if combined == "" {
		return "no failure output"
	}
	lines := strings.Split(strings.ReplaceAll(combined, "\r\n", "\n"), "\n")
	var nonEmpty []string
	for _, l := range lines {
		if trimmed := strings.TrimSpace(l); trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}
	if len(nonEmpty) > 10 {
		nonEmpty = nonEmpty[len(nonEmpty)-10:]
	}
	summary := strings.Join(nonEmpty, " | ")
	if len(summary) > 500 {
		summary = summary[len(summary)-500:]
	}
	return summary
}
