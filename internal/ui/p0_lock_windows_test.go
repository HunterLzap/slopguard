//go:build windows

package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestP0WindowsLockExclusiveAndMetadataReadable(t *testing.T) {
	if os.Getenv("SLOPG_SG_WIN_LOCK_CHILD") == "1" {
		home := os.Getenv("SLOPG_SG_WIN_LOCK_HOME")
		lock, err := acquireInstanceLock(home, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println("LOCKED")
		for {
			time.Sleep(time.Hour)
			_ = lock
		}
	}

	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestP0WindowsLockExclusiveAndMetadataReadable")
	cmd.Env = append(os.Environ(),
		"SLOPG_SG_WIN_LOCK_CHILD=1",
		"SLOPG_SG_WIN_LOCK_HOME="+home,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock child: %v", err)
	}
	locked := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "LOCKED" {
				close(locked)
				return
			}
		}
	}()
	select {
	case <-locked:
	case <-time.After(8 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("lock child did not acquire lock")
	}

	lockPath := instanceLockPath(home)
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("ui.lock metadata must remain readable while the byte-range lock is held: %v", err)
	}
	var record instanceLockRecord
	if err := json.Unmarshal(raw, &record); err != nil || strings.TrimSpace(record.InstanceID) == "" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("ui.lock metadata invalid: %s err=%v", raw, err)
	}

	if second, err := acquireInstanceLock(home, p0FreePort(t)); err == nil {
		second.release()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("second process acquired the lock while the first held it")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill lock child: %v", err)
	}
	_ = cmd.Wait()
	lock, err := acquireInstanceLock(home, p0FreePort(t))
	if err != nil {
		t.Fatalf("lock was not released after the holder exited: %v", err)
	}
	lock.release()
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("ui.lock metadata should remain after release: %v", err)
	}
}
