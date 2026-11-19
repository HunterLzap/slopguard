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
)

const (
	uiTokenFileName     = "ui-token"
	uiSessionCookieName = "slopguard_session"
	uiTokenHeaderName   = "X-SlopGuard-Token"
	maxRequestBodyBytes = 2 << 20 // 2 MiB
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

// ensurePersistentAuthToken generates a fresh cryptographically random session token
// and stores it atomically with 0600 permissions inside a 0700 ~/.slopguard directory.
func ensurePersistentAuthToken(home string) (string, error) {
	path := tokenFilePath(home)
	if path == "" {
		return "", errors.New("cannot locate home directory for ui-token")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create slopguard state dir: %w", err)
	}
	_ = os.Chmod(dir, 0o700)

	token := generateRandomToken()
	tmpFile, err := os.CreateTemp(dir, uiTokenFileName+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp ui-token: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return "", fmt.Errorf("chmod ui-token: %w", err)
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
	_ = os.Chmod(path, 0o600)
	return token, nil
}

// AuthToken returns the active session token required to access the dashboard API.
func (s *Server) AuthToken() string {
	if s == nil {
		return ""
	}
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.authToken
}

// SetAuthToken updates the active dashboard session token.
func (s *Server) SetAuthToken(token string) {
	if s == nil {
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.authToken = strings.TrimSpace(token)
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
		if constantTimeTokenMatch(expected, strings.TrimSpace(c.Value)) {
			return true, false
		}
	}
	return false, false
}

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":       "slopguard",
		"pid":           os.Getpid(),
		"port":          s.port,
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
			if qTok := strings.TrimSpace(r.URL.Query().Get("token")); qTok != "" {
				if constantTimeTokenMatch(s.AuthToken(), qTok) {
					http.SetCookie(w, &http.Cookie{
						Name:     uiSessionCookieName,
						Value:    s.AuthToken(),
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
	_, _ = w.Write([]byte(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>SlopGuard · 本地鉴权</title></head><body style="font-family:-apple-system,sans-serif;padding:48px;color:#1d1d1f;"><h2>需要本地会话授权</h2><p>请通过 <code>SlopGuard.app</code> 或执行 <code>slopguard ui</code> 打开控制台，以携带本地一次性授权令牌（<code>~/.slopguard/ui-token</code>）。</p></body></html>`))
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
