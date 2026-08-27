package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	maxProviderLine   = 1 << 20
	maxProviderBytes  = 32 << 20
	maxProviderEvents = 4096
)

type agentRunRequest struct {
	Workspace     string `json:"workspace"`
	Prompt        string `json:"prompt"`
	Session       string `json:"session,omitempty"`
	ClientSession string `json:"clientSession,omitempty"`
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
		"model":               p.OpenCodeID + "/" + model,
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
	if !decode(w, r, &q) {
		return
	}
	q.Prompt = strings.TrimSpace(q.Prompt)
	if q.Prompt == "" {
		http.Error(w, "prompt required", 400)
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
	modelRef := provider.OpenCodeID + "/" + modelID
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
	args := []string{"--print-logs", "--log-level", "INFO", "run", "--format", "json", "--auto", "--dir", workspace, "--model", modelRef}
	if strings.TrimSpace(q.Session) != "" {
		args = append(args, "--session", strings.TrimSpace(q.Session))
	}
	args = append(args, q.Prompt)
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
		if recovered, ri, ro, rc, e := recoverSession(ctx, binary, sessionID, workspace, env); e == nil {
			if !sawText && strings.TrimSpace(recovered) != "" {
				writeEvent(w, flusher, "recovered", map[string]any{"text": recovered, "sessionID": sessionID})
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
func cortexOpenCodeConfig(provider Provider, modelID string) ([]byte, error) {
	modelRef := provider.OpenCodeID + "/" + modelID
	cfg := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"model":      modelRef,
		"permission": "allow",
	}
	if provider.AuthMode == "key" {
		options := map[string]any{"apiKey": "{env:CORTEX_PROVIDER_API_KEY}"}
		entry := map[string]any{"options": options}
		if provider.ID == "opencode" {
			entry["npm"] = "@ai-sdk/openai-compatible"
			entry["name"] = "OpenCode Zen"
			options["baseURL"] = "https://opencode.ai/zen/v1"
			entry["models"] = map[string]any{modelID: map[string]any{"name": modelID}}
		}
		cfg["provider"] = map[string]any{provider.OpenCodeID: entry}
	}
	return json.Marshal(cfg)
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
func recoverSession(ctx context.Context, binary, id, workspace string, env []string) (string, uint64, uint64, float64, error) {
	cmd := exec.CommandContext(ctx, binary, "export", id)
	cmd.Dir = workspace
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", 0, 0, 0, errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", 0, 0, 0, err
	}
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		return "", 0, 0, 0, errors.New("session export did not contain JSON")
	}
	var v any
	if err := json.Unmarshal(out[start:], &v); err != nil {
		return "", 0, 0, 0, err
	}
	texts := assistantTexts(v)
	var i, o uint64
	var c float64
	collectUsage(v, &i, &o, &c)
	return strings.Join(texts, "\n\n"), i, o, c, nil
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
