package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const uiLockFileName = "ui.lock"

// AlreadyRunningError means a healthy SlopGuard dashboard already owns the
// user's single-instance lock. Callers should attach to URL instead of starting
// a second server or rotating the auth token.
type AlreadyRunningError struct {
	URL        string
	Port       int
	PID        int
	InstanceID string
}

func (e *AlreadyRunningError) Error() string {
	if e == nil {
		return "SlopGuard dashboard is already running"
	}
	url := strings.TrimSpace(e.URL)
	if url == "" {
		url = fmt.Sprintf("http://127.0.0.1:%d/", e.Port)
	}
	return fmt.Sprintf("SlopGuard dashboard is already running at %s (pid %d)", url, e.PID)
}

// TrustedDashboard is the verified local dashboard endpoint. Its identity comes
// from the user's lock record and must match /api/identity on the same port.
type TrustedDashboard struct {
	URL        string
	Port       int
	PID        int
	InstanceID string
}

type instanceLockRecord struct {
	Version    int    `json:"version"`
	PID        int    `json:"pid"`
	Port       int    `json:"port"`
	InstanceID string `json:"instance_id"`
	StartedAt  string `json:"started_at"`
}

type instanceLock struct {
	path   string
	file   *os.File
	record instanceLockRecord
}

func instanceLockPath(home string) string {
	home = strings.TrimSpace(home)
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".slopguard", uiLockFileName)
}

// acquireInstanceLock enforces one dashboard process per user home with an
// operating-system-level exclusive lock. The lock file is persistent metadata;
// the OS releases the actual lock when the process exits, so age-based stale
// detection is neither needed nor safe.
func acquireInstanceLock(home string, port int) (*instanceLock, error) {
	path := instanceLockPath(home)
	if path == "" {
		return nil, errors.New("ui: cannot locate home directory for ui.lock")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ui: create state dir: %w", err)
	}
	if err := hardenSecretDir(dir); err != nil {
		return nil, fmt.Errorf("ui: protect state dir: %w", err)
	}

	record := instanceLockRecord{
		Version:    1,
		PID:        os.Getpid(),
		Port:       port,
		InstanceID: generateRandomToken(),
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')

	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if openErr != nil {
		return nil, fmt.Errorf("ui: open instance lock: %w", openErr)
	}

	if lockErr := lockFileExclusive(f); lockErr != nil {
		if !isFileLockBusy(lockErr) {
			_ = f.Close()
			return nil, fmt.Errorf("ui: lock instance file: %w", lockErr)
		}

		// Another process holds the OS lock. Its metadata is advisory only:
		// never remove the file or treat age as proof that the process is dead.
		existing, readErr := readInstanceLockRecord(f)
		_ = f.Close()
		if readErr != nil || strings.TrimSpace(existing.InstanceID) == "" {
			return nil, errors.New("ui: another SlopGuard dashboard is starting; retry shortly")
		}
		if probeDashboardIdentity(existing.Port, existing.InstanceID) {
			return nil, &AlreadyRunningError{
				URL:        fmt.Sprintf("http://127.0.0.1:%d/", existing.Port),
				Port:       existing.Port,
				PID:        existing.PID,
				InstanceID: existing.InstanceID,
			}
		}
		return nil, fmt.Errorf("ui: another SlopGuard dashboard is starting (pid %d); retry shortly", existing.PID)
	}

	if err := writeInstanceLockRecord(f, encoded); err != nil {
		_ = unlockFile(f)
		_ = f.Close()
		return nil, err
	}
	if err := hardenSecretFile(path); err != nil {
		_ = unlockFile(f)
		_ = f.Close()
		return nil, fmt.Errorf("ui: protect instance lock: %w", err)
	}
	return &instanceLock{path: path, file: f, record: record}, nil
}

func readInstanceLockRecord(f *os.File) (instanceLockRecord, error) {
	var record instanceLockRecord
	if f == nil {
		return record, errors.New("ui: missing instance lock file")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return record, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return record, err
	}
	if len(raw) == 0 {
		return record, errors.New("ui: empty instance lock")
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, err
	}
	return record, nil
}

func writeInstanceLockRecord(f *os.File, encoded []byte) error {
	if f == nil {
		return errors.New("ui: missing instance lock file")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("ui: seek instance lock: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("ui: truncate instance lock: %w", err)
	}
	if _, err := f.Write(encoded); err != nil {
		return fmt.Errorf("ui: write instance lock: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("ui: sync instance lock: %w", err)
	}
	return nil
}

func (l *instanceLock) setPort(port int) error {
	if l == nil {
		return nil
	}
	l.record.Port = port
	encoded, err := json.MarshalIndent(l.record, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if l.file == nil {
		return errors.New("ui: instance lock is not held")
	}
	if err := writeInstanceLockRecord(l.file, encoded); err != nil {
		return fmt.Errorf("ui: update instance lock: %w", err)
	}
	_ = hardenSecretFile(l.path)
	return nil
}

func (l *instanceLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unlockFile(l.file)
	_ = l.file.Close()
	l.file = nil
}

func probeDashboardIdentity(port int, instanceID string) bool {
	expected := strings.TrimSpace(instanceID)
	if port < 1 || port > 65535 || expected == "" {
		return false
	}
	client := &http.Client{Timeout: 600 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/identity", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var identity struct {
		Service    string `json:"service"`
		InstanceID string `json:"instance_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&identity); err != nil {
		return false
	}
	if identity.Service != "slopguard" {
		return false
	}
	return identity.InstanceID == expected
}

// ProbeTrustedDashboard verifies that ui.lock belongs to the dashboard
// listening on port and that the live /api/identity instance id matches it.
func ProbeTrustedDashboard(home string, port int) (TrustedDashboard, bool) {
	path := instanceLockPath(home)
	if path == "" || port < 1 || port > 65535 {
		return TrustedDashboard{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return TrustedDashboard{}, false
	}
	var record instanceLockRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return TrustedDashboard{}, false
	}
	if record.Port != port || strings.TrimSpace(record.InstanceID) == "" {
		return TrustedDashboard{}, false
	}
	if !probeDashboardIdentity(port, record.InstanceID) {
		return TrustedDashboard{}, false
	}
	return TrustedDashboard{
		URL:        fmt.Sprintf("http://127.0.0.1:%d/", port),
		Port:       port,
		PID:        record.PID,
		InstanceID: record.InstanceID,
	}, true
}
