package adapter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

// ParseFindings 调用 contract.Adapter.ParseOutput，并在检查失败但解析不到
// 结构化 Findings 时给出兜底 Finding，避免失败被静默吞掉。
func ParseFindings(a contract.Adapter, checkID string, result contract.RunResult) []contract.Finding {
	output := strings.TrimSpace(result.Stdout + "\n" + result.Stderr)
	if a != nil {
		if findings := a.ParseOutput(checkID, output); len(findings) > 0 {
			return findings
		}
	}
	if result.Err != nil || result.ExitCode != 0 {
		return []contract.Finding{summaryFinding(checkID, result)}
	}
	return nil
}

func summaryFinding(checkID string, result contract.RunResult) contract.Finding {
	message := firstNonEmptyLine(result.Stderr)
	if message == "" {
		message = firstNonEmptyLine(result.Stdout)
	}
	switch {
	case result.Err != nil:
		message = fmt.Sprintf("检查执行失败：%v", result.Err)
	case message == "":
		message = fmt.Sprintf("检查失败（退出码 %d）", result.ExitCode)
	}
	return contract.Finding{Rule: checkID, Message: message, Severity: contract.SeverityError}
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// TS/JS
// ---------------------------------------------------------------------------

// ParseOutput 实现 contract.Adapter。
func (a *TypeScriptAdapter) ParseOutput(checkID string, output string) []contract.Finding {
	switch checkID {
	case "eslint":
		if findings, err := parseESLintOutput(output); err == nil {
			return findings
		}
	case "tsc":
		return parseTSCOutput(output)
	case "knip":
		return parseKnipOutput(output)
	case "jscpd":
		return parseJSCPDOutput(output)
	}
	return nil
}

type eslintReportFile struct {
	FilePath string `json:"filePath"`
	Messages []struct {
		RuleID   string `json:"ruleId"`
		Severity int    `json:"severity"`
		Message  string `json:"message"`
		Line     int    `json:"line"`
		Column   int    `json:"column"`
	} `json:"messages"`
}

func parseESLintOutput(out string) ([]contract.Finding, error) {
	var report []eslintReportFile
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); err != nil {
		return nil, err
	}
	var findings []contract.Finding
	for _, file := range report {
		for _, message := range file.Messages {
			rule := "eslint"
			if message.RuleID != "" {
				rule = "eslint:" + message.RuleID
			}
			severity := contract.SeverityWarn
			if message.Severity >= 2 {
				severity = contract.SeverityError
			}
			findings = append(findings, contract.Finding{
				File:     file.FilePath,
				Line:     message.Line,
				Column:   message.Column,
				Rule:     rule,
				Message:  message.Message,
				Severity: severity,
			})
		}
	}
	return findings, nil
}

var (
	tscLocationRE = regexp.MustCompile(`^(.+?)\((\d+),(\d+)\):\s+(error|warning)\s+(TS\d+):\s*(.*)$`)
	tscDashRE     = regexp.MustCompile(`^(.+?):(\d+):(\d+)\s+-\s+(error|warning)\s+(TS\d+):\s*(.*)$`)
)

func parseTSCOutput(out string) []contract.Finding {
	var findings []contract.Finding
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if match := tscLocationRE.FindStringSubmatch(line); match != nil {
			findings = append(findings, tscFinding(match))
			continue
		}
		if match := tscDashRE.FindStringSubmatch(line); match != nil {
			findings = append(findings, tscFinding(match))
		}
	}
	return findings
}

func tscFinding(match []string) contract.Finding {
	severity := contract.SeverityError
	if match[4] == "warning" {
		severity = contract.SeverityWarn
	}
	return contract.Finding{
		File:     match[1],
		Line:     atoiOrZero(match[2]),
		Column:   atoiOrZero(match[3]),
		Rule:     "tsc:" + match[5],
		Message:  match[6],
		Severity: severity,
	}
}

type knipReport struct {
	Issues []struct {
		File   string `json:"file"`
		Issues []struct {
			Type    string `json:"type"`
			Symbol  string `json:"symbol"`
			Line    int    `json:"line"`
			Col     int    `json:"col"`
			Message string `json:"message"`
		} `json:"issues"`
	} `json:"issues"`
}

func parseKnipOutput(out string) []contract.Finding {
	var report knipReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); err != nil {
		return nil
	}
	var findings []contract.Finding
	for _, file := range report.Issues {
		for _, issue := range file.Issues {
			message := issue.Message
			if message == "" {
				message = strings.TrimSpace(strings.Join([]string{issue.Type, issue.Symbol}, " "))
			}
			if message == "" {
				message = "knip 检出未使用内容"
			}
			findings = append(findings, contract.Finding{
				File:     file.File,
				Line:     issue.Line,
				Column:   issue.Col,
				Rule:     "knip:" + issue.Type,
				Message:  message,
				Severity: contract.SeverityWarn,
			})
		}
	}
	return findings
}

var jscpdCloneLineRE = regexp.MustCompile(`^\s*-\s+(.+?)\s+\[(\d+):(\d+)\s*-\s*\d+:\d+\]`)

func parseJSCPDOutput(out string) []contract.Finding {
	var findings []contract.Finding
	for _, line := range strings.Split(out, "\n") {
		match := jscpdCloneLineRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		findings = append(findings, contract.Finding{
			File:     match[1],
			Line:     atoiOrZero(match[2]),
			Column:   atoiOrZero(match[3]),
			Rule:     "jscpd:duplicate",
			Message:  "检出重复代码块",
			Severity: contract.SeverityWarn,
		})
	}
	return findings
}

// ---------------------------------------------------------------------------
// Python
// ---------------------------------------------------------------------------

// ParseOutput 实现 contract.Adapter。
func (a *PythonAdapter) ParseOutput(checkID string, output string) []contract.Finding {
	switch checkID {
	case "ruff":
		if findings, err := parseRuffOutput(output); err == nil {
			return findings
		}
	case "pyright":
		if findings, err := parsePyrightOutput(output); err == nil {
			return findings
		}
	case "semgrep":
		if findings, err := parseSemgrepOutput(output); err == nil {
			return findings
		}
	}
	return nil
}

type ruffReportItem struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Filename string `json:"filename"`
	Location struct {
		Row    int `json:"row"`
		Column int `json:"column"`
	} `json:"location"`
}

func parseRuffOutput(out string) ([]contract.Finding, error) {
	var report []ruffReportItem
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); err != nil {
		return nil, err
	}
	findings := make([]contract.Finding, 0, len(report))
	for _, item := range report {
		rule := "ruff"
		if item.Code != "" {
			rule = "ruff:" + item.Code
		}
		findings = append(findings, contract.Finding{
			File:     item.Filename,
			Line:     item.Location.Row,
			Column:   item.Location.Column,
			Rule:     rule,
			Message:  item.Message,
			Severity: contract.SeverityError,
		})
	}
	return findings, nil
}

type pyrightReport struct {
	GeneralDiagnostics []struct {
		File     string `json:"file"`
		Severity string `json:"severity"`
		Message  string `json:"message"`
		Rule     string `json:"rule"`
		Range    struct {
			Start struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"start"`
		} `json:"range"`
	} `json:"generalDiagnostics"`
}

func parsePyrightOutput(out string) ([]contract.Finding, error) {
	var report pyrightReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); err != nil {
		return nil, err
	}
	findings := make([]contract.Finding, 0, len(report.GeneralDiagnostics))
	for _, diagnostic := range report.GeneralDiagnostics {
		severity := contract.SeverityWarn
		if strings.EqualFold(diagnostic.Severity, "error") {
			severity = contract.SeverityError
		}
		rule := "pyright"
		if diagnostic.Rule != "" {
			rule = "pyright:" + diagnostic.Rule
		}
		findings = append(findings, contract.Finding{
			File:     diagnostic.File,
			Line:     diagnostic.Range.Start.Line + 1,
			Column:   diagnostic.Range.Start.Character + 1,
			Rule:     rule,
			Message:  diagnostic.Message,
			Severity: severity,
		})
	}
	return findings, nil
}

type semgrepReport struct {
	Results []struct {
		CheckID string `json:"check_id"`
		Path    string `json:"path"`
		Start   struct {
			Line int `json:"line"`
			Col  int `json:"col"`
		} `json:"start"`
		Extra struct {
			Message  string `json:"message"`
			Severity string `json:"severity"`
		} `json:"extra"`
	} `json:"results"`
}

func parseSemgrepOutput(out string) ([]contract.Finding, error) {
	var report semgrepReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &report); err != nil {
		return nil, err
	}
	findings := make([]contract.Finding, 0, len(report.Results))
	for _, result := range report.Results {
		severity := contract.SeverityWarn
		switch strings.ToUpper(result.Extra.Severity) {
		case "ERROR":
			severity = contract.SeverityError
		case "WARNING", "INFO":
			severity = contract.SeverityWarn
		}
		rule := "semgrep"
		if result.CheckID != "" {
			rule = "semgrep:" + result.CheckID
		}
		findings = append(findings, contract.Finding{
			File:     result.Path,
			Line:     result.Start.Line,
			Column:   result.Start.Col,
			Rule:     rule,
			Message:  result.Extra.Message,
			Severity: severity,
		})
	}
	return findings, nil
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
