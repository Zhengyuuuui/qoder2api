package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/logger"
)

const (
	sessionCookieBase = "qoder2api_session"
	sessionTTL        = 24 * time.Hour
)

// consoleWebPort 由 main 启动时写入，用于区分同 host 不同端口的 cookie
// （浏览器 cookie 不区分端口，CN 3588 与 Global 3589 必须用不同 cookie 名）
var consoleWebPort int

func sessionCookieName() string {
	if consoleWebPort > 0 {
		return fmt.Sprintf("%s_%d", sessionCookieBase, consoleWebPort)
	}
	return sessionCookieBase
}

// legacy cookie 名（升级前），读取时兼容并在登录时写新名
func legacySessionCookieName() string {
	return sessionCookieBase
}

type sessionStore struct {
	mu   sync.Mutex
	data map[string]time.Time // token -> expire
}

var sessions = &sessionStore{data: map[string]time.Time{}}

func (s *sessionStore) create() (string, time.Time) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	exp := time.Now().Add(sessionTTL)
	s.mu.Lock()
	s.data[tok] = exp
	s.mu.Unlock()
	return tok, exp
}

func (s *sessionStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.data[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.data, tok)
		return false
	}
	// sliding expiration
	s.data[tok] = time.Now().Add(sessionTTL)
	return true
}

func (s *sessionStore) revoke(tok string) {
	s.mu.Lock()
	delete(s.data, tok)
	s.mu.Unlock()
}

func effectiveConsolePassword() string {
	if v := strings.TrimSpace(os.Getenv("QODER2API_CONSOLE_PASSWORD")); v != "" {
		return v
	}
	if svc != nil {
		if st, err := svc.GetSettings(); err == nil && st != nil {
			if p := strings.TrimSpace(st.ConsolePassword); p != "" {
				return p
			}
		}
	}
	return ""
}

// ensureConsolePassword 若未配置密码则生成并写入 settings，启动时打印一次。
func ensureConsolePassword() string {
	if p := effectiveConsolePassword(); p != "" {
		return p
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	p := hex.EncodeToString(b)
	cur, err := account.LoadSettings()
	if err != nil || cur == nil {
		cur = &account.Settings{Port: 8963, LogLevel: "info"}
	}
	cur.ConsolePassword = p
	_ = account.SaveSettings(cur)
	// 注意：这里只同步 BridgeToken，不覆盖 svc.bridgePort。
	// 端口优先级由 main 的 --instance/--bridge-port 决定，避免首次启动时
	// settings 默认值（8963）把 --instance=global 预设的 8964 冲掉。
	if svc != nil {
		svc.bridgeToken = cur.BridgeToken
	}
	logger.Info("console password auto-generated (saved to settings): %s", p)
	logger.Info("override with env QODER2API_CONSOLE_PASSWORD or settings.console_password")
	return p
}

func passwordOK(input string) bool {
	want := effectiveConsolePassword()
	if want == "" {
		return false
	}
	// constant-time compare
	a := sha256.Sum256([]byte(input))
	b := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func sessionTokenFromRequest(r *http.Request) string {
	// 优先读带端口的 cookie，再兼容旧名
	for _, name := range []string{sessionCookieName(), legacySessionCookieName()} {
		if c, err := r.Cookie(name); err == nil && c != nil && c.Value != "" {
			return c.Value
		}
	}
	// also allow Authorization: Bearer <session> for API tools
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func setSessionCookie(w http.ResponseWriter, tok string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(),
		Value:    tok,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure: only enable behind HTTPS; leave false for local/http deploy
	})
	// 清掉旧通用 cookie，避免同 host 另一端口被覆盖后误用
	if sessionCookieName() != legacySessionCookieName() {
		http.SetCookie(w, &http.Cookie{
			Name:     legacySessionCookieName(),
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

func clearSessionCookie(w http.ResponseWriter) {
	for _, name := range []string{sessionCookieName(), legacySessionCookieName()} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

func isPublicPath(path string) bool {
	if path == "/login.html" || path == "/favicon.ico" {
		return true
	}
	if strings.HasPrefix(path, "/api/auth/") {
		return true
	}
	// 健康检查端点：外部探针（负载均衡 / k8s liveness）无法携带控制台
	// 会话，故放行。它只暴露风控计数摘要，不含任何凭据。
	if path == "/health" {
		return true
	}
	// static assets for login page
	if strings.HasPrefix(path, "/assets/") {
		return true
	}
	return false
}

func requireConsoleAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		tok := sessionTokenFromRequest(r)
		if sessions.valid(tok) {
			next.ServeHTTP(w, r)
			return
		}
		// HTML pages -> redirect login; API -> 401 JSON
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: please login"))
			return
		}
		http.Redirect(w, r, "/login.html", http.StatusFound)
	})
}

func handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !passwordOK(strings.TrimSpace(req.Password)) {
		// small delay against brute force
		time.Sleep(300 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, fmt.Errorf("invalid password"))
		return
	}
	tok, exp := sessions.create()
	setSessionCookie(w, tok, exp)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":         true,
		"expires_at": exp.Unix(),
	})
}

func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	tok := sessionTokenFromRequest(r)
	sessions.revoke(tok)
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleAuthMe(w http.ResponseWriter, r *http.Request) {
	tok := sessionTokenFromRequest(r)
	ok := sessions.valid(tok)
	if !ok {
		writeError(w, http.StatusUnauthorized, fmt.Errorf("unauthorized"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"authed":  true,
		"message": "console session active",
	})
}
