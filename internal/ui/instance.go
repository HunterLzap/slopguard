package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	uiLockFileName     = "ui.lock"
	instanceStaleAfter = 2 * time.Minute
)

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

type instanceLockRecord struct {
	Version    int    `json:"version"`
	PID        int    `json:"pid"`
	Port       int    `json:"port"`
	InstanceID string `json:"instance_id"`
	StartedAt  string `json:"started_at"`
}

type instanceLock struct {
	path   string
	record instanceLockRecord
}

func instanceLockPath(home string) string {
	home = strings.TrimSpace(home)
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".slopguard", uiLockFileName)
}

// acquireInstanceLock enforces one dashboard process per user home. The lock
// file contains an instance id so a second process can verify that the local
// HTTP endpoint is the same SlopGuard process before attaching.
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

	for attempt := 0; attempt < 2; attempt++ {
		f, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr == nil {
			if _, writeErr := f.Write(encoded); writeErr != nil {
				_ = f.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("ui: write instance lock: %w", writeErr)
			}
			if syncErr := f.Sync(); syncErr != nil {
				_ = f.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("ui: sync instance lock: %w", syncErr)
			}
			if closeErr := f.Close(); closeErr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("ui: close instance lock: %w", closeErr)
			}
			_ = hardenSecretFile(path)
			return &instanceLock{path: path, record: record}, nil
		}
		if !os.IsExist(openErr) {
			return nil, fmt.Errorf("ui: create instance lock: %w", openErr)
		}

		existingRaw, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return nil, fmt.Errorf("ui: read instance lock: %w", readErr)
		}
		var existing instanceLockRecord
		parseOK := json.Unmarshal(existingRaw, &existing) == nil
		if !parseOK && existing.InstanceID == "" {
			if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) < instanceStaleAfter {
				return nil, errors.New("ui: instance lock is unreadable; retry after it becomes stale")
			}
			_ = os.Remove(path)
			continue
		}
		if probeDashboardIdentity(existing.Port, existing.InstanceID) {
			return nil, &AlreadyRunningError{
				URL:        fmt.Sprintf("http://127.0.0.1:%d/", existing.Port),
				Port:       existing.Port,
				PID:        existing.PID,
				InstanceID: existing.InstanceID,
			}
		}
		startedAt, _ := time.Parse(time.RFC3339, existing.StartedAt)
		age := time.Since(startedAt)
		if startedAt.IsZero() {
			if info, statErr := os.Stat(path); statErr == nil {
				age = time.Since(info.ModTime())
			}
		}
		if !processAlive(existing.PID) || age > instanceStaleAfter {
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				return nil, fmt.Errorf("ui: remove stale instance lock: %w", removeErr)
			}
			continue
		}
		return nil, fmt.Errorf("ui: another SlopGuard dashboard is starting (pid %d); retry shortly", existing.PID)
	}
	return nil, errors.New("ui: could not acquire instance lock")
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
	if err := os.WriteFile(l.path, encoded, 0o600); err != nil {
		return fmt.Errorf("ui: update instance lock: %w", err)
	}
	_ = hardenSecretFile(l.path)
	return nil
}

func (l *instanceLock) release() {
	if l == nil || l.path == "" {
		return
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var current instanceLockRecord
	if json.Unmarshal(raw, &current) != nil || current.InstanceID != l.record.InstanceID {
		return
	}
	_ = os.Remove(l.path)
}

func probeDashboardIdentity(port int, instanceID string) bool {
	if port < 1 || port > 65535 {
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
	if err := json.NewDecoder(resp.Body).Decode(&identity); err != nil {
		return false
	}
	if identity.Service != "slopguard" {
		return false
	}
	if strings.TrimSpace(instanceID) == "" {
		return true
	}
	return identity.InstanceID == instanceID
}
