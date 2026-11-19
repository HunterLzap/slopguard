package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestP0RequestDashboardTicketUsesHeaderNotURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	token := strings.Repeat("0123456789abcdef", 4)
	if err := os.MkdirAll(filepath.Join(home, ".slopguard"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".slopguard", "ui-token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var sawTokenHeader bool
	instanceID := "ticket-instance"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/identity":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"service":     "slopguard",
				"instance_id": instanceID,
			})
			return
		case "/api/auth/ticket":
			if r.Method != http.MethodPost {
				http.Error(w, "unexpected method", http.StatusBadRequest)
				return
			}
			if strings.Contains(r.URL.String(), token) {
				t.Errorf("long-term token leaked into ticket request URL: %s", r.URL.String())
			}
			for _, values := range r.Header {
				for _, value := range values {
					if value == token {
						sawTokenHeader = true
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"ok":     "true",
				"ticket": "ticket-p0",
				"url":    "/?ticket=ticket-p0",
			})
			return
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	lockRaw, err := json.Marshal(map[string]any{"port": port, "instance_id": instanceID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".slopguard", "ui.lock"), lockRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	ticketURL, err := requestDashboardTicket(server.URL)
	if err != nil {
		t.Fatalf("requestDashboardTicket: %v", err)
	}
	if !sawTokenHeader {
		t.Fatal("long-term token was not sent as an HTTP header")
	}
	if strings.Contains(ticketURL, token) {
		t.Fatalf("long-term token leaked into returned URL: %s", ticketURL)
	}
	if ticketURL != server.URL+"/?ticket=ticket-p0" {
		t.Fatalf("ticket URL = %q", ticketURL)
	}
}

func TestP0TrustedDashboardIdentityMatchesLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	if err := os.MkdirAll(filepath.Join(home, ".slopguard"), 0o700); err != nil {
		t.Fatal(err)
	}

	identityServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/identity" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service":     "slopguard",
			"instance_id": "lock-instance",
		})
	}))
	defer identityServer.Close()
	port := identityServer.Listener.Addr().(*net.TCPAddr).Port

	writeLock := func(instanceID string) {
		raw, _ := json.Marshal(map[string]any{"port": port, "instance_id": instanceID})
		if err := os.WriteFile(filepath.Join(home, ".slopguard", "ui.lock"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeLock("lock-instance")
	if !isTrustedDashboardResponding(port) {
		t.Fatal("matching lock instance must be trusted")
	}
	writeLock("different-instance")
	if isTrustedDashboardResponding(port) {
		t.Fatal("mismatched instance must not be trusted")
	}
}
