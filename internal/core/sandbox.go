package core

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/HunterLzap/slopguard/internal/core/adapters"
)

type Snippet struct {
	Language string `json:"language"`
	File     string `json:"file"`
	Code     string `json:"code"`
}

type SandboxReport struct {
	ProofHash           string
	PositiveHits        int
	ProjectHits         int
	ProjectScannedFiles int
	ProjectHitFiles     int
	DurationMs          int64
}

func SandboxProofHash(ruleYAML string, positiveHits, projectHits int) string {
	data := fmt.Sprintf("%s|%d|%d", ruleYAML, positiveHits, projectHits)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(data)))
}

func (s Semgrep) VerifyRuleInSandbox(ctx context.Context, project, ruleYAML string, positives []Snippet) (report SandboxReport, err error) {
	start := time.Now()
	defer func() { report.DurationMs = time.Since(start).Milliseconds() }()
	bin, err := s.Runner.LookPath(SemgrepTool.Name)
	if err != nil {
		return report, fmt.Errorf("semgrep is required (%s): %w", SemgrepTool.InstallCommand, err)
	}
	dir, err := os.MkdirTemp("", "slopguard-sandbox-*")
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	rule := filepath.Join(dir, "rule.yaml")
	if err := WriteFileAtomic(rule, []byte(ruleYAML), 0600, false, nil); err != nil {
		return report, err
	}
	if err := s.validate(ctx, bin, rule); err != nil {
		return report, err
	}
	samplesDir := filepath.Join(dir, "samples")
	var samples []string
	for i, sample := range positives {
		if sample.Code == "" {
			continue
		}
		ext := adapters.SampleExtension(sample.Language)
		if language, ok := adapters.Match(sample.File); ok && language.ID == sample.Language && language.Source(sample.File) {
			ext = filepath.Ext(sample.File)
		}
		name := fmt.Sprintf("sample-%d%s", i, ext)
		if err := WriteFileAtomic(filepath.Join(samplesDir, name), []byte(sample.Code), 0600, false, nil); err != nil {
			return report, err
		}
		samples = append(samples, name)
	}
	positive, err := s.scan(ctx, bin, samplesDir, []string{rule}, samples)
	if err != nil {
		return report, fmt.Errorf("scan positive samples: %w", err)
	}
	report.PositiveHits = len(positive.Diagnostics)
	if report.PositiveHits == 0 {
		return report, fmt.Errorf("sandbox rule matched no positive samples")
	}
	files, err := treeFiles(project)
	if err != nil {
		return report, err
	}
	targets, err := s.scan(ctx, bin, project, []string{rule}, files)
	if err != nil {
		return report, fmt.Errorf("scan project in sandbox: %w", err)
	}
	hitFiles := make(map[string]bool)
	for _, hit := range targets.Diagnostics {
		hitFiles[hit.File] = true
	}
	report.ProjectHits = len(targets.Diagnostics)
	report.ProjectScannedFiles = len(targets.ScannedFiles)
	report.ProjectHitFiles = len(hitFiles)
	if report.ProjectHits > 20 && report.ProjectHitFiles*100 > report.ProjectScannedFiles*30 {
		return report, fmt.Errorf("sandbox rule is too broad: %d hits across %d of %d scanned files", report.ProjectHits, report.ProjectHitFiles, report.ProjectScannedFiles)
	}
	report.ProofHash = SandboxProofHash(ruleYAML, report.PositiveHits, report.ProjectHits)
	return report, nil
}
