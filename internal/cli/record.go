package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/store"
)

func runRecord(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projectFlag := fs.String("project", ".", "project path")
	fingerprint := fs.String("fingerprint", "", "mistake fingerprint (generated when omitted)")
	category := fs.String("category", "other", "mistake category")
	summary := fs.String("summary", "", "mistake summary (required)")
	badSnippet := fs.String("bad-snippet", "", "bad code snippet")
	goodSnippet := fs.String("good-snippet", "", "correct alternative snippet")
	fs.StringVar(badSnippet, "bad", "", "alias for --bad-snippet")
	fs.StringVar(goodSnippet, "good", "", "alias for --good-snippet")
	source := fs.String("source", "manual", "record source: manual | agent | git_fix")
	help, err := parseFlags(fs, args)
	if err != nil {
		return commandError(stderr, "%v", err)
	}
	if help {
		return ExitOK
	}
	if strings.TrimSpace(*summary) == "" {
		return commandError(stderr, "--summary is required")
	}
	project, err := filepath.Abs(*projectFlag)
	if err != nil {
		return commandError(stderr, "resolve project path: %v", err)
	}
	if info, err := os.Stat(project); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return commandError(stderr, "project path %s: %v", project, err)
	}

	fp := strings.TrimSpace(*fingerprint)
	if fp == "" {
		fp = mistakeFingerprint(project, *category, *summary)
	}
	db, err := store.OpenDefault()
	if err != nil {
		return commandError(stderr, "open mistake store: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	count, err := db.RecordMistake(ctx, contract.Mistake{
		ProjectPath: project,
		Fingerprint: fp,
		Category:    *category,
		Summary:     *summary,
		BadSnippet:  *badSnippet,
		GoodSnippet: *goodSnippet,
		Source:      *source,
	})
	if err != nil {
		return commandError(stderr, "record mistake: %v", err)
	}
	fmt.Fprintf(stdout, "Recorded mistake (fingerprint=%s, count=%d)\n", fp, count)
	return ExitOK
}

func mistakeFingerprint(project, category, summary string) string {
	h := sha256.Sum256([]byte(project + "\x00" + strings.TrimSpace(category) + "\x00" + strings.TrimSpace(summary)))
	return hex.EncodeToString(h[:])
}
