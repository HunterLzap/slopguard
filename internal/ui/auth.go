package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	uiTokenFileName     = "ui-token"
	uiSessionCookieName = "slopguard_session"
	uiTokenHeaderName   = "X-SlopGuard-Token"
	maxRequestBodyBytes = 2 << 20 // 2 MiB
	authTicketTTL       = 60 * time.Second
	authSessionTTL      = 12 * time.Hour
)

func generateRandomToken() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// Fallback should never happen on modern OSes; surface failure visibly.
		panic(fmt.Sprintf("ui: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(raw[:])
}

// tokenFilePath returns ~/.slopguard/ui-token (chmod 0600 inside 0700 dir).
func tokenFilePath(home string) string {
	home = strings.TrimSpace(home)
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".slopguard", uiTokenFileName)
}

// loadOrCreateAuthToken returns the stable local long-term API token. It only
// creates a new token when the existing file is missing or malformed; normal
// restarts must not rotate the token out from under an already running
// dashboard or a cached CLI process.
func loadOrCreateAuthToken(home string) (string, error) {
	path := tokenFilePath(home)
	if path == "" {
		return "", errors.New("cannot locate home directory for ui-token")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create slopguard state dir: %w", err)
	}
	if err := hardenSecretDir(dir); err != nil {
		return "", fmt.Errorf("protect slopguard state dir: %w", err)
	}

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("ui-token is not a regular file: %s", path)
		}
		if raw, readErr := os.ReadFile(path); readErr == nil {
			token := strings.TrimSpace(string(raw))
			if validLongTermToken(token) {
				_ = hardenSecretFile(path)
				return token, nil
			}
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect ui-token: %w", err)
	}

	token := generateRandomToken()
	tmpFile, err := os.CreateTemp(dir, uiTokenFileName+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp ui-token: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := hardenSecretFile(tmpPath); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return "", fmt.Errorf("protect temp ui-token: %w", err)
	}
	if _, err := tmpFile.WriteString(token + "\n"); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return "", fmt.Errorf("write ui-token: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return "", fmt.Errorf("sync ui-token: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("close ui-token: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return "", fmt.Errorf("install ui-token: %w", err)
	}
	if err := hardenSecretFile(path); err != nil {
		return "", fmt.Errorf("protect ui-token: %w", err)
	}
	return token, nil
}

// ensurePersistentAuthToken is kept for package-internal compatibility; it now
// follows the stable-token semantics above.
func ensurePersistentAuthToken(home string) (string, error) {
	return loadOrCreateAuthToken(home)
}

func validLongTermToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for _, r := range token {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// AuthToken returns the active long-term local API token.
func (s *Server) AuthToken() string {
	if s == nil {
		return ""
	}
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.authToken
}

// SetAuthToken updates the active dashboard long-term token and clears any
// browser tickets/sessions derived from a previous token.
func (s *Server) SetAuthToken(token string) {
	if s == nil {
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.authToken = strings.TrimSpace(token)
	s.authTickets = make(map[string]authTicket)
	s.authSessions = make(map[string]authSession)
}

type authTicket struct {
	ExpiresAt time.Time
}

type authSession struct {
	ExpiresAt time.Time
}

// IssueAuthTicket creates a short-lived, one-time browser bootstrap ticket.
func (s *Server) IssueAuthTicket(ttl time.Duration) (string, time.Time, error) {
	if s == nil {
		return "", time.Time{}, errors.New("ui: nil server")
	}
	if ttl <= 0 {
		ttl = authTicketTTL
	}
	ticket := generateRandomToken()
	expires := time.Now().Add(ttl)
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authTickets == nil {
		s.authTickets = make(map[string]authTicket)
	}
	s.authTickets[ticket] = authTicket{ExpiresAt: expires}
	return ticket, expires, nil
}

func (s *Server) consumeAuthTicket(ticket string) bool {
	if s == nil || strings.TrimSpace(ticket) == "" {
		return false
	}
	now := time.Now()
	s.authMu.Lock()
	defer s.authMu.Unlock()
	rec, ok := s.authTickets[ticket]
	if !ok {
		return false
	}
	delete(s.authTickets, ticket)
	return now.Before(rec.ExpiresAt)
}

func (s *Server) issueAuthSession() (string, time.Time) {
	session := generateRandomToken()
	expires := time.Now().Add(authSessionTTL)
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authSessions == nil {
		s.authSessions = make(map[string]authSession)
	}
	s.authSessions[session] = authSession{ExpiresAt: expires}
	return session, expires
}

func (s *Server) validAuthSession(session string) bool {
	if s == nil || strings.TrimSpace(session) == "" {
		return false
	}
	now := time.Now()
	s.authMu.Lock()
	defer s.authMu.Unlock()
	rec, ok := s.authSessions[session]
	if !ok {
		return false
	}
	if !now.Before(rec.ExpiresAt) {
		delete(s.authSessions, session)
		return false
	}
	return true
}

func constantTimeTokenMatch(expected, candidate string) bool {
	if expected == "" || candidate == "" {
		return false
	}
	if len(expected) != len(candidate) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(candidate)) == 1
}

func isLoopbackHostname(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func splitHostPortSafe(hostHeader string) (string, string, bool) {
	raw := strings.TrimSpace(hostHeader)
	if raw == "" {
		return "", "", false
	}
	if h, p, err := net.SplitHostPort(raw); err == nil {
		return h, p, true
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		return raw[1 : len(raw)-1], "", true
	}
	if strings.Count(raw, ":") == 0 {
		return raw, "", true
	}
	return "", "", false
}

func isAllowedLoopbackHost(hostHeader string, serverPort int) bool {
	host, port, ok := splitHostPortSafe(hostHeader)
	if !ok || !isLoopbackHostname(host) {
		return false
	}
	if serverPort > 0 && port != "" && port != strconv.Itoa(serverPort) {
		return false
	}
	return true
}

func isAllowedOriginHeader(originHeader, reqHost string, serverPort int) bool {
	originHeader = strings.TrimSpace(originHeader)
	if originHeader == "" {
		return true
	}
	if strings.EqualFold(originHeader, "null") {
		return false
	}
	u, err := neturl.Parse(originHeader)
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	if !isLoopbackHostname(u.Hostname()) {
		return false
	}
	if serverPort > 0 {
		if p := u.Port(); p != "" && p != strconv.Itoa(serverPort) {
			return false
		}
	}
	if reqHost != "" && !strings.EqualFold(u.Host, reqHost) {
		reqHostname, reqPort, ok := splitHostPortSafe(reqHost)
		if !ok || !strings.EqualFold(u.Hostname(), reqHostname) {
			return false
		}
		if reqPort != "" && u.Port() != "" && reqPort != u.Port() {
			return false
		}
	}
	return true
}

func (s *Server) authenticateRequest(r *http.Request) (ok bool, viaHeader bool) {
	expected := s.AuthToken()
	if expected == "" {
		return false, false
	}
	if hdr := strings.TrimSpace(r.Header.Get(uiTokenHeaderName)); hdr != "" {
		if constantTimeTokenMatch(expected, hdr) {
			return true, true
		}
		return false, false
	}
	if c, err := r.Cookie(uiSessionCookieName); err == nil {
		if s.validAuthSession(strings.TrimSpace(c.Value)) {
			return true, false
		}
	}
	return false, false
}

func (s *Server) handleAuthTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !constantTimeTokenMatch(s.AuthToken(), strings.TrimSpace(r.Header.Get(uiTokenHeaderName))) {
		writeError(w, http.StatusUnauthorized, "未授权访问：缺少有效的本地长期令牌")
		return
	}
	ticket, expires, err := s.IssueAuthTicket(authTicketTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"ticket":     ticket,
		"expires_at": expires.UTC().Format(time.RFC3339),
		"url":        "/?ticket=" + neturl.QueryEscape(ticket),
	})
}

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	startedAt := ""
	if !s.startedAt.IsZero() {
		startedAt = s.startedAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":       "slopguard",
		"pid":           os.Getpid(),
		"port":          s.port,
		"instance_id":   s.instanceID,
		"started_at":    startedAt,
		"auth_required": true,
	})
}

func (s *Server) withSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")

		if !isAllowedLoopbackHost(r.Host, s.port) {
			writeError(w, http.StatusForbidden, "forbidden host header")
			return
		}
		if sfs := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); sfs != "" {
			if sfs != "same-origin" && sfs != "none" {
				writeError(w, http.StatusForbidden, "cross-site request blocked")
				return
			}
		}
		if !isAllowedOriginHeader(r.Header.Get("Origin"), r.Host, s.port) {
			writeError(w, http.StatusForbidden, "forbidden origin header")
			return
		}

		path := r.URL.Path
		if path == "/api/identity" {
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(path, "/static/") {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				methodNotAllowed(w, http.MethodGet)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if path == "/" || path == "/index.html" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				methodNotAllowed(w, http.MethodGet)
				return
			}
			if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" {
				if s.consumeAuthTicket(ticket) {
					session, _ := s.issueAuthSession()
					http.SetCookie(w, &http.Cookie{
						Name:     uiSessionCookieName,
						Value:    session,
						Path:     "/",
						HttpOnly: true,
						SameSite: http.SameSiteStrictMode,
					})
					http.Redirect(w, r, "/", http.StatusFound)
					return
				}
				writeUnauthorizedHTML(w)
				return
			}
			if ok, _ := s.authenticateRequest(r); !ok {
				writeUnauthorizedHTML(w)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			ok, viaHeader := s.authenticateRequest(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "未授权访问：缺少或无效的本地会话令牌")
				return
			}
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				if !viaHeader && !strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Requested-With")), "SlopGuard") {
					writeError(w, http.StatusForbidden, "缺少同源请求校验头 (X-Requested-With)")
					return
				}
				if r.Body != nil && r.ContentLength != 0 {
					ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
					if !strings.HasPrefix(ct, "application/json") {
						writeError(w, http.StatusUnsupportedMediaType, "请求必须使用 application/json")
						return
					}
				}
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
			}
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func writeUnauthorizedHTML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>SlopGuard · 本地鉴权</title></head><body style="font-family:-apple-system,sans-serif;padding:48px;color:#1d1d1f;"><h2>需要本地会话授权</h2><p>请通过 <code>SlopGuard.app</code> 或执行 <code>slopguard ui</code> 打开控制台；浏览器会使用一次性授权 ticket 建立本地会话。</p></body></html>`))
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) bool {
	if r.Body == nil {
		if allowEmpty {
			return true
		}
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return false
	}
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) && allowEmpty {
		return true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "请求体超出大小限制 (2 MiB)")
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid JSON payload")
	return false
}
