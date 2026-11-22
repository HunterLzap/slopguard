package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsBatchShim(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	path := filepath.Join(dir, "slopguard-fixture.cmd")
	if err := os.WriteFile(path, []byte("@echo off\r\necho %~1\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := ExecRunner{}
	bin, err := runner.LookPath("slopguard-fixture")
	if err != nil {
		t.Fatal(err)
	}
	got, err := runner.Run(context.Background(), dir, bin, "spaces work")
	if err != nil || got.ExitCode != 0 || got.Stdout != "spaces work\n" {
		t.Fatalf("batch result = %+v, %v", got, err)
	}
	if _, err := runner.Run(context.Background(), dir, bin, "%PATH%"); err == nil {
		t.Fatal("allowed expansion in batch argument")
	}
}
