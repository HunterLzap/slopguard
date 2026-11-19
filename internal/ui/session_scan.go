package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/engine"
)

const (
	maxSessionFilesPerSource = 15
	maxSessionWalkEntries    = 4000
	maxSessionReadBytes      = 2 << 20 // 2MB hard cap per log file
	defaultSessionScanDays   = 14
	maxSessionScanItems      = 10
)

type sessionScanCandidate struct {
	ProjectPath string `json:"project_path"`
	Fingerprint string `json:"fingerprint"`
	Category    string `json:"category"`
	Summary     string `json:"summary"`
	BadSnippet  string `json:"bad_snippet"`
	Source      string `json:"source"`
	OccurredAt  string `json:"occurred_at,omitempty"`
	OriginFile  string `json:"origin_file,omitempty"`
}

type sessionScanCounts struct {
	GitCommits       int `json:"git_commits"`
	ClaudeFiles      int `json:"claude_files"`
	CodexFiles       int `json:"codex_files"`
	AntigravityFiles int `json:"antigravity_files"`
}

type sessionScanResponse struct {
	OK       bool                   `json:"ok"`
	Project  string                 `json:"project"`
	DryRun   bool                   `json:"dry_run"`
	Days     int                    `json:"days"`
	Scanned  sessionScanCounts      `json:"scanned"`
	Found    int                    `json:"found"`
	Imported int                    `json:"imported"`
	AIQueued bool                   `json:"ai_queued"`
	Items    []sessionScanCandidate `json:"items"`
}

func (s *Server) handleScanSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var req struct {
		Project string   `json:"project"`
		Days    int      `json:"days"`
		DryRun  bool     `json:"dry_run"`
		Sources []string `json:"sources"`
	}
	if !decodeJSONBody(w, r, &req, true) {
		return
	}
	project := strings.TrimSpace(req.Project)
	if project != "" {
		abs, err := filepath.Abs(project)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("无效项目路径: %v", err))
			return
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			writeError(w, http.StatusBadRequest, "所选项目目录不存在")
			return
		}
		project = abs
	}
	days := req.Days
	if days <= 0 {
		days = defaultSessionScanDays
	}
	if days > 90 {
		days = 90
	}

	resp := s.scanLocalSessionsAndRework(r.Context(), project, days, req.DryRun, req.Sources)
	if !resp.DryRun && resp.Imported > 0 && s.ai != nil {
		st := s.ai.Status()
		if st.AutoAnalyze && st.ActiveRunner != "" {
			resp.AIQueued = true
			s.ai.TriggerAsync(project)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) scanLocalSessionsAndRework(ctx context.Context, project string, days int, dryRun bool, sources []string) sessionScanResponse {
	allow := make(map[string]bool)
	if len(sources) == 0 {
		allow["git_fix"] = true
		allow["claude"] = true
		allow["codex"] = true
		allow["antigravity"] = true
	} else {
		for _, src := range sources {
			allow[strings.ToLower(strings.TrimSpace(src))] = true
		}
	}

	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	home := s.resolveHomeDir()
	runner := s.runner
	if runner == nil {
		runner = engine.NewExecRunner()
	}

	var counts sessionScanCounts
	var candidates []sessionScanCandidate
	seenFP := make(map[string]struct{})
	var knownProjects []string
	if project == "" && s.db != nil {
		if listed, err := s.db.ListProjects(ctx); err == nil {
			for _, p := range listed {
				if p.ProjectPath != "" {
					knownProjects = append(knownProjects, p.ProjectPath)
				}
			}
		}
	}
	addCandidate := func(c sessionScanCandidate) {
		if len(candidates) >= maxSessionScanItems {
			return
		}
		c.Summary = strings.TrimSpace(c.Summary)
		if c.Summary == "" {
			return
		}
		if c.ProjectPath == "" {
			c.ProjectPath = project
		}
		if c.ProjectPath == "" {
			return
		}
		c.ProjectPath = filepath.Clean(c.ProjectPath)
		if c.Category == "" {
			c.Category = "lint"
		}
		if c.Fingerprint == "" {
			c.Fingerprint = mistakeFingerprintUI(c.ProjectPath, c.Category, c.Summary)
		}
		if _, dup := seenFP[c.Fingerprint]; dup {
			return
		}
		seenFP[c.Fingerprint] = struct{}{}
		candidates = append(candidates, c)
	}

	if allow["git_fix"] {
		var gitProjects []string
		if project != "" {
			gitProjects = []string{project}
		} else {
			for _, kp := range knownProjects {
				if len(gitProjects) >= 5 {
					break
				}
				gitProjects = append(gitProjects, kp)
			}
		}
		for _, gp := range gitProjects {
			if len(candidates) >= maxSessionScanItems {
				break
			}
			commits, items := scanGitFixRework(ctx, runner, gp, days)
			counts.GitCommits += commits
			for _, item := range items {
				addCandidate(item)
			}
		}
	}

	if home != "" && ctx.Err() == nil {
		if allow["claude"] && len(candidates) < maxSessionScanItems {
			files := collectRecentFiles(ctx, filepath.Join(home, ".claude", "projects"), ".jsonl", cutoff, maxSessionFilesPerSource)
			counts.ClaudeFiles = len(files)
			for _, f := range files {
				if ctx.Err() != nil || len(candidates) >= maxSessionScanItems {
					break
				}
				for _, item := range scanClaudeJSONL(f, project) {
					addCandidate(item)
				}
			}
		}
		if allow["codex"] && len(candidates) < maxSessionScanItems && ctx.Err() == nil {
			files := collectRecentFiles(ctx, filepath.Join(home, ".codex", "sessions"), ".jsonl", cutoff, maxSessionFilesPerSource)
			counts.CodexFiles = len(files)
			for _, f := range files {
				if ctx.Err() != nil || len(candidates) >= maxSessionScanItems {
					break
				}
				for _, item := range scanCodexJSONL(f, project) {
					addCandidate(item)
				}
			}
		}
		if allow["antigravity"] && len(candidates) < maxSessionScanItems && ctx.Err() == nil {
			files := collectAntigravityTranscripts(ctx, filepath.Join(home, ".gemini", "antigravity", "brain"), cutoff, maxSessionFilesPerSource)
			counts.AntigravityFiles = len(files)
			for _, f := range files {
				if ctx.Err() != nil || len(candidates) >= maxSessionScanItems {
					break
				}
				for _, item := range scanAntigravityTranscript(f, project, knownProjects) {
					addCandidate(item)
				}
			}
		}
	}

	imported := 0
	if !dryRun && s.db != nil {
		for _, item := range candidates {
			_, err := s.db.RecordMistake(ctx, contract.Mistake{
				ProjectPath: item.ProjectPath,
				Fingerprint: item.Fingerprint,
				Category:    item.Category,
				Summary:     item.Summary,
				BadSnippet:  item.BadSnippet,
				Source:      item.Source,
				Status:      "pending",
			})
			if err == nil {
				imported++
			}
		}
	}

	if candidates == nil {
		candidates = []sessionScanCandidate{}
	}
	return sessionScanResponse{
		OK:       true,
		Project:  project,
		DryRun:   dryRun,
		Days:     days,
		Scanned:  counts,
		Found:    len(candidates),
		Imported: imported,
		Items:    candidates,
	}
}

var gitFixSubjectPattern = regexp.MustCompile(`(?i)(^|[^a-z0-9])(fix|fixes|fixed|bug|bugfix|hotfix|revert)([^a-z0-9]|$)|修复|修正|回滚|修补`)

func scanGitFixRework(ctx context.Context, runner contract.Runner, project string, days int) (int, []sessionScanCandidate) {
	if project == "" {
		return 0, nil
	}
	info, err := os.Stat(project)
	if err != nil || !info.IsDir() {
		return 0, nil
	}
	git := engine.NewGit(runner, project)
	if !git.IsRepository(ctx) {
		return 0, nil
	}
	res := runner.Run(ctx, contract.RunSpec{
		Name: "git",
		Args: []string{
			"log",
			fmt.Sprintf("--since=%d.days", days),
			"-n", "40",
			"--name-only",
			"--pretty=format:%H\t%aI\t%s",
		},
		Dir:     project,
		Timeout: 10 * time.Second,
	})
	if res.Err != nil || res.Skipped || res.ExitCode != 0 {
		return 0, nil
	}

	type commitEntry struct {
		hash    string
		date    string
		subject string
		files   []string
	}
	var commits []commitEntry
	var current *commitEntry

	for _, rawLine := range strings.Split(strings.ReplaceAll(res.Stdout, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) == 3 && len(parts[0]) >= 7 {
			commits = append(commits, commitEntry{
				hash:    parts[0],
				date:    parts[1],
				subject: parts[2],
			})
			current = &commits[len(commits)-1]
			continue
		}
		if current != nil {
			current.files = append(current.files, line)
		}
	}

	var out []sessionScanCandidate
	for _, c := range commits {
		if !gitFixSubjectPattern.MatchString(c.subject) {
			continue
		}
		subj := c.subject
		if len([]rune(subj)) > 110 {
			r := []rune(subj)
			subj = string(r[:110]) + "..."
		}
		shortHash := c.hash
		if len(shortHash) > 8 {
			shortHash = shortHash[:8]
		}
		fileList := c.files
		if len(fileList) > 5 {
			fileList = fileList[:5]
		}
		snippet := shortHash
		if len(fileList) > 0 {
			snippet = shortHash + " · " + strings.Join(fileList, ", ")
		}
		out = append(out, sessionScanCandidate{
			ProjectPath: project,
			Category:    inferReworkCategory(c.subject, fileList),
			Summary:     "[Git 返工] " + subj,
			BadSnippet:  truncateText(snippet, 280),
			Source:      "git_fix",
			OccurredAt:  c.date,
		})
	}
	return len(commits), out
}

func inferReworkCategory(subject string, files []string) string {
	low := strings.ToLower(subject + " " + strings.Join(files, " "))
	switch {
	case strings.Contains(low, "type") || strings.Contains(low, "ts2") || strings.Contains(low, "mypy") || strings.Contains(low, "pyright"):
		return "type"
	case strings.Contains(low, "import") || strings.Contains(low, "module"):
		return "import"
	case strings.Contains(low, "lint") || strings.Contains(low, "eslint") || strings.Contains(low, "ruff"):
		return "lint"
	case strings.Contains(low, "syntax") || strings.Contains(low, "parse"):
		return "syntax"
	case strings.Contains(low, "ui") || strings.Contains(low, "css") || strings.Contains(low, "layout"):
		return "ui"
	default:
		return "rework"
	}
}

type recentFile struct {
	path    string
	modTime time.Time
}

func collectRecentFiles(ctx context.Context, root, suffix string, cutoff time.Time, limit int) []string {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}
	var found []recentFile
	walked := 0
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, wErr error) error {
		if ctx != nil && ctx.Err() != nil {
			return fs.SkipAll
		}
		if walked >= maxSessionWalkEntries {
			return fs.SkipAll
		}
		walked++
		if wErr != nil {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(entry.Name()), suffix) {
			return nil
		}
		fi, statErr := entry.Info()
		if statErr != nil || !fi.Mode().IsRegular() || fi.ModTime().Before(cutoff) {
			return nil
		}
		found = append(found, recentFile{path: path, modTime: fi.ModTime()})
		return nil
	})
	sort.Slice(found, func(i, j int) bool {
		return found[i].modTime.After(found[j].modTime)
	})
	if len(found) > limit {
		found = found[:limit]
	}
	paths := make([]string, len(found))
	for i, f := range found {
		paths[i] = f.path
	}
	return paths
}

func collectAntigravityTranscripts(ctx context.Context, brainDir string, cutoff time.Time, limit int) []string {
	entries, err := os.ReadDir(brainDir)
	if err != nil {
		return nil
	}
	var found []recentFile
	for i, entry := range entries {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		if i >= maxSessionWalkEntries {
			break
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		p := filepath.Join(brainDir, entry.Name(), ".system_generated", "logs", "transcript.jsonl")
		fi, statErr := os.Lstat(p)
		if statErr != nil || !fi.Mode().IsRegular() || fi.ModTime().Before(cutoff) {
			continue
		}
		found = append(found, recentFile{path: p, modTime: fi.ModTime()})
	}
	sort.Slice(found, func(i, j int) bool {
		return found[i].modTime.After(found[j].modTime)
	})
	if len(found) > limit {
		found = found[:limit]
	}
	paths := make([]string, len(found))
	for i, f := range found {
		paths[i] = f.path
	}
	return paths
}

func readBoundedLines(path string, maxBytes int64) []string {
	fi, lErr := os.Lstat(path)
	if lErr != nil || !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	reader := bufio.NewReader(io.LimitReader(f, maxBytes))
	var lines []string
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			lines = append(lines, trimmed)
		}
		if err != nil {
			break
		}
	}
	return lines
}

func projectMatchesPath(targetProject, candidate string) bool {
	if targetProject == "" {
		return true
	}
	if candidate == "" {
		return false
	}
	t := filepath.Clean(targetProject)
	c := filepath.Clean(candidate)
	return c == t || strings.HasPrefix(c, t+string(filepath.Separator))
}

type sessionHit struct {
	text string
	ts   string
}

func scanClaudeJSONL(path, targetProject string) []sessionScanCandidate {
	lines := readBoundedLines(path, maxSessionReadBytes)
	if len(lines) == 0 {
		return nil
	}
	var sessionCwd string
	var hits []sessionHit
	projectSeenInFile := false

	for _, line := range lines {
		if targetProject != "" && strings.Contains(line, targetProject) {
			projectSeenInFile = true
		}
		if !strings.Contains(line, `"tool_result"`) && (sessionCwd != "" || !strings.Contains(line, `"cwd"`)) {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		if cwd, _ := obj["cwd"].(string); cwd != "" && sessionCwd == "" {
			sessionCwd = cwd
		}
		ts, _ := obj["timestamp"].(string)

		if topType, _ := obj["type"].(string); topType == "tool_result" {
			if isErr, _ := obj["is_error"].(bool); isErr {
				if text := extractAnyText(obj["content"]); text != "" {
					hits = append(hits, sessionHit{text: text, ts: ts})
				}
			}
		}
		if msg, ok := obj["message"].(map[string]any); ok {
			if items, ok := msg["content"].([]any); ok {
				for _, item := range items {
					m, ok := item.(map[string]any)
					if !ok {
						continue
					}
					if t, _ := m["type"].(string); t == "tool_result" {
						isErr, _ := m["is_error"].(bool)
						text := extractAnyText(m["content"])
						if text != "" && (isErr || hasErrorIndicator(text)) {
							hits = append(hits, sessionHit{text: text, ts: ts})
						}
					}
				}
			}
		}
	}

	if targetProject != "" && !projectMatchesPath(targetProject, sessionCwd) && !projectSeenInFile {
		return nil
	}
	proj := sessionCwd
	if targetProject != "" {
		proj = targetProject
	}
	if proj == "" {
		return nil
	}
	return buildCandidatesFromHits(hits, proj, "session_claude", filepath.Base(path))
}

func scanCodexJSONL(path, targetProject string) []sessionScanCandidate {
	lines := readBoundedLines(path, maxSessionReadBytes)
	if len(lines) == 0 {
		return nil
	}
	var sessionCwd string
	var hits []sessionHit
	projectSeenInFile := false

	for _, line := range lines {
		if targetProject != "" && strings.Contains(line, targetProject) {
			projectSeenInFile = true
		}
		needsParse := (sessionCwd == "" && (strings.Contains(line, `"session_meta"`) || strings.Contains(line, `"turn_context"`))) ||
			strings.Contains(line, `"function_call_output"`) ||
			strings.Contains(line, `"custom_tool_call_output"`) ||
			strings.Contains(line, `"item_completed"`)
		if !needsParse {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		topType, _ := obj["type"].(string)
		ts, _ := obj["timestamp"].(string)
		payload, _ := obj["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		switch topType {
		case "session_meta", "turn_context":
			if cwd, _ := payload["cwd"].(string); cwd != "" && sessionCwd == "" {
				sessionCwd = cwd
			}
		case "response_item":
			pt, _ := payload["type"].(string)
			if pt == "function_call_output" || pt == "custom_tool_call_output" {
				out := extractAnyText(payload["output"])
				if out != "" && hasErrorIndicator(out) {
					hits = append(hits, sessionHit{text: out, ts: ts})
				}
			}
		case "event_msg":
			if pt, _ := payload["type"].(string); pt == "item_completed" {
				if out := extractAnyText(payload["item"]); out != "" && hasErrorIndicator(out) {
					hits = append(hits, sessionHit{text: out, ts: ts})
				}
			}
		}
	}

	if targetProject != "" && !projectMatchesPath(targetProject, sessionCwd) && !projectSeenInFile {
		return nil
	}
	proj := sessionCwd
	if targetProject != "" {
		proj = targetProject
	}
	if proj == "" {
		return nil
	}
	return buildCandidatesFromHits(hits, proj, "session_codex", filepath.Base(path))
}

func scanAntigravityTranscript(path, targetProject string, knownProjects []string) []sessionScanCandidate {
	lines := readBoundedLines(path, maxSessionReadBytes)
	if len(lines) == 0 {
		return nil
	}
	var hits []sessionHit
	projectSeen := targetProject == ""
	inferredProject := targetProject

	for _, line := range lines {
		if targetProject != "" {
			if strings.Contains(line, targetProject) {
				projectSeen = true
			}
		} else if inferredProject == "" {
			for _, kp := range knownProjects {
				if kp != "" && strings.Contains(line, kp) {
					inferredProject = kp
					break
				}
			}
		}
		if !strings.Contains(line, `"ERROR"`) && !hasErrorIndicator(line) {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		status, _ := obj["status"].(string)
		ts, _ := obj["created_at"].(string)
		content, _ := obj["content"].(string)
		if content != "" && (strings.EqualFold(status, "ERROR") || hasErrorIndicator(content)) {
			hits = append(hits, sessionHit{text: content, ts: ts})
		}
	}

	if !projectSeen || inferredProject == "" {
		return nil
	}
	return buildCandidatesFromHits(hits, inferredProject, "session_antigravity", filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path)))))
}

func buildCandidatesFromHits(hits []sessionHit, project, source, origin string) []sessionScanCandidate {
	var out []sessionScanCandidate
	for _, h := range hits {
		if len(out) >= 4 {
			break
		}
		cat, summary, snippet := extractHighSignalError(h.text)
		if summary == "" {
			continue
		}
		out = append(out, sessionScanCandidate{
			ProjectPath: project,
			Category:    cat,
			Summary:     summary,
			BadSnippet:  snippet,
			Source:      source,
			OccurredAt:  h.ts,
			OriginFile:  origin,
		})
	}
	return out
}

func extractAnyText(val any) string {
	switch v := val.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if s := extractAnyText(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, k := range []string{"text", "output", "stderr", "stdout", "message", "content"} {
			if inner, ok := v[k]; ok {
				if s := extractAnyText(inner); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func hasErrorIndicator(text string) bool {
	low := strings.ToLower(text)
	if strings.Contains(low, "command exited with code 0") && !strings.Contains(low, "error ts") && !strings.Contains(low, "--- fail:") {
		return false
	}
	return strings.Contains(low, "error") ||
		strings.Contains(low, "fail") ||
		strings.Contains(low, "exception") ||
		strings.Contains(low, "syntaxerror") ||
		strings.Contains(low, "typeerror") ||
		strings.Contains(low, "undefined:") ||
		strings.Contains(low, "exited with code ")
}

type signalRule struct {
	category string
	re       *regexp.Regexp
}

var (
	highSignalRules = []signalRule{
		{category: "type", re: regexp.MustCompile(`error TS\d{4}:\s*.+`)},
		{category: "type", re: regexp.MustCompile(`\bTypeError:\s*.+`)},
		{category: "syntax", re: regexp.MustCompile(`\bSyntaxError:\s*.+`)},
		{category: "import", re: regexp.MustCompile(`\b(?:ImportError|ModuleNotFoundError|ReferenceError|NameError):\s*.+`)},
		{category: "lint", re: regexp.MustCompile(`\b(?:[EFWB]\d{3,4}|UP\d{3})\s+\S.*`)},
		{category: "lint", re: regexp.MustCompile(`^\s*\d+:\d+\s+error\s+.+\s+[a-z0-9@/_-]+$`)},
		{category: "type", re: regexp.MustCompile(`[^\s:]+\.go:\d+(?::\d+)?:\s*.+`)},
		{category: "type", re: regexp.MustCompile(`\bundefined:\s+\S+`)},
		{category: "test", re: regexp.MustCompile(`^---\s+FAIL:\s+\S+.*`)},
		{category: "type", re: regexp.MustCompile(`error\[E\d{4}\]:\s*.+`)},
	}
	noisePattern        = regexp.MustCompile(`(?i)(permission denied|econnrefused|timed out|timeout|rate limit|404 not found|canceled|cancelled|encountered error in step execution|declaring permissions:|invalid line range:|target content not found)`)
	lineColNormPattern  = regexp.MustCompile(`:\d+(?::\d+)?`)
)

func extractHighSignalError(text string) (category, summary, snippet string) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || len(line) < 8 || noisePattern.MatchString(line) {
			continue
		}
		for _, rule := range highSignalRules {
			loc := rule.re.FindString(line)
			if loc == "" {
				continue
			}
			norm := lineColNormPattern.ReplaceAllString(loc, "")
			norm = truncateText(strings.TrimSpace(norm), 140)
			if norm == "" {
				continue
			}
			return rule.category, norm, truncateText(line, 280)
		}
	}
	return "", "", ""
}

func truncateText(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}
