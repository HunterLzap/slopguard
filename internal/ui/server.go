package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/HunterLzap/slopguard/internal/adapter"
	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/radar"
	"github.com/HunterLzap/slopguard/internal/store"
)

//go:embed assets
var assets embed.FS

// Options controls the local dashboard server.
type Options struct {
	// Port is the local TCP port; 0 means the default 7788.
	Port int
	// NoOpen disables opening the system browser.
	NoOpen bool
}

// Server exposes the read-only dashboard API and embedded single-page app.
type Server struct {
	db     *store.Store
	runner contract.Runner
	mux    *http.ServeMux
}

// NewServer creates a dashboard server backed by the given store and runner.
func NewServer(db *store.Store, runner contract.Runner) *Server {
	s := &Server{db: db, runner: runner}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(mustSubFS(assets, "assets")))))
	mux.HandleFunc("/api/overview", s.handleOverview)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/mistakes", s.handleMistakes)
	mux.HandleFunc("/api/mistakes/", s.handleMistakeAction)
	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/radar", s.handleRadar)
	mux.HandleFunc("/api/screenshot", s.handleScreenshot)
	s.mux = mux
	return s
}

// Handler returns the dashboard HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Run starts the local dashboard on 127.0.0.1 and blocks until ctx is cancelled
// or the server stops. The browser is opened unless Options.NoOpen is set.
func Run(ctx context.Context, db *store.Store, runner contract.Runner, opts Options, stdout io.Writer) error {
	if db == nil {
		return errors.New("ui: nil store")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Port == 0 {
		opts.Port = 7788
	}
	if opts.Port < 1 || opts.Port > 65535 {
		return fmt.Errorf("ui: port out of range: %d", opts.Port)
	}
	server := NewServer(db, runner)
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opts.Port))
	if err != nil {
		return fmt.Errorf("ui: listen 127.0.0.1:%d: %w", opts.Port, err)
	}
	defer listener.Close()

	address := listener.Addr().String()
	url := "http://" + address + "/"
	if stdout != nil {
		fmt.Fprintf(stdout, "SlopGuard dashboard: %s\n", url)
	}
	if !opts.NoOpen {
		if err := openBrowser(url); err != nil && stdout != nil {
			fmt.Fprintf(stdout, "slopguard: warning: cannot open browser: %v\n", err)
		}
	}

	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("ui: shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func mustSubFS(source fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(source, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "dashboard asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	overview, err := s.db.OverviewStats(r.Context(), r.URL.Query().Get("project"), r.URL.Query().Get("granularity"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	projects, err := s.db.ListProjects(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) handleMistakes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	records, err := s.db.ListMistakes(r.Context(), r.URL.Query().Get("status"), r.URL.Query().Get("project"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mistakes": records})
}

func (s *Server) handleMistakeAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/mistakes/")
	if !strings.HasSuffix(path, "/ignore") {
		http.NotFound(w, r)
		return
	}
	fingerprint := strings.TrimSuffix(path, "/ignore")
	if fingerprint == "" {
		writeError(w, http.StatusBadRequest, "fingerprint is required")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	if err := s.db.UpdateMistakeStatus(r.Context(), fingerprint, "ignored"); err != nil {
		if errors.Is(err, store.ErrMistakeNotFound) {
			writeError(w, http.StatusNotFound, "mistake not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "fingerprint": fingerprint, "status": "ignored"})
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if s.db == nil {
		writeError(w, http.StatusInternalServerError, "store is not available")
		return
	}
	query := store.RuleLedgerQuery{
		ProjectPath: r.URL.Query().Get("project"),
		Engine:      r.URL.Query().Get("engine"),
		Scope:       r.URL.Query().Get("scope"),
		Status:      r.URL.Query().Get("status"),
	}
	rules, err := s.db.ListRuleLedger(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

func (s *Server) handleRadar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	top := 5
	if raw := strings.TrimSpace(r.URL.Query().Get("top")); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &top); err != nil || top <= 0 {
			writeError(w, http.StatusBadRequest, "top must be a positive integer")
			return
		}
	}
	if project == "" {
		writeJSON(w, http.StatusOK, emptyRadarResponse(""))
		return
	}
	info, err := os.Stat(project)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, emptyRadarResponse(project))
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusOK, emptyRadarResponse(project))
		return
	}
	baseline, err := radar.LoadBaselineStatus(project)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	report, err := radar.ScanReport(r.Context(), radar.Options{Root: project, Top: top, Runner: s.runner})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	screenshots, err := adapter.ListUIScreenshots(project)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project":     project,
		"baseline":    baseline,
		"hotspots":    report.Hotspots,
		"skipped":     report.Skipped,
		"screenshots": screenshots,
	})
}

func emptyRadarResponse(project string) map[string]any {
	return map[string]any{
		"project": project,
		"baseline": map[string]any{
			"available": false,
			"metrics":   []any{},
		},
		"hotspots":    []radar.Hotspot{},
		"screenshots": []adapter.UIScreenshot{},
	}
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if project == "" || path == "" {
		writeError(w, http.StatusBadRequest, "project and path are required")
		return
	}
	fullPath, err := safeProjectFile(project, path)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	http.ServeFile(w, r, fullPath)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func safeProjectFile(project, path string) (string, error) {
	root, err := filepath.Abs(project)
	if err != nil {
		return "", err
	}
	joined := path
	if !filepath.IsAbs(path) {
		joined = filepath.Join(root, path)
	}
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", errors.New("screenshot path is outside the project")
	}
	return abs, nil
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
