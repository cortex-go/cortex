package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	cortex "github.com/cortex-go/cortex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Listen, Root, DataDir string
	TrustProxy            bool
	PublicOrigin          string
}
type App struct {
	listen, root, dataDir string
	trustProxy            bool
	publicOrigin          *url.URL
	db                    *sql.DB
	mux                   *http.ServeMux
	mu                    sync.RWMutex
	authMu                sync.Mutex
	setupMu               sync.Mutex
	sessions              map[string]sessionInfo
	loginFailures         map[string][]time.Time
	oauthStates           map[string]oauthState
	pendingTOTP           map[string]pendingTOTP
	usedTOTP              map[string]time.Time
	settings              Settings
}
type Provider struct {
	ID, Label, Hint, OpenCodeID, DefaultModel, AuthMode string
}
type Settings struct {
	ActiveProvider string            `json:"activeProvider"`
	Keys           map[string]string `json:"keys"`
	Models         map[string]string `json:"models"`
	Auth           AuthSettings      `json:"auth"`
}

type publicSettings struct {
	ActiveProvider string           `json:"activeProvider"`
	Providers      []map[string]any `json:"providers"`
}

var providers = []Provider{
	{"opencode", "OpenCode Zen", "OpenCode Zen API key", "opencode", "deepseek-v4-flash", "key"},
	{"openrouter", "OpenRouter", "OpenRouter API key", "openrouter", "anthropic/claude-sonnet-4.5", "key"},
	{"openai", "OpenAI API", "OpenAI API key", "openai", "gpt-5.2", "key"},
	{"anthropic", "Anthropic API", "Anthropic API key", "anthropic", "claude-sonnet-4-20250514", "key"},
	{"google", "Google AI", "Google AI API key", "google", "gemini-2.5-pro", "key"},
	{"deepseek", "DeepSeek API", "DeepSeek API key", "deepseek", "deepseek-v4-pro", "key"},
	{"openai-chatgpt", "ChatGPT Plus / Pro", "Uses an existing OpenCode ChatGPT login", "openai", "gpt-5.2", "opencode-auth"},
	{"github-copilot", "GitHub Copilot", "Uses an existing OpenCode GitHub Copilot login", "github-copilot", "gpt-5", "opencode-auth"},
}

func New(o Options) (*App, error) {
	root, err := filepath.Abs(o.Root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.DataDir, 0700); err != nil {
		return nil, err
	}
	db, err := openDatabase(o.DataDir)
	if err != nil {
		return nil, err
	}
	var publicOrigin *url.URL
	if strings.TrimSpace(o.PublicOrigin) != "" {
		publicOrigin, err = url.Parse(strings.TrimRight(strings.TrimSpace(o.PublicOrigin), "/"))
		if err != nil || (publicOrigin.Scheme != "http" && publicOrigin.Scheme != "https") || publicOrigin.Host == "" || publicOrigin.User != nil || publicOrigin.Path != "" || publicOrigin.RawQuery != "" || publicOrigin.Fragment != "" {
			db.Close()
			return nil, errors.New("public origin must be an http(s) origin without a path")
		}
	}
	a := &App{listen: o.Listen, root: root, dataDir: o.DataDir, trustProxy: o.TrustProxy, publicOrigin: publicOrigin, db: db, mux: http.NewServeMux(), settings: Settings{ActiveProvider: "opencode", Keys: map[string]string{}, Models: map[string]string{}}, sessions: map[string]sessionInfo{}, loginFailures: map[string][]time.Time{}, oauthStates: map[string]oauthState{}, pendingTOTP: map[string]pendingTOTP{}, usedTOTP: map[string]time.Time{}}
	_ = a.loadSettings()
	if a.settings.Keys == nil {
		a.settings.Keys = map[string]string{}
	}
	if a.settings.Models == nil {
		a.settings.Models = map[string]string{}
	}
	a.routes()
	return a, nil
}
func (a *App) Close() error { return a.db.Close() }
func (a *App) Root() string { return a.root }
func (a *App) ListenAndServe() error {
	return a.httpServer().ListenAndServe()
}
func (a *App) httpServer() *http.Server {
	return &http.Server{
		Addr:              a.listen,
		Handler:           a.security(a.recoverPanics(a.httpBoundary(a.hostBoundary(a.authMiddleware(a.mux))))),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}
func (a *App) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
func (a *App) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func (a *App) httpBoundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.RequestURI()) > 8192 {
			http.Error(w, "request URI too long", http.StatusRequestURITooLong)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) hostBoundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.validHost(r.Host) {
			http.Error(w, "invalid host", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *App) routes() {
	for _, route := range a.apiRoutes() {
		a.mux.Handle(route.Policy.Path, enforceRoutePolicy(route.Policy, route.Handler))
	}
	a.mux.Handle("/", http.FileServer(http.FS(cortex.PublicFS())))
}
func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeJSON(w, r, v, 1<<20)
}
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "invalid json", 400)
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		http.Error(w, "invalid json", 400)
		return false
	}
	return true
}
func (a *App) settingsPath() string { return filepath.Join(a.dataDir, "settings.json") }
func (a *App) loadSettings() error {
	b, err := os.ReadFile(a.settingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &a.settings)
}
func (a *App) saveSettings() error {
	a.mu.RLock()
	b, err := json.MarshalIndent(a.settings, "", "  ")
	a.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := a.settingsPath() + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, a.settingsPath())
}
func (a *App) publicSettings() publicSettings {
	a.mu.RLock()
	active := a.settings.ActiveProvider
	keys := make(map[string]string, len(a.settings.Keys))
	models := make(map[string]string, len(a.settings.Models))
	for k, v := range a.settings.Keys {
		keys[k] = v
	}
	for k, v := range a.settings.Models {
		models[k] = v
	}
	a.mu.RUnlock()
	out := publicSettings{ActiveProvider: active}
	for _, p := range providers {
		model := strings.TrimSpace(models[p.ID])
		if model == "" {
			model = p.DefaultModel
		}
		configured := false
		if p.AuthMode == "key" {
			configured = strings.TrimSpace(keys[p.ID]) != ""
		} else {
			configured = a.hostOpenCodeAuthConfigured(p.OpenCodeID)
		}
		out.Providers = append(out.Providers, map[string]any{
			"id": p.ID, "label": p.Label, "hint": p.Hint, "configured": configured,
			"authMode": p.AuthMode, "model": model, "defaultModel": p.DefaultModel,
			"openCodeID": p.OpenCodeID,
		})
	}
	return out
}

func (a *App) settingsAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		jsonOut(w, a.publicSettings())
	case "POST":
		var q struct {
			Provider  string `json:"provider"`
			Key       string `json:"key"`
			Model     string `json:"model"`
			RemoveKey bool   `json:"removeKey"`
		}
		if !decode(w, r, &q) {
			return
		}
		valid := false
		for _, p := range providers {
			if p.ID == q.Provider {
				valid = true
			}
		}
		if !valid {
			http.Error(w, "unknown provider", 400)
			return
		}
		p, _ := providerByID(q.Provider)
		model := strings.TrimSpace(q.Model)
		if model == "" {
			model = p.DefaultModel
		}
		if strings.Contains(model, "#") {
			http.Error(w, "model variants are not supported in Settings yet", 400)
			return
		}
		a.mu.Lock()
		if a.settings.Keys == nil {
			a.settings.Keys = map[string]string{}
		}
		if a.settings.Models == nil {
			a.settings.Models = map[string]string{}
		}
		if p.AuthMode == "key" {
			if q.RemoveKey {
				delete(a.settings.Keys, q.Provider)
			} else if strings.TrimSpace(q.Key) != "" {
				a.settings.Keys[q.Provider] = strings.TrimSpace(q.Key)
			}
		}
		a.settings.Models[q.Provider] = model
		a.settings.ActiveProvider = q.Provider
		a.mu.Unlock()
		if err := a.saveSettings(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		jsonOut(w, a.publicSettings())
	default:
		http.Error(w, "method", 405)
	}
}
func hostOpenCodeAuthPath() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "opencode", "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode", "auth.json")
}
func (a *App) hostOpenCodeAuthConfigured(providerID string) bool {
	path := hostOpenCodeAuthPath()
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var auth map[string]any
	if json.Unmarshal(b, &auth) != nil {
		return false
	}
	_, ok := auth[providerID]
	return ok
}
func (a *App) configuredModel(providerID string) string {
	p, ok := providerByID(providerID)
	if !ok {
		return ""
	}
	a.mu.RLock()
	model := strings.TrimSpace(a.settings.Models[providerID])
	a.mu.RUnlock()
	if model == "" {
		model = p.DefaultModel
	}
	return model
}
func (a *App) status(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, map[string]any{"root": a.root, "settings": a.publicSettings()})
}
func (a *App) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return a.root, nil
	}
	if filepath.IsAbs(p) {
		p = filepath.Clean(p)
	} else {
		p = filepath.Join(a.root, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(a.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes workspace root")
	}
	return abs, nil
}

type fileEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

func (a *App) filesAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	p, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	out := []fileEntry{}
	for _, e := range ents {
		info, _ := e.Info()
		rel, _ := filepath.Rel(a.root, filepath.Join(p, e.Name()))
		out = append(out, fileEntry{e.Name(), filepath.ToSlash(rel), e.IsDir(), info.Size()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	jsonOut(w, out)
}
func (a *App) fileAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	p, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	if st.IsDir() {
		http.Error(w, "directory", 400)
		return
	}
	if st.Size() > 2<<20 {
		http.Error(w, "file too large to preview", 413)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.Copy(w, f)
}
func providerByID(id string) (Provider, bool) {
	for _, p := range providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}
func (a *App) credential(id string) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	k := strings.TrimSpace(a.settings.Keys[id])
	return k, k != ""
}
func relDisplay(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	if r == "." {
		return filepath.Base(root)
	}
	return filepath.ToSlash(r)
}
func writeEvent(w http.ResponseWriter, f http.Flusher, t string, data any) {
	json.NewEncoder(w).Encode(map[string]any{"type": t, "data": data})
	f.Flush()
}
func readLimit(r io.Reader, n int64) ([]byte, error) { return io.ReadAll(io.LimitReader(r, n)) }
func setEnv(env []string, name, value string) []string {
	prefix := name + "="
	out := env[:0]
	for _, v := range env {
		if !strings.HasPrefix(v, prefix) {
			out = append(out, v)
		}
	}
	return append(out, prefix+value)
}
func errText(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
