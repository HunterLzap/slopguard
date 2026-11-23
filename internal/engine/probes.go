package engine

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

const (
	// ProbeConstraintRuleID 是不可执行约束注释探针的规则 ID。
	ProbeConstraintRuleID = "probe:constraint_comment"
	// ProbeWorkaroundRuleID 是敷衍/绕路注释探针的规则 ID。
	ProbeWorkaroundRuleID = "probe:workaround_comment"

	// ConstraintCommentHint 指导宿主 Agent 将自然语言约束固化为可执行检查。
	ConstraintCommentHint = "Encode this constraint as the cheapest type, runtime assertion, test, or lint rule, then delete the comment once enforced."
	// WorkaroundCommentHint 指导宿主 Agent 实施最小根因修复并清理临时补丁。
	WorkaroundCommentHint = "Implement the minimal root-cause fix and remove the temporary workaround; do not stack symptom guards."
)

var (
	constraintCommentRE = regexp.MustCompile(`(?i)\b(?:do\s+not\s+remove|do\s+not\s+change\s+wording|talk\s+to\s+.+?\s+before\s+changing|do\s+not\s+touch)\b`)
	workaroundCommentRE = regexp.MustCompile(`(?i)\b(?:workaround|quick[\s-]+fix|temp(?:orary)?[\s-]+hack|symptom[\s-]+guard)\b`)
)

// ScanDiffProbes 仅扫描 diff 新增行（+）中的注释片段，检测：
// 1. probe:constraint_comment（约束类注释）
// 2. probe:workaround_comment（敷衍/绕路类注释）
// 非注释代码行、字符串字面量、删除行（-）及 suppressed_files 均被跳过。
func ScanDiffProbes(files []DiffFile, severity contract.Severity, baseline *Baseline, loader DiffContentLoader) []contract.Finding {
	if severity == "" {
		severity = contract.SeverityError
	}
	var findings []contract.Finding
	for _, df := range files {
		if df.IsBinary || df.IsDeleted {
			continue
		}
		if strings.HasPrefix(df.Path, ".slopguard/") {
			continue
		}
		if !SupportsCommentLexing(df.Path) {
			continue
		}
		if baseline != nil && baseline.IsSuppressed(df.Path) {
			continue
		}
		addedLexed, _ := LexDiffFile(df, loader)
		for _, line := range addedLexed {
			comment := strings.TrimSpace(line.Comments)
			if comment == "" {
				continue
			}
			if match := constraintCommentRE.FindString(comment); match != "" {
				findings = append(findings, contract.Finding{
					File:       df.Path,
					Line:       line.Line,
					Rule:       ProbeConstraintRuleID,
					Message:    fmt.Sprintf("non-executable constraint comment detected (%q)", match),
					Severity:   severity,
					ActionHint: ConstraintCommentHint,
				})
			}
			if match := workaroundCommentRE.FindString(comment); match != "" {
				findings = append(findings, contract.Finding{
					File:       df.Path,
					Line:       line.Line,
					Rule:       ProbeWorkaroundRuleID,
					Message:    fmt.Sprintf("workaround/excuse comment detected (%q)", match),
					Severity:   severity,
					ActionHint: WorkaroundCommentHint,
				})
			}
		}
	}
	return findings
}
