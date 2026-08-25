package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type AuthSettings struct {
	PasswordHash       string `json:"passwordHash,omitempty"`
	TOTPSecret         string `json:"totpSecret,omitempty"`
	TOTPEnabled        bool   `json:"totpEnabled,omitempty"`
	GoogleEnabled      bool   `json:"googleEnabled,omitempty"`
	GoogleClientID     string `json:"googleClientId,omitempty"`
	GoogleClientSecret string `json:"googleClientSecret,omitempty"`
	GoogleEmail        string `json:"googleEmail,omitempty"`
}
type sessionInfo struct{ Expires time.Time }

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// PBKDF2-HMAC-SHA256 avoids storing a plaintext password and uses only the Go standard library.
func passwordHash(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	iter := 310000
	dk := pbkdf2SHA256([]byte(password), salt, iter, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(dk))
}
func verifyPassword(stored, password string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 100000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(p[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(p[3])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}
func pbkdf2SHA256(password, salt []byte, iter, n int) []byte {
	out := make([]byte, 0, n)
	for block := uint32(1); len(out) < n; block++ {
		b := make([]byte, len(salt)+4)
		copy(b, salt)
		binary.BigEndian.PutUint32(b[len(salt):], block)
		m := hmac.New(sha256.New, password)
		m.Write(b)
		u := m.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			m = hmac.New(sha256.New, password)
			m.Write(u)
			u = m.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:n]
}
func totpCode(secret string, now time.Time) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(now.Unix()/30))
	m := hmac.New(sha1.New, key)
	m.Write(b[:])
	sum := m.Sum(nil)
	o := sum[len(sum)-1] & 15
	v := (uint32(sum[o])&127)<<24 | uint32(sum[o+1])<<16 | uint32(sum[o+2])<<8 | uint32(sum[o+3])
	return fmt.Sprintf("%06d", v%1000000)
}
func verifyTOTP(secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	now := time.Now()
	for d := -1; d <= 1; d++ {
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, now.Add(time.Duration(d)*30*time.Second))), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

func (a *App) authConfigured() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.settings.Auth.PasswordHash != ""
}
func (a *App) authenticated(r *http.Request) bool {
	c, err := r.Cookie("cortex_session")
	if err != nil {
		return false
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	s, ok := a.sessions[c.Value]
	if !ok || time.Now().After(s.Expires) {
		if ok {
			delete(a.sessions, c.Value)
		}
		return false
	}
	return true
}
func (a *App) newSessionCookie(w http.ResponseWriter, r *http.Request) {
	token := randomToken(32)
	a.authMu.Lock()
	a.sessions[token] = sessionInfo{Expires: time.Now().Add(7 * 24 * time.Hour)}
	a.authMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "cortex_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, MaxAge: 7 * 24 * 3600})
}
func (a *App) clearSession(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("cortex_session"); e == nil {
		a.authMu.Lock()
		delete(a.sessions, c.Value)
		a.authMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "cortex_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
}

func (a *App) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/api/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		if !a.authConfigured() {
			http.Error(w, "setup required", http.StatusUnauthorized)
			return
		}
		if !a.authenticated(r) {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *App) authState(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	x := a.settings.Auth
	a.mu.RUnlock()
	jsonOut(w, map[string]any{"configured": x.PasswordHash != "", "authenticated": a.authenticated(r), "totpEnabled": x.TOTPEnabled, "googleEnabled": x.GoogleEnabled, "googleConfigured": x.GoogleClientID != "" && x.GoogleClientSecret != "", "googleEmail": x.GoogleEmail, "googleClientID": x.GoogleClientID})
}
func (a *App) authSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	if a.authConfigured() {
		http.Error(w, "already configured", 409)
		return
	}
	var q struct {
		Password string `json:"password"`
		Confirm  string `json:"confirm"`
	}
	if !decode(w, r, &q) {
		return
	}
	if len(q.Password) < 7 {
		http.Error(w, "password must be at least 7 characters", 400)
		return
	}
	if q.Password != q.Confirm {
		http.Error(w, "passwords do not match", 400)
		return
	}
	a.mu.Lock()
	a.settings.Auth.PasswordHash = passwordHash(q.Password)
	a.mu.Unlock()
	if err := a.saveSettings(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.newSessionCookie(w, r)
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var q struct{ Password, TOTP string }
	if !decode(w, r, &q) {
		return
	}
	a.mu.RLock()
	x := a.settings.Auth
	a.mu.RUnlock()
	if !verifyPassword(x.PasswordHash, q.Password) {
		http.Error(w, "invalid credentials", 401)
		return
	}
	if x.TOTPEnabled && !verifyTOTP(x.TOTPSecret, q.TOTP) {
		http.Error(w, "invalid two-factor code", 401)
		return
	}
	a.newSessionCookie(w, r)
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	a.clearSession(w, r)
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if !a.authConfigured() || !a.authenticated(r) {
		http.Error(w, "authentication required", 401)
		return false
	}
	return true
}
func (a *App) authPassword(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(w, r) {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var q struct{ Current, Password, Confirm string }
	if !decode(w, r, &q) {
		return
	}
	a.mu.RLock()
	old := a.settings.Auth.PasswordHash
	a.mu.RUnlock()
	if !verifyPassword(old, q.Current) {
		http.Error(w, "current password is incorrect", 401)
		return
	}
	if len(q.Password) < 7 || q.Password != q.Confirm {
		http.Error(w, "new password must match and be at least 7 characters", 400)
		return
	}
	a.mu.Lock()
	a.settings.Auth.PasswordHash = passwordHash(q.Password)
	a.mu.Unlock()
	if err := a.saveSettings(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authTOTPBegin(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(w, r) {
		return
	}
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	a.mu.Lock()
	a.pendingTOTP = secret
	a.mu.Unlock()
	jsonOut(w, map[string]string{"secret": secret, "uri": "otpauth://totp/Cortex?secret=" + secret + "&issuer=Cortex"})
}
func (a *App) authTOTPEnable(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(w, r) {
		return
	}
	var q struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &q) {
		return
	}
	a.mu.RLock()
	secret := a.pendingTOTP
	a.mu.RUnlock()
	if secret == "" || !verifyTOTP(secret, q.Code) {
		http.Error(w, "invalid two-factor code", 400)
		return
	}
	a.mu.Lock()
	a.settings.Auth.TOTPSecret = secret
	a.settings.Auth.TOTPEnabled = true
	a.pendingTOTP = ""
	a.mu.Unlock()
	if err := a.saveSettings(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authTOTPDisable(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(w, r) {
		return
	}
	var q struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &q) {
		return
	}
	a.mu.RLock()
	hash := a.settings.Auth.PasswordHash
	a.mu.RUnlock()
	if !verifyPassword(hash, q.Password) {
		http.Error(w, "password is incorrect", 401)
		return
	}
	a.mu.Lock()
	a.settings.Auth.TOTPEnabled = false
	a.settings.Auth.TOTPSecret = ""
	a.mu.Unlock()
	if err := a.saveSettings(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) authGoogleConfig(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuth(w, r) {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var q struct {
		Enabled                       bool `json:"enabled"`
		ClientID, ClientSecret, Email string
	}
	if !decode(w, r, &q) {
		return
	}
	a.mu.Lock()
	a.settings.Auth.GoogleEnabled = q.Enabled
	a.settings.Auth.GoogleClientID = strings.TrimSpace(q.ClientID)
	if strings.TrimSpace(q.ClientSecret) != "" {
		a.settings.Auth.GoogleClientSecret = strings.TrimSpace(q.ClientSecret)
	}
	a.settings.Auth.GoogleEmail = strings.ToLower(strings.TrimSpace(q.Email))
	a.mu.Unlock()
	if err := a.saveSettings(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func (a *App) googleStart(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	x := a.settings.Auth
	a.mu.RUnlock()
	if !x.GoogleEnabled || x.GoogleClientID == "" || x.GoogleClientSecret == "" {
		http.Error(w, "Google sign-in is not configured", 400)
		return
	}
	state := randomToken(24)
	a.authMu.Lock()
	a.oauthStates[state] = time.Now().Add(10 * time.Minute)
	a.authMu.Unlock()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	redirect := scheme + "://" + r.Host + "/api/auth/google/callback"
	q := url.Values{"client_id": {x.GoogleClientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {"openid email"}, "state": {state}, "prompt": {"select_account"}}
	http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?"+q.Encode(), 302)
}
func (a *App) googleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	a.authMu.Lock()
	exp, ok := a.oauthStates[state]
	delete(a.oauthStates, state)
	a.authMu.Unlock()
	if !ok || time.Now().After(exp) {
		http.Error(w, "invalid OAuth state", 400)
		return
	}
	code := r.URL.Query().Get("code")
	a.mu.RLock()
	x := a.settings.Auth
	a.mu.RUnlock()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	redirect := scheme + "://" + r.Host + "/api/auth/google/callback"
	resp, err := http.PostForm("https://oauth2.googleapis.com/token", url.Values{"code": {code}, "client_id": {x.GoogleClientID}, "client_secret": {x.GoogleClientSecret}, "redirect_uri": {redirect}, "grant_type": {"authorization_code"}})
	if err != nil {
		http.Error(w, "Google token exchange failed", 502)
		return
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode/100 != 2 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok) != nil || tok.AccessToken == "" {
		http.Error(w, "Google token exchange failed", 401)
		return
	}
	req, _ := http.NewRequest("GET", "https://openidconnect.googleapis.com/v1/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	ur, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "Google user lookup failed", 502)
		return
	}
	defer ur.Body.Close()
	var user struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if ur.StatusCode/100 != 2 || json.NewDecoder(io.LimitReader(ur.Body, 1<<20)).Decode(&user) != nil || !user.EmailVerified {
		http.Error(w, "Google account email is not verified", 401)
		return
	}
	if x.GoogleEmail != "" && !strings.EqualFold(x.GoogleEmail, user.Email) {
		http.Error(w, "Google account is not authorized for this Cortex instance", 403)
		return
	}
	a.newSessionCookie(w, r)
	http.Redirect(w, r, "/", 302)
}
func (a *App) authDebugHash() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	h := sha256.Sum256([]byte(a.settings.Auth.PasswordHash))
	return hex.EncodeToString(h[:4])
}

var _ = os.ErrNotExist
