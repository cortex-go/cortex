package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxProviderLine   = 1 << 20
	maxProviderBytes  = 32 << 20
	maxProviderEvents = 4096
	maxImageCount     = 6
	maxImageBytes     = 10 << 20
	maxRunBytes       = 96 << 20
)

type agentRunRequest struct {
	Workspace     string        `json:"workspace"`
	Prompt        string        `json:"prompt"`
	Session       string        `json:"session,omitempty"`
	ClientSession string        `json:"clientSession,omitempty"`
	Images        []imageUpload `json:"images,omitempty"`
}

type imageUpload struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

func (a *App) agentStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	a.mu.RLock()
	providerID := a.settings.ActiveProvider
	a.mu.RUnlock()
	p, ok := providerByID(providerID)
	if !ok {
		http.Error(w, "unknown provider", 500)
		return
	}
	configured := false
	if p.AuthMode == "key" {
		_, configured = a.credential(providerID)
	} else {
		configured = a.hostOpenCodeAuthConfigured(p.OpenCodeID)
	}
	model := a.configuredModel(providerID)
	_, err := exec.LookPath("opencode")
	note := ""
	if p.AuthMode == "opencode-auth" {
		note = "Uses OpenCode's existing OAuth credential on the Cortex host. Run `opencode auth login --provider " + p.OpenCodeID + "` once if it is not configured."
	}
	jsonOut(w, map[string]any{
		"available":           err == nil && configured && model != "",
		"opencodeInstalled":   err == nil,
		"credentialAvailable": configured,
		"provider":            providerID,
		"providerLabel":       p.Label,
		"model":               modelRefFor(p, model),
		"authMode":            p.AuthMode,
		"note":                note,
	})
}

func (a *App) agentRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var q agentRunRequest
	if !decodeJSON(w, r, &q, maxRunBytes) {
		return
	}
	q.Prompt = strings.TrimSpace(q.Prompt)
	if q.Prompt == "" {
		http.Error(w, "prompt required", 400)
		return
	}
	if len(q.Images) > maxImageCount {
		http.Error(w, "too many images", http.StatusRequestEntityTooLarge)
		return
	}
	select {
	case a.runSlots <- struct{}{}:
		defer func() { <-a.runSlots }()
	default:
		http.Error(w, "agent concurrency limit reached", http.StatusTooManyRequests)
		return
	}
	workspace, err := a.resolve(q.Workspace)
	if err != nil {
		http.Error(w, "invalid workspace: "+err.Error(), 400)
		return
	}
	st, err := os.Stat(workspace)
	if err != nil || !st.IsDir() {
		http.Error(w, "workspace must be a directory", 400)
		return
	}
	a.mu.RLock()
	providerID := a.settings.ActiveProvider
	a.mu.RUnlock()
	provider, ok := providerByID(providerID)
	if !ok {
		http.Error(w, "unknown provider", 400)
		return
	}
	modelID := a.configuredModel(providerID)
	if modelID == "" {
		http.Error(w, "model is not configured for "+provider.Label, 400)
		return
	}
	key := ""
	if provider.AuthMode == "key" {
		var configured bool
		key, configured = a.credential(providerID)
		if !configured {
			http.Error(w, provider.Label+" credential is not configured", 400)
			return
		}
	} else if !a.hostOpenCodeAuthConfigured(provider.OpenCodeID) {
		http.Error(w, provider.Label+" is not connected in OpenCode; run `opencode auth login --provider "+provider.OpenCodeID+"` on the Cortex host", 400)
		return
	}
	modelRef := modelRefFor(provider, modelID)
	binary, err := exec.LookPath("opencode")
	if err != nil {
		http.Error(w, "OpenCode is not installed or not in Cortex's PATH", 503)
		return
	}
	clientSession := strings.TrimSpace(q.ClientSession)
	if clientSession == "" {
		clientSession = "default"
	}
	if !validRecordID(clientSession) {
		http.Error(w, "invalid client session", 400)
		return
	}
	runDir, err := os.MkdirTemp("", "cortex-opencode-config-")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer os.RemoveAll(runDir)
	configDir := filepath.Join(runDir, "config")
	dataDir := filepath.Join(a.dataDir, "sessions", clientSession, "data")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if provider.AuthMode == "opencode-auth" {
		if err := copyOpenCodeAuth(dataDir); err != nil {
			http.Error(w, "copy OpenCode auth: "+err.Error(), 500)
			return
		}
	}
	cfg, err := cortexOpenCodeConfig(provider, modelID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	configPath := filepath.Join(configDir, "opencode.json")
	if err := os.WriteFile(configPath, cfg, 0600); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	runID := randomToken(18)
	a.runMu.Lock()
	a.activeRuns[runID] = cancel
	a.runMu.Unlock()
	defer func() { a.runMu.Lock(); delete(a.activeRuns, runID); a.runMu.Unlock() }()
	imageFiles := []string{}
	imageDir := ""
	if len(q.Images) > 0 {
		var err error
		imageDir, imageFiles, err = writeImageAttachments(q.Images)
		if err != nil {
			http.Error(w, "invalid image: "+err.Error(), 400)
			return
		}
		defer os.RemoveAll(imageDir)
	}
	args := agentRunArgs(workspace, modelRef, q.Session, imageFiles, q.Prompt)
	cmd := exec.CommandContext(ctx, binary, args...)
	configureProcessGroup(cmd)
	cmd.Dir = workspace
	env := append([]string{}, os.Environ()...)
	if provider.AuthMode == "key" {
		env = setEnv(env, "CORTEX_PROVIDER_API_KEY", key)
	}
	env = setEnv(env, "OPENCODE_CONFIG", configPath)
	env = setEnv(env, "OPENCODE_CONFIG_DIR", configDir)
	env = setEnv(env, "XDG_CONFIG_HOME", configDir)
	env = setEnv(env, "XDG_DATA_HOME", dataDir)
	env = setEnv(env, "OPENCODE_DISABLE_AUTOUPDATE", "1")
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	if err := cmd.Start(); err != nil {
		writeEvent(w, flusher, "error", map[string]any{"message": err.Error()})
		return
	}
	writeEvent(w, flusher, "run", map[string]any{"runID": runID})
	if err := a.startAgentRun(runID, clientSession, q.Prompt, workspace, providerID, modelID); err != nil {
		cancel()
		_ = cmd.Wait()
		writeEvent(w, flusher, "error", map[string]any{"message": "persist agent run: " + err.Error()})
		return
	}
	errCh := make(chan string, 1)
	go func() { b, _ := readLimit(stderr, 256<<10); errCh <- string(b) }()
	var input, output uint64
	var cost float64
	sessionID := ""
	sawText := false
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64<<10), maxProviderLine)
	streamBytes, streamEvents := 0, 0
	truncated := false
	for scan.Scan() {
		line := append([]byte(nil), scan.Bytes()...)
		streamBytes += len(line)
		streamEvents++
		if streamBytes > maxProviderBytes || streamEvents > maxProviderEvents {
			truncated = true
			cancel()
			writeEvent(w, flusher, "truncated", map[string]any{"message": "Provider output limit reached; the run was stopped."})
			break
		}
		var raw map[string]any
		if json.Unmarshal(line, &raw) == nil {
			collectUsage(raw, &input, &output, &cost)
			if id, _ := raw["sessionID"].(string); id != "" {
				sessionID = id
			}
			if eventText(raw) != "" {
				sawText = true
			}
			rewriteImageURLs(raw, clientSession)
			writeEvent(w, flusher, "opencode", raw)
		} else {
			writeEvent(w, flusher, "output", map[string]any{"text": string(line)})
		}
	}
	waitErr := cmd.Wait()
	stderrText := <-errCh
	if scan.Err() != nil && waitErr == nil {
		waitErr = scan.Err()
	}
	if truncated {
		waitErr = errors.New("provider output limit reached")
	}
	if waitErr == nil && sessionID != "" {
		if recovered, images, ri, ro, rc, e := recoverSession(ctx, binary, sessionID, workspace, clientSession, env); e == nil {
			if !sawText && strings.TrimSpace(recovered) != "" {
				writeEvent(w, flusher, "recovered", map[string]any{"text": recovered, "sessionID": sessionID})
			}
			if len(images) > 0 {
				writeEvent(w, flusher, "recovered-images", map[string]any{"images": images, "sessionID": sessionID})
			}
			if ri > input {
				input = ri
			}
			if ro > output {
				output = ro
			}
			if rc > cost {
				cost = rc
			}
		}
	}
	if waitErr != nil {
		msg := a.redactSecrets(strings.TrimSpace(stderrText))
		if msg == "" {
			msg = waitErr.Error()
		}
		a.finishAgentRun(runID, clientSession, "failed", sessionID, msg, input, output, cost)
		writeEvent(w, flusher, "error", map[string]any{"message": msg})
		return
	}
	a.finishAgentRun(runID, clientSession, "completed", sessionID, "", input, output, cost)
	writeEvent(w, flusher, "done", map[string]any{"inputTokens": input, "outputTokens": output, "estimatedCostUsd": cost, "sessionID": sessionID})
}

func (a *App) agentCancel(w http.ResponseWriter, r *http.Request) {
	var q struct {
		RunID string `json:"runID"`
	}
	if !decode(w, r, &q) {
		return
	}
	if !validRecordID(q.RunID) {
		http.Error(w, "invalid run id", 400)
		return
	}
	a.runMu.Lock()
	cancel := a.activeRuns[q.RunID]
	a.runMu.Unlock()
	if cancel == nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	cancel()
	jsonOut(w, map[string]bool{"cancelled": true})
}

// agentImage serves a generated image from an isolated session's OpenCode data
// directory so the browser can display model-produced images. It resolves the
// path within the session data dir, rejects anything else (including symlinks
// escaping it), and only serves content that sniffs as an image.
func (a *App) agentImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	clientSession := strings.TrimSpace(r.URL.Query().Get("session"))
	if !validRecordID(clientSession) {
		http.Error(w, "invalid session", 400)
		return
	}
	base := filepath.Join(a.dataDir, "sessions", clientSession, "data")
	abs, err := filepath.Abs(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "invalid path", 400)
		return
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		http.Error(w, "path escapes session data", 400)
		return
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "file is unavailable", 404)
			return
		}
		http.Error(w, "path is unavailable", 400)
		return
	}
	rel, err = filepath.Rel(base, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		http.Error(w, "path escapes session data", 400)
		return
	}
	f, err := os.Open(real)
	if err != nil {
		http.Error(w, "file is unavailable", 404)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.Error(w, "not a regular file", 400)
		return
	}
	if st.Size() > 20<<20 {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}
	header := make([]byte, 512)
	n, _ := io.ReadFull(f, header)
	ct := http.DetectContentType(header[:n])
	if !strings.HasPrefix(ct, "image/") {
		http.Error(w, "not an image", 400)
		return
	}
	w.Header().Set("Content-Type", ct)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "file is unavailable", 500)
		return
	}
	_, _ = io.Copy(w, f)
}

// rewriteImageURLs converts file:// image URLs in an OpenCode streamed event
// into Cortex endpoint URLs the browser can fetch. Images produced by a model
// arrive as file parts or tool-result attachments carrying mediaType/mime.
func rewriteImageURLs(v any, clientSession string) {
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			u, _ := x["url"].(string)
			mime, _ := x["mediaType"].(string)
			if mime == "" {
				mime, _ = x["mime"].(string)
			}
			if strings.HasPrefix(u, "file://") && strings.HasPrefix(strings.ToLower(mime), "image/") {
				if p := fileURLPath(u); p != "" {
					x["url"] = "/api/agent/image?session=" + url.QueryEscape(clientSession) + "&path=" + url.QueryEscape(p)
				}
			}
			for _, y := range x {
				walk(y)
			}
		}
	}
	walk(v)
}

func fileURLPath(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.Path
}

// agentModels lists the models opencode knows for a provider by running
// `opencode models <id>` in the same isolated environment as a run. The list
// comes from the provider's catalog (models.dev for built-ins, the provider
// config for custom ones), so no credential is required beyond what a run needs.
func (a *App) agentModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	providerID := strings.TrimSpace(r.URL.Query().Get("provider"))
	p, ok := providerByID(providerID)
	if !ok {
		http.Error(w, "unknown provider", 400)
		return
	}
	if p.ID == "opencode" {
		jsonOut(w, map[string]any{"provider": providerID, "models": zenModels})
		return
	}
	modelID := a.configuredModel(providerID)
	if modelID == "" {
		modelID = p.DefaultModel
	}
	empty := func(note string) {
		jsonOut(w, map[string]any{"provider": providerID, "models": []string{}, "note": note})
	}
	key := ""
	if p.AuthMode == "key" {
		var configured bool
		key, configured = a.credential(providerID)
		if !configured {
			empty("Configure a " + p.Label + " API key to list models")
			return
		}
	} else if !a.hostOpenCodeAuthConfigured(p.OpenCodeID) {
		empty("Connect " + p.Label + " in OpenCode to list models")
		return
	}
	binary, err := exec.LookPath("opencode")
	if err != nil {
		empty("OpenCode is not installed")
		return
	}
	runDir, err := os.MkdirTemp("", "cortex-opencode-config-")
	if err != nil {
		empty("")
		return
	}
	defer os.RemoveAll(runDir)
	configDir := filepath.Join(runDir, "config")
	dataDir := filepath.Join(runDir, "data")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		empty("")
		return
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		empty("")
		return
	}
	if p.AuthMode == "opencode-auth" {
		if err := copyOpenCodeAuth(dataDir); err != nil {
			empty("")
			return
		}
	}
	cfg, err := cortexOpenCodeConfig(p, modelID)
	if err != nil {
		empty("")
		return
	}
	configPath := filepath.Join(configDir, "opencode.json")
	if err := os.WriteFile(configPath, cfg, 0600); err != nil {
		empty("")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "models", p.OpenCodeID)
	env := append([]string{}, os.Environ()...)
	if p.AuthMode == "key" {
		env = setEnv(env, "CORTEX_PROVIDER_API_KEY", key)
	}
	env = setEnv(env, "OPENCODE_CONFIG", configPath)
	env = setEnv(env, "OPENCODE_CONFIG_DIR", configDir)
	env = setEnv(env, "XDG_CONFIG_HOME", configDir)
	env = setEnv(env, "XDG_DATA_HOME", dataDir)
	env = setEnv(env, "OPENCODE_DISABLE_AUTOUPDATE", "1")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		note := a.redactSecrets(strings.TrimSpace(string(out)))
		if note == "" {
			note = errText(err)
		}
		empty(note)
		return
	}
	jsonOut(w, map[string]any{"provider": providerID, "models": parseModelLines(out, p.OpenCodeID)})
}

func parseModelLines(out []byte, providerID string) []string {
	models := []string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, providerID+"/") {
			continue
		}
		id := strings.TrimPrefix(line, providerID+"/")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
		if len(models) >= 500 {
			break
		}
	}
	return models
}

// agentRunArgs builds the opencode run argv. The prompt must follow the "--"
// separator: "--file" is an array option in opencode run, so bare words placed
// after it would otherwise be consumed as further file paths.
func agentRunArgs(workspace, modelRef, session string, files []string, prompt string) []string {
	args := []string{"--print-logs", "--log-level", "INFO", "run", "--format", "json", "--auto", "--dir", workspace, "--model", modelRef}
	if s := strings.TrimSpace(session); s != "" {
		args = append(args, "--session", s)
	}
	for _, p := range files {
		args = append(args, "--file", p)
	}
	return append(args, "--", prompt)
}

func writeImageAttachments(imgs []imageUpload) (dir string, paths []string, err error) {
	dir, err = os.MkdirTemp("", "cortex-images-")
	if err != nil {
		return "", nil, err
	}
	for _, img := range imgs {
		path, perr := writeImageAttachment(dir, img)
		if perr != nil {
			os.RemoveAll(dir)
			return "", nil, perr
		}
		paths = append(paths, path)
	}
	return dir, paths, nil
}

func writeImageAttachment(dir string, img imageUpload) (string, error) {
	const prefix = "data:image/"
	if len(img.Name) > 500 {
		return "", errors.New("image name is too long")
	}
	if !strings.HasPrefix(img.Data, prefix) {
		return "", errors.New("unsupported image format")
	}
	comma := strings.IndexByte(img.Data, ',')
	if comma < 0 {
		return "", errors.New("invalid image data")
	}
	meta := img.Data[len(prefix):comma]
	var ext string
	switch {
	case strings.HasPrefix(meta, "png;"):
		ext = ".png"
	case strings.HasPrefix(meta, "jpeg;") || strings.HasPrefix(meta, "jpg;"):
		ext = ".jpg"
	case strings.HasPrefix(meta, "gif;"):
		ext = ".gif"
	case strings.HasPrefix(meta, "webp;"):
		ext = ".webp"
	default:
		return "", errors.New("unsupported image type")
	}
	raw, err := base64.StdEncoding.DecodeString(img.Data[comma+1:])
	if err != nil {
		return "", errors.New("invalid image encoding")
	}
	if len(raw) == 0 {
		return "", errors.New("empty image")
	}
	if len(raw) > maxImageBytes {
		return "", errors.New("image exceeds the 10 MiB limit")
	}
	path := filepath.Join(dir, randomToken(12)+ext)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return "", err
	}
	return path, nil
}

func cortexOpenCodeConfig(provider Provider, modelID string) ([]byte, error) {
	cfg := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"model":      modelRefFor(provider, modelID),
		"permission": "allow",
	}
	if provider.AuthMode == "key" {
		if provider.ID == "opencode" {
			cfg["provider"] = zenProviderConfig(modelID)
		} else {
			cfg["provider"] = map[string]any{provider.OpenCodeID: map[string]any{
				"options": map[string]any{"apiKey": "{env:CORTEX_PROVIDER_API_KEY}"},
			}}
		}
	}
	return json.Marshal(cfg)
}

// zenModels is the OpenCode Zen catalogue, snapshot from
// https://opencode.ai/zen/v1/models on 2026-08-29. Zen serves each model family
// through a different protocol, so the catalogue is split by family and the
// generated config declares one provider entry per family (see zenProviderConfig).
// Unknown or newer IDs can still be typed manually; they route to the
// OpenAI-compatible chat family. Refresh this list with
// `curl https://opencode.ai/zen/v1/models` when needed.
var (
	zenOpenAIModels = []string{
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.5",
		"gpt-5.5-pro",
		"gpt-5.4",
		"gpt-5.4-pro",
		"gpt-5.4-mini",
		"gpt-5.4-nano",
		"gpt-5.3-codex-spark",
		"gpt-5.3-codex",
		"gpt-5.2",
		"gpt-5.2-codex",
		"gpt-5.1",
		"gpt-5.1-codex-max",
		"gpt-5.1-codex",
		"gpt-5.1-codex-mini",
		"gpt-5",
		"gpt-5-codex",
		"gpt-5-nano",
		"grok-build-0.1",
		"grok-4.6",
		"grok-4.5",
		"muse-spark-1.2",
	}
	zenAnthropicModels = []string{
		"claude-fable-5",
		"claude-opus-5",
		"claude-opus-4-8",
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-opus-4-5",
		"claude-sonnet-5",
		"claude-sonnet-4-6",
		"claude-sonnet-4-5",
		"claude-sonnet-4",
		"claude-haiku-4-5",
	}
	zenGoogleModels = []string{
		"gemini-3.6-flash",
		"gemini-3.7-flash",
		"gemini-3.5-flash-lite",
		"gemini-3.5-flash",
		"gemini-3.1-pro",
		"gemini-3-flash",
	}
	zenChatModels = []string{
		"deepseek-v4-pro",
		"deepseek-v4-flash",
		"glm-5.2",
		"glm-5.1",
		"glm-5",
		"minimax-m3",
		"minimax-m2.7",
		"minimax-m2.5",
		"kimi-k3",
		"kimi-k2.7-code",
		"kimi-k2.6",
		"kimi-k2.5",
		"qwen3.6-plus",
		"qwen3.5-plus",
		"big-pickle",
		"deepseek-v4-flash-free",
		"muse-spark-1.2-contributor-free",
		"mimo-v2.5-free",
		"hy3-free",
		"ling-3.0-flash-fin-free",
		"nemotron-3-ultra-free",
		"nemotron-3.5-lightning-free",
		"laguna-s-2.1-free",
	}
	zenModels = append(append(append(append([]string{}, zenOpenAIModels...), zenAnthropicModels...), zenGoogleModels...), zenChatModels...)
)

// zenProviderKey maps a Zen model ID to the generated provider entry that
// speaks the protocol that model family needs on the Zen gateway.
func zenProviderKey(modelID string) string {
	switch {
	case strings.HasPrefix(modelID, "gpt-"), strings.HasPrefix(modelID, "grok-"), modelID == "muse-spark-1.2":
		return "opencode"
	case strings.HasPrefix(modelID, "claude-"):
		return "opencode-anthropic"
	case strings.HasPrefix(modelID, "gemini-"):
		return "opencode-google"
	default:
		return "opencode-chat"
	}
}

func zenProviderConfig(selected string) map[string]any {
	providers := map[string]any{}
	addFamily := func(key, npm string, models []string) {
		m := make(map[string]any, len(models)+1)
		for _, id := range models {
			m[id] = map[string]any{"name": id}
		}
		if zenProviderKey(selected) == key {
			m[selected] = map[string]any{"name": selected}
		}
		providers[key] = map[string]any{
			"npm":     npm,
			"name":    "OpenCode Zen",
			"options": map[string]any{"apiKey": "{env:CORTEX_PROVIDER_API_KEY}", "baseURL": "https://opencode.ai/zen/v1"},
			"models":  m,
		}
	}
	addFamily("opencode", "@ai-sdk/openai", zenOpenAIModels)
	addFamily("opencode-anthropic", "@ai-sdk/anthropic", zenAnthropicModels)
	addFamily("opencode-google", "@ai-sdk/google", zenGoogleModels)
	addFamily("opencode-chat", "@ai-sdk/openai-compatible", zenChatModels)
	return providers
}

func modelRefFor(provider Provider, modelID string) string {
	if provider.ID == "opencode" {
		return zenProviderKey(modelID) + "/" + modelID
	}
	return provider.OpenCodeID + "/" + modelID
}

func copyOpenCodeAuth(dataDir string) error {
	src := hostOpenCodeAuthPath()
	if src == "" {
		return errors.New("OpenCode auth path unavailable")
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	dstDir := filepath.Join(dataDir, "opencode")
	if err := os.MkdirAll(dstDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dstDir, "auth.json"), b, 0600)
}

func eventText(raw map[string]any) string {
	if raw["type"] != "text" {
		return ""
	}
	part, _ := raw["part"].(map[string]any)
	s, _ := part["text"].(string)
	return strings.TrimSpace(s)
}
func recoverSession(ctx context.Context, binary, id, workspace, clientSession string, env []string) (string, []map[string]string, uint64, uint64, float64, error) {
	cmd := exec.CommandContext(ctx, binary, "export", id)
	cmd.Dir = workspace
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", nil, 0, 0, 0, errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", nil, 0, 0, 0, err
	}
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		return "", nil, 0, 0, 0, errors.New("session export did not contain JSON")
	}
	var v any
	if err := json.Unmarshal(out[start:], &v); err != nil {
		return "", nil, 0, 0, 0, err
	}
	rewriteImageURLs(v, clientSession)
	texts := assistantTexts(v)
	images := assistantImages(v)
	var i, o uint64
	var c float64
	collectUsage(v, &i, &o, &c)
	return strings.Join(texts, "\n\n"), images, i, o, c, nil
}
func assistantImages(v any) []map[string]string {
	out := []map[string]string{}
	seen := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			role, _ := x["role"].(string)
			if info, ok := x["info"].(map[string]any); ok && role == "" {
				role, _ = info["role"].(string)
			}
			if role == "assistant" {
				if parts, ok := x["parts"].([]any); ok {
					for _, y := range parts {
						p, _ := y.(map[string]any)
						if p == nil {
							continue
						}
						if p["type"] == "file" || p["type"] == "image" {
							if m, _ := p["mediaType"].(string); strings.HasPrefix(strings.ToLower(m), "image/") {
								u, _ := p["url"].(string)
								n, _ := p["filename"].(string)
								if u != "" && !seen[u] {
									seen[u] = true
									out = append(out, map[string]string{"url": u, "name": n})
								}
							}
						}
						if p["type"] == "tool" {
							if state, ok := p["state"].(map[string]any); ok {
								if atts, ok := state["attachments"].([]any); ok {
									for _, a := range atts {
										am, _ := a.(map[string]any)
										if am == nil {
											continue
										}
										if m, _ := am["mime"].(string); strings.HasPrefix(strings.ToLower(m), "image/") {
											u, _ := am["url"].(string)
											if u != "" && !seen[u] {
												seen[u] = true
												out = append(out, map[string]string{"url": u})
											}
										}
									}
								}
							}
						}
					}
					return
				}
			}
			for _, y := range x {
				walk(y)
			}
		}
	}
	walk(v)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}
func assistantTexts(v any) []string {
	out := []string{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			role, _ := x["role"].(string)
			if info, ok := x["info"].(map[string]any); ok && role == "" {
				role, _ = info["role"].(string)
			}
			if role == "assistant" {
				if parts, ok := x["parts"].([]any); ok {
					for _, y := range parts {
						p, _ := y.(map[string]any)
						if p["type"] == "text" {
							if t, _ := p["text"].(string); strings.TrimSpace(t) != "" {
								out = append(out, t)
							}
						}
					}
					return
				}
			}
			for _, y := range x {
				walk(y)
			}
		}
	}
	walk(v)
	return out
}
func collectUsage(v any, input, output *uint64, cost *float64) {
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			collectUsage(x, input, output, cost)
		}
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	// OpenCode session exports record step-finish usage as:
	// "tokens": {"input": ..., "output": ..., ...}, with cost beside it.
	if tokens, ok := m["tokens"].(map[string]any); ok {
		if n, ok := number(tokens["input"]); ok {
			*input += uint64(n)
		}
		if n, ok := number(tokens["output"]); ok {
			*output += uint64(n)
		}
	}
	for k, x := range m {
		lk := strings.ToLower(k)
		switch lk {
		case "inputtokens", "input_tokens":
			if n, ok := number(x); ok {
				*input += uint64(n)
			}
		case "outputtokens", "output_tokens":
			if n, ok := number(x); ok {
				*output += uint64(n)
			}
		case "cost", "costusd", "cost_usd":
			if n, ok := number(x); ok {
				*cost += n
			}
		}
		collectUsage(x, input, output, cost)
	}
}
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, e := n.Float64()
		return f, e == nil
	case string:
		f, e := strconv.ParseFloat(n, 64)
		return f, e == nil
	}
	return 0, false
}
func sanitize(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	if len(s) > 12000 {
		s = s[len(s)-12000:]
	}
	return s
}
