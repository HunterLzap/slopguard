package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

var (
	doubleQuotedRE = regexp.MustCompile(`"[^"\n\\]*(?:\\.[^"\n\\]*)*"`)
	backtickRE     = regexp.MustCompile("`[^`\\n\\\\]*(?:\\\\.[^`\\n\\\\]*)*`")
	singleQuotedRE = regexp.MustCompile(`(^|[^a-zA-Z0-9_])'[^'\n\\]*(?:\\.[^'\n\\]*)*'`)
	slashPathRE    = regexp.MustCompile(`(?:[A-Za-z]:)?(?:[./\\][^\s:'"()\[\],;]+|[a-zA-Z0-9_.-]+(?:[/\\][a-zA-Z0-9_.-]+)+)(?::\d+(?::\d+)?)?`)
	fileWithLocRE  = regexp.MustCompile(`\b[a-zA-Z0-9_.-]+\.[a-zA-Z0-9]{1,8}:\d+(?::\d+)?\b`)
	sourceFileRE   = regexp.MustCompile(`\b[a-zA-Z0-9_.-]+\.(?:ts|tsx|mts|cts|js|jsx|mjs|cjs|py|pyi|go|rs|java|kt|swift|c|cc|cpp|h|hpp|yaml|yml|json|toml)\b`)
	numberRE       = regexp.MustCompile(`\b\d+\b`)
)

// ParseFindingRule 从 Finding.Rule 中拆出适配器/工具标识与规则 ID。
// 例如 "pyright:reportOptionalMemberAccess" -> ("pyright", "reportOptionalMemberAccess")；
// 无冒号时返回 ("<rule or fallback>", "")。
func ParseFindingRule(rule, fallbackAdapter string) (adapterName, ruleID string) {
	trimmed := strings.TrimSpace(rule)
	fallback := strings.TrimSpace(fallbackAdapter)
	if idx := strings.IndexByte(trimmed, ':'); idx >= 0 {
		prefix := strings.TrimSpace(trimmed[:idx])
		id := strings.TrimSpace(trimmed[idx+1:])
		if prefix == "" {
			prefix = fallback
		}
		return prefix, id
	}
	if trimmed != "" {
		return trimmed, ""
	}
	return fallback, ""
}

// SanitizeMessageForFingerprint 剥离报错文本中的引号内标识符、文件路径与数字，
// 用于无明确 ruleID 的错误聚类。
func SanitizeMessageForFingerprint(message string) string {
	s := strings.TrimSpace(message)
	if s == "" {
		return ""
	}
	s = doubleQuotedRE.ReplaceAllString(s, " <str> ")
	s = backtickRE.ReplaceAllString(s, " <str> ")
	s = singleQuotedRE.ReplaceAllString(s, "${1}<str>")
	s = slashPathRE.ReplaceAllString(s, " <path> ")
	s = fileWithLocRE.ReplaceAllString(s, " <path> ")
	s = sourceFileRE.ReplaceAllString(s, " <path> ")
	s = numberRE.ReplaceAllString(s, "<num>")
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// ClusterKey 生成规范化聚簇键：
// ruleID 非空时返回 strings.ToLower(adapter + ":" + ruleID)；
// ruleID 为空时剥离动态变量名、路径和数字后参与键构造。
func ClusterKey(adapterName, ruleID, message string) string {
	adp := strings.ToLower(strings.TrimSpace(adapterName))
	id := strings.ToLower(strings.TrimSpace(ruleID))
	if id != "" {
		return adp + ":" + id
	}
	normMsg := SanitizeMessageForFingerprint(message)
	if normMsg == "" {
		return adp + ":"
	}
	return adp + ":" + normMsg
}

// MistakeFingerprint 对规范化聚簇键计算 SHA256 指纹。
// 传入非空 project 可防止不同项目在全局 mistakes 表上发生指纹跨库串号。
func MistakeFingerprint(project, adapterName, ruleID, message string) string {
	key := ClusterKey(adapterName, ruleID, message)
	payload := key
	if proj := canonicalizeProjectForFingerprint(project); proj != "" {
		payload = proj + "\x00" + key
	}
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// FindingFingerprint 为自动捕获的 verify Finding 计算聚类指纹。
func FindingFingerprint(project string, f contract.Finding, fallbackAdapter string) string {
	adp, ruleID := ParseFindingRule(f.Rule, fallbackAdapter)
	return MistakeFingerprint(project, adp, ruleID, f.Message)
}

func canonicalizeProjectForFingerprint(project string) string {
	trimmed := strings.TrimSpace(project)
	if trimmed == "" {
		return ""
	}
	if abs, err := filepath.Abs(trimmed); err == nil {
		trimmed = abs
	}
	if resolved, err := filepath.EvalSymlinks(trimmed); err == nil && resolved != "" {
		trimmed = resolved
	}
	return filepath.Clean(trimmed)
}
