package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestProcessFixture(t *testing.T) {
	switch os.Getenv("SLOPGUARD_PROCESS_FIXTURE") {
	case "output":
		fmt.Fprint(os.Stdout, "one\r\ntwo\n")
		fmt.Fprint(os.Stderr, "three\r\n")
		os.Exit(7)
	case "timeout":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
}

func TestProcessOutput(t *testing.T) {
	t.Setenv("SLOPGUARD_PROCESS_FIXTURE", "output")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := (ExecRunner{}).Run(context.Background(), t.TempDir(), bin, "-test.run=^TestProcessFixture$")
	if err != nil || r.ExitCode != 7 || r.Stdout != "one\ntwo\n" || r.Stderr != "three\n" {
		t.Fatalf("run = %+v, %v", r, err)
	}
}

func TestProcessTimeout(t *testing.T) {
	t.Setenv("SLOPGUARD_PROCESS_FIXTURE", "timeout")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = (ExecRunner{}).Run(ctx, t.TempDir(), bin, "-test.run=^TestProcessFixture$")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error = %v", err)
	}
}
