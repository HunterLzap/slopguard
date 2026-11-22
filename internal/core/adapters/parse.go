package adapters

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

type Parser func(stdout, stderr string) ([]contract.Diagnostic, error)

var (
	location     = regexp.MustCompile(`^(.+?):([0-9]+)(?::([0-9]+))?:\s*(.+)$`)
	typeScript   = regexp.MustCompile(`^(?:(.+)\(([0-9]+),([0-9]+)\): )?error (TS[0-9]+): (.+)$`)
	maven        = regexp.MustCompile(`^(.+?):\[([0-9]+),([0-9]+)\]\s*(.+)$`)
	gradleKotlin = regexp.MustCompile(`^[ew]: (?:file:/+)?(.+?):([0-9]+):([0-9]+)\s*(.+)$`)
	messageRule  = regexp.MustCompile(`^(.*?)\s*\[([\w./,:-]+)\]$`)
	ktlintRule   = regexp.MustCompile(`^(.*?)\s*\(([\w.-]+:[\w.-]+)\)$`)
	flutterLine  = regexp.MustCompile(`^\s*(error|warning|info)\s*[•-]\s*(.*?)\s*[•-]\s*(.*?)\s*[•-]\s*(\S+)\s*$`)
)

func number(value string) int {
	n, _ := strconv.Atoi(value)
	return n
}

func parseNative(stdout, stderr string) ([]contract.Diagnostic, error) {
	var result []contract.Diagnostic
	for _, line := range strings.Split(stdout+"\n"+stderr, "\n") {
		line = strings.TrimSpace(line)
		severity := "error"
		for _, prefix := range []string{"vet: ", "[ERROR] ", "[WARNING] "} {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimPrefix(line, prefix)
				if prefix == "[WARNING] " {
					severity = "warning"
				}
			}
		}
		var file, row, col, msg string
		switch {
		case maven.MatchString(line):
			parts := maven.FindStringSubmatch(line)
			file, row, col, msg = parts[1], parts[2], parts[3], parts[4]
		case gradleKotlin.MatchString(line):
			parts := gradleKotlin.FindStringSubmatch(line)
			file, row, col, msg = parts[1], parts[2], parts[3], parts[4]
			if strings.HasPrefix(line, "w:") {
				severity = "warning"
			}
		case location.MatchString(line):
			parts := location.FindStringSubmatch(line)
			file, row, col, msg = parts[1], parts[2], parts[3], parts[4]
		default:
			continue
		}
		for _, level := range []string{"fatal error", "error", "warning", "note", "style", "performance", "portability", "information"} {
			if remainder, ok := strings.CutPrefix(msg, level+": "); ok {
				msg = remainder
				switch level {
				case "fatal error", "error":
					severity = "error"
				case "note", "information":
					severity = "info"
				default:
					severity = "warning"
				}
				break
			}
		}
		var rule string
		if parts := messageRule.FindStringSubmatch(msg); parts != nil {
			msg, rule = parts[1], parts[2]
		} else if parts := ktlintRule.FindStringSubmatch(msg); parts != nil {
			msg, rule = parts[1], parts[2]
		}
		result = append(result, contract.Diagnostic{Severity: severity, File: file, Line: number(row), Column: number(col), Message: msg, RuleID: rule})
	}
	return result, nil
}

func parseTypeScript(stdout, stderr string) ([]contract.Diagnostic, error) {
	var result []contract.Diagnostic
	for _, line := range strings.Split(stdout+"\n"+stderr, "\n") {
		if p := typeScript.FindStringSubmatch(line); p != nil {
			result = append(result, contract.Diagnostic{Severity: "error", File: p[1], Line: number(p[2]), Column: number(p[3]), RuleID: p[4], Message: p[5]})
		}
	}
	return result, nil
}

func parseDart(stdout, stderr string) ([]contract.Diagnostic, error) {
	var result []contract.Diagnostic
	for _, line := range strings.Split(stdout+"\n"+stderr, "\n") {
		p := strings.SplitN(line, "|", 8)
		if len(p) == 8 && number(p[4]) > 0 {
			result = append(result, contract.Diagnostic{Severity: strings.ToLower(p[0]), RuleID: p[2], File: p[3], Line: number(p[4]), Column: number(p[5]), Message: p[7]})
		} else if match := flutterLine.FindStringSubmatch(line); match != nil {
			pos := location.FindStringSubmatch(match[3] + ": diagnostic")
			if pos != nil {
				result = append(result, contract.Diagnostic{Severity: match[1], Message: match[2], RuleID: match[4], File: pos[1], Line: number(pos[2]), Column: number(pos[3])})
			}
		}
	}
	return result, nil
}

func parseESLint(stdout, _ string) ([]contract.Diagnostic, error) {
	var output []struct {
		FilePath string
		Messages []struct {
			RuleID       string
			Severity     int
			Line, Column int
			Message      string
		}
	}
	if err := decode(stdout, &output); err != nil {
		return nil, err
	}
	var result []contract.Diagnostic
	for _, file := range output {
		for _, m := range file.Messages {
			level := "warning"
			if m.Severity == 2 {
				level = "error"
			}
			result = append(result, contract.Diagnostic{Severity: level, RuleID: m.RuleID, File: file.FilePath, Line: m.Line, Column: m.Column, Message: m.Message})
		}
	}
	return result, nil
}

func parseRuff(stdout, _ string) ([]contract.Diagnostic, error) {
	var output []struct {
		Filename, Code, Message string
		Location                struct{ Row, Column int }
	}
	if err := decode(stdout, &output); err != nil {
		return nil, err
	}
	var result []contract.Diagnostic
	for _, m := range output {
		result = append(result, contract.Diagnostic{Severity: "error", File: m.Filename, RuleID: m.Code, Line: m.Location.Row, Column: m.Location.Column, Message: m.Message})
	}
	return result, nil
}

func parsePyright(stdout, _ string) ([]contract.Diagnostic, error) {
	var output struct {
		GeneralDiagnostics []struct {
			File, Severity, Rule, Message string
			Range                         struct{ Start struct{ Line, Character int } }
		}
	}
	if err := decode(stdout, &output); err != nil {
		return nil, err
	}
	var result []contract.Diagnostic
	for _, m := range output.GeneralDiagnostics {
		result = append(result, contract.Diagnostic{Severity: m.Severity, File: m.File, RuleID: m.Rule, Line: m.Range.Start.Line + 1, Column: m.Range.Start.Character + 1, Message: m.Message})
	}
	return result, nil
}

func parseBiome(stdout, _ string) ([]contract.Diagnostic, error) {
	var output struct {
		Diagnostics []struct {
			Category, Severity, Description string
			Location                        struct {
				Path       struct{ File string }
				Span       []int
				SourceCode string
			}
		}
	}
	if err := decode(stdout, &output); err != nil {
		return nil, err
	}
	var result []contract.Diagnostic
	for _, m := range output.Diagnostics {
		d := contract.Diagnostic{Severity: m.Severity, File: m.Location.Path.File, RuleID: m.Category, Message: m.Description}
		if len(m.Location.Span) == 2 {
			offset := m.Location.Span[0]
			if offset >= 0 && offset <= len(m.Location.SourceCode) && m.Location.SourceCode != "" {
				before := m.Location.SourceCode[:offset]
				d.Line = 1 + strings.Count(before, "\n")
				d.Column = len(before) - strings.LastIndexByte(before, '\n')
			}
		}
		result = append(result, d)
	}
	return result, nil
}

func parseCargo(stdout, _ string) ([]contract.Diagnostic, error) {
	var result []contract.Diagnostic
	for _, line := range strings.Split(stdout, "\n") {
		if line == "" {
			continue
		}
		var output struct {
			Reason  string
			Message struct {
				Level, Message string
				Code           *struct{ Code string }
				Spans          []struct {
					Primary bool   `json:"is_primary"`
					File    string `json:"file_name"`
					Line    int    `json:"line_start"`
					Column  int    `json:"column_start"`
				}
			}
		}
		if err := decode(line, &output); err != nil {
			return nil, err
		}
		if output.Reason != "compiler-message" || output.Message.Level == "failure-note" {
			continue
		}
		m := output.Message
		d := contract.Diagnostic{Severity: m.Level, Message: m.Message}
		if m.Code != nil {
			d.RuleID = m.Code.Code
		}
		for _, span := range m.Spans {
			if span.Primary {
				d.File, d.Line, d.Column = filepath.FromSlash(span.File), span.Line, span.Column
				break
			}
		}
		result = append(result, d)
	}
	return result, nil
}

func decode(stdout string, output any) error {
	if err := json.Unmarshal([]byte(stdout), output); err != nil {
		return fmt.Errorf("malformed checker JSON: %w", err)
	}
	return nil
}
