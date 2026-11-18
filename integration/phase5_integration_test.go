package integration_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/HunterLzap/slopguard/internal/store"
)

func TestPhase5VerifyRecordFailureWritesScanRun(t *testing.T) {
	home := t.TempDir()
	root := copyFixture(t, "typescript-repo")
	installTSStubs(t, root)
	env := map[string]string{"HOME": home, "USERPROFILE": home}
	if result := runCLIWithInput(t, root, env, "", "init", "--root", root); result.Code != 0 {
		t.Fatalf("init exit = %d: %s", result.Code, result.Stderr)
	}
	overrides := map[string]string{"HOME": home, "USERPROFILE": home, "FAKE_TSC_FAIL": "1"}
	result := runCLIWithInput(t, root, overrides, "", "verify", "--all", "--record", "--json", "--root", root)
	if result.Code != 1 {
		t.Fatalf("verify failure exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", result.Code, result.Stdout, result.Stderr)
	}

	db, err := store.Open(filepath.Join(home, ".slopguard", "slopguard.db"))
	if err != nil {
		t.Fatalf("open recorded store: %v", err)
	}
	defer db.Close()
	overview, err := db.OverviewStats(context.Background(), "", "day")
	if err != nil {
		t.Fatalf("OverviewStats: %v", err)
	}
	if overview.TotalRuns != 1 || overview.FailedRuns != 1 {
		t.Fatalf("overview = %+v, want one failed run recorded", overview)
	}
}
