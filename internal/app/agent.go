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

const defaultModel = "opencode/deepseek-v4-flash"

type agentRunRequest struct {
	Workspace string `json:"workspace"`
	Prompt    string `json:"prompt"`
	Session   string `json:"session,omitempty"`
}

func (a *App) agentStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method", 405)
		return
	}
	a.mu.RLock()
	provider := a.settings.ActiveProvider
	a.mu.RUnlock()
	_, configured := a.credential(provider)
	_, err := exec.LookPath("opencode")
	jsonOut(w, map[string]any{"available": err == nil && configured && provider == "opencode", "opencodeInstalled": err == nil, "credentialAvailable": configured, "provider": provider, "model": defaultModel, "note": "Cortex v0.1 agent execution uses OpenCode Zen; additional stored providers are reserved for upcoming model/provider selection."})
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
	provider := a.settings.ActiveProvider
	a.mu.RUnlock()
	if provider != "opencode" {
		http.Error(w, "Cortex v0.1 agent execution currently uses OpenCode Zen; select OpenCode Zen in Settings", 400)
		return
	}
	key, ok := a.credential("opencode")
	if !ok {
		http.Error(w, "OpenCode Zen credential is not configured", 400)
		return
	}
	binary, err := exec.LookPath("opencode")
	if err != nil {
		http.Error(w, "OpenCode is not installed or not in Cortex's PATH", 503)
		return
	}
	runDir, err := os.MkdirTemp("", "cortex-opencode-")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer os.RemoveAll(runDir)
	configDir := filepath.Join(runDir, "config")
	dataDir := filepath.Join(runDir, "data")
	os.MkdirAll(configDir, 0700)
	os.MkdirAll(dataDir, 0700)
	cfg := []byte(`{"$schema":"https://opencode.ai/config.json","model":"opencode/deepseek-v4-flash","permission":"allow","provider":{"opencode":{"npm":"@ai-sdk/openai-compatible","name":"OpenCode Zen","options":{"baseURL":"https://opencode.ai/zen/v1","apiKey":"{env:OPENCODE_API_KEY}"},"models":{"deepseek-v4-flash":{"name":"DeepSeek V4 Flash"}}}}}`)
	configPath := filepath.Join(configDir, "opencode.json")
	if err := os.WriteFile(configPath, cfg, 0600); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	args := []string{"--print-logs", "--log-level", "INFO", "run", "--format", "json", "--auto", "--dir", workspace, "--model", defaultModel}
	if strings.TrimSpace(q.Session) != "" {
		args = append(args, "--session", strings.TrimSpace(q.Session))
	}
	args = append(args, q.Prompt)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workspace
	env := append([]string{}, os.Environ()...)
	env = setEnv(env, "OPENCODE_API_KEY", key)
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
	errCh := make(chan string, 1)
	go func() { b, _ := readLimit(stderr, 256<<10); errCh <- string(b) }()
	var input, output uint64
	var cost float64
	sessionID := ""
	sawText := false
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64<<10), 4<<20)
	for scan.Scan() {
		line := append([]byte(nil), scan.Bytes()...)
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
	if waitErr == nil && !sawText && sessionID != "" {
		if recovered, ri, ro, rc, e := recoverSession(ctx, binary, sessionID, workspace, env); e == nil && strings.TrimSpace(recovered) != "" {
			writeEvent(w, flusher, "recovered", map[string]any{"text": recovered, "sessionID": sessionID})
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
		msg := sanitize(strings.TrimSpace(stderrText), key)
		if msg == "" {
			msg = waitErr.Error()
		}
		writeEvent(w, flusher, "error", map[string]any{"message": msg})
		return
	}
	writeEvent(w, flusher, "done", map[string]any{"inputTokens": input, "outputTokens": output, "estimatedCostUsd": cost, "sessionID": sessionID})
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
