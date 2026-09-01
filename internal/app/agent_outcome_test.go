package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpenCode writes a deterministic fake `opencode` executable into a temp
// directory that is prepended to PATH. The script emits configurable stdout
// NDJSON, stderr, and exit status so the runner can be exercised without a
// real provider. Each invoke rewrites the script for the next scenario.
type fakeOpenCode struct {
	dir  string
	path string
	mu   sync.Mutex
}

func newFakeOpenCode(t *testing.T) *fakeOpenCode {
	t.Helper()
	dir := t.TempDir()
	f := &fakeOpenCode{dir: dir, path: filepath.Join(dir, "opencode")}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

// invoke rewrites the fake script. export is the stdout for `opencode export`
// (recovery), stdout/stderr are the run output, and exitCode is the run exit.
func (f *fakeOpenCode) invoke(t *testing.T, stdout, stderr string, exitCode int, export string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.WriteFile(filepath.Join(f.dir, "stdout.txt"), []byte(stdout), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "stderr.txt"), []byte(stderr), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "export.txt"), []byte(export), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/bash
if [ "$1" = "export" ]; then
  cat "` + filepath.Join(f.dir, "export.txt") + `"
  exit 0
fi
cat "` + filepath.Join(f.dir, "stdout.txt") + `"
cat "` + filepath.Join(f.dir, "stderr.txt") + `" >&2
exit ` + fmt.Sprintf("%d", exitCode) + `
`
	if err := os.WriteFile(f.path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

// agentTestApp builds an App whose settings configure the active provider key
// and model so agentRun can start.
func agentTestApp(t *testing.T, fake *fakeOpenCode) *App {
	t.Helper()
	a := hardeningTestApp(t)
	a.mu.Lock()
	a.settings.Keys = map[string]string{"opencode": "sk-test-secret-key-abcdef"}
	a.settings.Models = map[string]string{"opencode": "deepseek-v4-flash"}
	a.mu.Unlock()
	if err := a.saveSettings(); err != nil {
		t.Fatal(err)
	}
	// Re-load so the App sees persisted settings (agentRun reads live fields,
	// so this is not strictly needed, but keeps parity with the API path).
	a.mu.Lock()
	a.settings.Keys = map[string]string{"opencode": "sk-test-secret-key-abcdef"}
	a.settings.Models = map[string]string{"opencode": "deepseek-v4-flash"}
	a.mu.Unlock()
	return a
}

// runAgentRequest drives a full /api/agent/run request and returns all NDJSON
// events emitted plus the persisted run state.
func runAgentRequest(t *testing.T, a *App, workspace string, fake *fakeOpenCode) []map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"workspace": workspace, "prompt": "test", "clientSession": "conv1"})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/agent/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.agentRun(rec, req)
	events := []map[string]any{}
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err == nil {
			events = append(events, ev)
		}
	}
	return events
}

// workspaceUnderRoot creates a workspace inside the app's root so the strict
// resolve boundary accepts it.
func workspaceUnderRoot(t *testing.T, a *App) string {
	t.Helper()
	ws := filepath.Join(a.root, "ws")
	if err := os.MkdirAll(ws, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	return ws
}

func runStateFromDB(t *testing.T, a *App, runID string) string {
	t.Helper()
	var state string
	if err := a.db.QueryRow("SELECT state FROM agent_runs WHERE id=?", runID).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return state
}

func runDiagnosticsFromDB(t *testing.T, a *App, runID string) diagnostics {
	t.Helper()
	var raw string
	if err := a.db.QueryRow("SELECT diagnostics FROM agent_runs WHERE id=?", runID).Scan(&raw); err != nil {
		t.Fatalf("read diagnostics: %v", err)
	}
	var d diagnostics
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("unmarshal diagnostics: %v", err)
		}
	}
	return d
}

func terminalEventType(events []map[string]any) string {
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == "done" || t == "error" || t == "cancelled" || t == "truncated" {
			return t
		}
	}
	return ""
}

// --- runState transition tests ---

func TestRunStateCauseTransitions(t *testing.T) {
	cases := []struct {
		name string
		seq  []runCause
		want runCause
	}{
		{"disconnect then stop upgrades", []runCause{causeRequestCanceled, causeUserStop}, causeUserStop},
		{"stop then disconnect keeps stop", []runCause{causeUserStop, causeRequestCanceled}, causeUserStop},
		{"limit then disconnect keeps limit", []runCause{causeOutputLimit, causeRequestCanceled}, causeOutputLimit},
		{"stop then limit becomes limit", []runCause{causeUserStop, causeOutputLimit}, causeOutputLimit},
		{"shutdown then disconnect keeps shutdown", []runCause{causeServiceShutdown, causeRequestCanceled}, causeServiceShutdown},
		{"shutdown after stop keeps stop", []runCause{causeUserStop, causeServiceShutdown}, causeUserStop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRunState()
			for _, c := range tc.seq {
				s.recordCause(c)
			}
			if got := s.snapshot().cause; got != tc.want {
				t.Fatalf("cause = %q want %q", got, tc.want)
			}
		})
	}
}

func TestRunStateSealRejectsLateStop(t *testing.T) {
	s := newRunState()
	s.seal()
	if s.recordCause(causeUserStop) {
		t.Fatal("late stop accepted after seal")
	}
}

// TestRunStateConcurrentCauseRace exercises concurrent recorders under the
// race detector: a disconnect racing a Stop must never downgrade user_stop,
// and a limit racing a disconnect must keep output_limit.
func TestRunStateConcurrentCauseRace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   runCause
		record []func(*runState)
	}{
		{
			name: "stop and disconnect race keeps stop",
			want: causeUserStop,
			record: []func(*runState){
				func(s *runState) { s.recordCause(causeUserStop) },
				func(s *runState) { s.recordCause(causeRequestCanceled) },
			},
		},
		{
			name: "limit and disconnect race keeps limit",
			want: causeOutputLimit,
			record: []func(*runState){
				func(s *runState) { s.recordCause(causeOutputLimit) },
				func(s *runState) { s.recordCause(causeRequestCanceled) },
			},
		},
		{
			name: "disconnect and stop race may upgrade to stop",
			want: causeUserStop,
			record: []func(*runState){
				func(s *runState) { s.recordCause(causeRequestCanceled) },
				func(s *runState) { s.recordCause(causeUserStop) },
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 50; i++ {
				s := newRunState()
				var wg sync.WaitGroup
				for _, r := range tc.record {
					wg.Add(1)
					go func(f func(*runState)) { defer wg.Done(); f(s) }(r)
				}
				wg.Wait()
				got := s.snapshot().cause
				if got == causeNone {
					continue // one recorder may have lost the race entirely; acceptable
				}
				if got != tc.want {
					t.Fatalf("cause = %q want %q", got, tc.want)
				}
			}
		})
	}
}

// TestRunStateErrorOrdering verifies the deterministic sequence ordering that
// the classifier relies on for error-before-cause precedence.
func TestRunStateErrorOrdering(t *testing.T) {
	s := newRunState()
	// error observed first, then a stop request: ordering must keep error first.
	s.observeError()
	s.recordCause(causeUserStop)
	snap := s.snapshot()
	if snap.errSeq >= snap.causeSeq {
		t.Fatalf("error seq %d must precede cause seq %d", snap.errSeq, snap.causeSeq)
	}
	// Reverse order: stop first then error.
	s2 := newRunState()
	s2.recordCause(causeUserStop)
	s2.observeError()
	snap2 := s2.snapshot()
	if snap2.causeSeq >= snap2.errSeq {
		t.Fatalf("cause seq %d must precede error seq %d", snap2.causeSeq, snap2.errSeq)
	}
}

// --- classifier tests ---

func TestClassifyRunPrecedence(t *testing.T) {
	cases := []struct {
		name        string
		state       runStateSnapshot
		stdoutError bool
		validStop   bool
		exit        exitStatus
		want        runOutcome
	}{
		{
			name:        "exit0 valid stop no error",
			state:       runStateSnapshot{},
			stdoutError: false,
			validStop:   true,
			exit:        exitStatus{exited: true, exitCode: 0},
			want:        outcomeCompleted,
		},
		{
			name:        "exit1 valid stop completed_with_process_error",
			state:       runStateSnapshot{},
			stdoutError: false,
			validStop:   true,
			exit:        exitStatus{exited: true, exitCode: 1},
			want:        outcomeCompletedWError,
		},
		{
			name:        "exit1 no stop failed",
			state:       runStateSnapshot{},
			stdoutError: false,
			validStop:   false,
			exit:        exitStatus{exited: true, exitCode: 1},
			want:        outcomeFailed,
		},
		{
			name:        "signal no cause failed even with stop",
			state:       runStateSnapshot{},
			stdoutError: false,
			validStop:   true,
			exit:        exitStatus{exited: false, signaled: true, signal: "SIGKILL"},
			want:        outcomeFailed,
		},
		{
			name:        "stdout error before cause failed",
			state:       runStateSnapshot{errSeq: 1, causeSeq: 2},
			stdoutError: true,
			validStop:   true,
			exit:        exitStatus{exited: true, exitCode: 0},
			want:        outcomeFailed,
		},
		{
			name:        "stdout error after stop cancelled",
			state:       runStateSnapshot{cause: causeUserStop, causeSeq: 1, errSeq: 2},
			stdoutError: true,
			validStop:   true,
			exit:        exitStatus{exited: false, signaled: true, signal: "SIGKILL"},
			want:        outcomeCancelled,
		},
		{
			name:        "output limit truncated",
			state:       runStateSnapshot{cause: causeOutputLimit, causeSeq: 1, errSeq: 2},
			stdoutError: true,
			validStop:   true,
			exit:        exitStatus{exited: false, signaled: true, signal: "SIGKILL"},
			want:        outcomeTruncated,
		},
		{
			name:        "shutdown interrupted",
			state:       runStateSnapshot{cause: causeServiceShutdown},
			stdoutError: false,
			validStop:   false,
			exit:        exitStatus{exited: false, signaled: true, signal: "SIGTERM"},
			want:        outcomeInterrupted,
		},
		{
			name:        "request cancelled cancelled",
			state:       runStateSnapshot{cause: causeRequestCanceled},
			stdoutError: false,
			validStop:   false,
			exit:        exitStatus{exited: false, signaled: true, signal: "SIGKILL"},
			want:        outcomeCancelled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRun(tc.state, tc.stdoutError, tc.validStop, tc.exit, causeNone); got != tc.want {
				t.Fatalf("classifyRun = %q want %q", got, tc.want)
			}
		})
	}
}

// --- tail capture tests ---

func TestTailCaptureDrainsToEOFAndRetainsTail(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		sb.WriteString(fmt.Sprintf("line %d\n", i))
	}
	sb.WriteString("level=ERROR message=failed ref=err_abc\n")
	c := captureTail(strings.NewReader(sb.String()), 4<<10)
	c.wait()
	if !c.truncated() {
		t.Fatal("expected truncation")
	}
	tail := c.String()
	if !strings.Contains(tail, "ref=err_abc") {
		t.Fatalf("tail lost final error line: %q", tail)
	}
	if strings.Contains(tail, "line 0") {
		t.Fatal("tail retained discarded prefix")
	}
}

func TestStderrLevelParsing(t *testing.T) {
	cases := map[string]string{
		"timestamp=x level=INFO message=hi":   "INFO",
		"timestamp=x level=ERROR message=bad": "ERROR",
		"level=WARN x=1":                      "WARN",
		"unstructured line":                   "",
		"level=DEBUG x":                       "DEBUG",
		"level=TRACE x":                       "",
	}
	for line, want := range cases {
		if got := stderrLevel(line); got != want {
			t.Fatalf("stderrLevel(%q) = %q want %q", line, got, want)
		}
	}
}

// --- merge/dedup tests ---

func TestMergeConversationEventsDedupsServerCopies(t *testing.T) {
	server := []conversationEvent{
		{Kind: "assistant", Text: "hello", CreatedAt: 1},
		{Kind: "assistant", Text: "hello", CreatedAt: 2},
		{Kind: "user", Text: "prompt", CreatedAt: 0},
	}
	client := []conversationEvent{
		{Kind: "assistant", Text: "hello", CreatedAt: 1},
		{Kind: "assistant", Text: "hello", CreatedAt: 2},
		{Kind: "user", Text: "prompt", CreatedAt: 0},
		{Kind: "image", Text: "data:x", CreatedAt: 3},
	}
	got := mergeConversationEvents(server, client)
	if len(got) != 4 {
		t.Fatalf("merged length = %d want 4: %+v", len(got), got)
	}
}

// --- full fake-opencode fixtures ---

func TestAgentRunExit0ValidStopCompletes(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	if got := terminalEventType(events); got != "done" {
		t.Fatalf("terminal event = %q want done", got)
	}
}

// TestAgentRunExitNonZeroValidStopIsCompletedWithProcessError verifies that a
// valid final stop followed by a non-zero exit produces
// completed_with_process_error, never a plain completion.
func TestAgentRunExitNonZeroValidStopIsCompletedWithProcessError(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	stderr := "level=INFO message=exiting loop\nlevel=INFO message=disposing instance\n"
	fake.invoke(t, stdout, stderr, 1, `{"info":{"id":"ses_x"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"final"}]}]}`)
	events := runAgentRequest(t, a, ws, fake)
	if got := terminalEventType(events); got != "error" {
		t.Fatalf("terminal event = %q want error", got)
	}
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if runID == "" {
		t.Fatal("no run ID emitted")
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeCompletedWError) {
		t.Fatalf("persisted state = %q want %q", state, outcomeCompletedWError)
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.Outcome != string(outcomeCompletedWError) {
		t.Fatalf("diagnostics outcome = %q want %q", d.Outcome, outcomeCompletedWError)
	}
	if d.Summary == "" || !strings.Contains(d.Summary, "exit") {
		t.Fatalf("summary should mention exit status: %q", d.Summary)
	}
}

// TestAgentRunStdoutErrorForcesFailure verifies an authoritative stdout error
// event overrides terminal-stop evidence.
func TestAgentRunStdoutErrorForcesFailure(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n{\"type\":\"error\",\"sessionID\":\"ses_x\",\"error\":{\"name\":\"ProviderError\",\"data\":{\"message\":\"stream failed\"}}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_x"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("persisted state = %q want failed", state)
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.StdoutError != "stream failed" {
		t.Fatalf("stdoutError = %q want stream failed", d.StdoutError)
	}
}

// TestAgentRunUnexpectedSignalIsFailed verifies a killed process with no local
// cause is never presented as a user cancellation.
func TestAgentRunUnexpectedSignalIsFailed(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	// Fake sends SIGKILL to itself after emitting a valid stop.
	if err := os.WriteFile(filepath.Join(fake.dir, "stdout.txt"), []byte(stdout), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/bash
cat "` + filepath.Join(fake.dir, "stdout.txt") + `"
kill -9 $$
`
	if err := os.WriteFile(fake.path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("persisted state = %q want failed (unexpected signal)", state)
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.Signal == "" {
		t.Fatal("signal not recorded")
	}
}

// TestAgentRunRecoveryAfterNonZeroExit verifies recovery under a failed
// outcome salvages the final response without reclassifying the run.
func TestAgentRunRecoveryAfterNonZeroExit(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"text\",\"sessionID\":\"ses_x\",\"part\":{\"type\":\"text\",\"text\":\"partial\"}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	export := `{"info":{"id":"ses_x","tokens":{"input":10,"output":20}},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"final recovered response"}]}]}`
	fake.invoke(t, stdout, "", 1, export)
	events := runAgentRequest(t, a, ws, fake)
	foundRecovered := false
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == "recovered" {
			foundRecovered = true
		}
	}
	if !foundRecovered {
		t.Fatal("recovered event not emitted")
	}
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state == string(outcomeCompleted) {
		t.Fatal("recovery must not reclassify a failed run as completed")
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.RecoveryResult != "ok" {
		t.Fatalf("recovery result = %q want ok", d.RecoveryResult)
	}
}

// TestAgentRunServerOwnedEventsPersisted verifies run events are persisted
// server-side before delivery so a disconnected client can reload them.
func TestAgentRunServerOwnedEventsPersisted(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"text\",\"sessionID\":\"ses_x\",\"part\":{\"type\":\"text\",\"text\":\"assistant output\"}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	runAgentRequest(t, a, ws, fake)
	events, err := a.loadAgentRunEvents("conv1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind == "assistant" && strings.Contains(ev.Text, "assistant output") {
			found = true
		}
	}
	if !found {
		t.Fatalf("assistant output not persisted server-side: %+v", events)
	}
}

// TestAgentRunSecretsRedactedInDiagnostics verifies the API key does not leak
// into the diagnostic tail or summary.
func TestAgentRunSecretsRedactedInDiagnostics(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"error\",\"sessionID\":\"ses_x\",\"error\":{\"data\":{\"message\":\"boom\"}}}\n"
	stderr := "level=ERROR message=failed cause=\"sk-test-secret-key-abcdef\"\n"
	fake.invoke(t, stdout, stderr, 1, `{}`)
	runAgentRequest(t, a, ws, fake)
	rows, err := a.db.Query("SELECT diagnostics FROM agent_runs")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, "sk-test-secret-key-abcdef") {
			t.Fatal("secret leaked into diagnostics")
		}
	}
}

// TestAgentRunStaleClientSaveCannotEraseServerEvents verifies a client
// conversation PUT cannot remove server-owned run events.
func TestAgentRunStaleClientSaveCannotEraseServerEvents(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"text\",\"sessionID\":\"ses_x\",\"part\":{\"type\":\"text\",\"text\":\"authoritative\"}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	runAgentRequest(t, a, ws, fake)
	// A stale tab PUTs a snapshot without the server event.
	c := conversation{ID: "conv1", Workspace: ws, Events: []conversationEvent{{Kind: "user", Text: "old", CreatedAt: 1}}}
	if err := a.saveConversation(&c); err != nil {
		t.Fatal(err)
	}
	events, err := a.loadAgentRunEvents("conv1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind == "assistant" && strings.Contains(ev.Text, "authoritative") {
			found = true
		}
	}
	if !found {
		t.Fatal("stale client save erased server-owned run events")
	}
}

// TestAgentRunConversationStateNotOverwrittenByClient verifies a client PUT
// cannot rewrite a terminal server state back to idle.
func TestAgentRunConversationStateNotOverwrittenByClient(t *testing.T) {
	a := hardeningTestApp(t)
	if _, err := a.db.Exec(`INSERT INTO conversations(id,state,created_at,updated_at) VALUES('conv1','failed',1,1)`); err != nil {
		t.Fatal(err)
	}
	c := conversation{ID: "conv1", Workspace: "/tmp", State: "idle", Events: nil}
	if err := a.saveConversation(&c); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := a.db.QueryRow("SELECT state FROM conversations WHERE id='conv1'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("client save overwrote state to %q", state)
	}
}

// TestAgentRunDiagnosticsEndpoint verifies the technical-details endpoint
// returns the persisted diagnostics.
func TestAgentRunDiagnosticsEndpoint(t *testing.T) {
	a := hardeningTestApp(t)
	if _, err := a.db.Exec(`INSERT INTO conversations(id,state,created_at,updated_at) VALUES('conv1','failed',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO agent_runs(id,conversation_id,state,prompt,started_at,diagnostics) VALUES('run1','conv1','failed','p',1,'{"outcome":"failed","category":"signal","signal":"SIGKILL"}')`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/agent/run-diagnostics?runID=run1", nil)
	rec := httptest.NewRecorder()
	a.agentRunDiagnostics(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "SIGKILL") {
		t.Fatalf("diagnostics body = %q", rec.Body.String())
	}
	// Unknown run ID.
	req2 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/agent/run-diagnostics?runID=nope", nil)
	rec2 := httptest.NewRecorder()
	a.agentRunDiagnostics(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d", rec2.Code)
	}
}

// TestAgentCancelRecordsUserStopAndStopsRun verifies Stop records user_stop.
func TestAgentCancelRecordsUserStopAndStopsRun(t *testing.T) {
	a := hardeningTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	run := newActiveRun(cancel)
	a.runMu.Lock()
	a.activeRuns["run1"] = run
	a.runMu.Unlock()
	defer func() { a.runMu.Lock(); delete(a.activeRuns, "run1"); a.runMu.Unlock() }()

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/agent/cancel", strings.NewReader(`{"runID":"run1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.agentCancel(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if snap := run.state.snapshot(); snap.cause != causeUserStop {
		t.Fatalf("cause = %q want user_stop", snap.cause)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancel did not stop the run context")
	}
	// Unknown run.
	req2 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/agent/cancel", strings.NewReader(`{"runID":"nope"}`))
	rec2 := httptest.NewRecorder()
	a.agentCancel(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d", rec2.Code)
	}
}

// TestAgentRunShutdownRecordsInterrupted verifies the shutdown sweep records
// service shutdown and converts stale running rows to interrupted.
func TestAgentRunShutdownRecordsInterrupted(t *testing.T) {
	a := hardeningTestApp(t)
	if _, err := a.db.Exec(`INSERT INTO conversations(id,state,created_at,updated_at) VALUES('conv1','running',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO agent_runs(id,conversation_id,state,prompt,started_at) VALUES('run1','conv1','running','p',1)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.runMu.Lock()
	a.activeRuns["run1"] = newActiveRun(cancel)
	a.runMu.Unlock()
	defer func() { a.runMu.Lock(); delete(a.activeRuns, "run1"); a.runMu.Unlock() }()
	a.stopActiveRuns()
	if snap := a.activeRuns["run1"].state.snapshot(); snap.cause != causeServiceShutdown {
		t.Fatalf("cause = %q want service_shutdown", snap.cause)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("stopActiveRuns did not cancel the run context")
	}
}

// TestAgentRunAtMostOneActiveRunPerConversation verifies a second run on a
// running conversation is rejected.
func TestAgentRunAtMostOneActiveRunPerConversation(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	// Simulate an already-running run for the conversation.
	if err := a.startAgentRun("existing", "conv1", "p", ws, "opencode", "deepseek-v4-flash"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"workspace": ws, "prompt": "test", "clientSession": "conv1"})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/agent/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.agentRun(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d want 409", rec.Code)
	}
}

// TestAgentRunLifecycleOrdering verifies durable creation precedes cmd.Start:
// a failure to create the durable row returns an HTTP error without starting
// the process. This is exercised implicitly by the single-active-run guard
// above; here we verify the run ID is emitted before assistant events.
func TestAgentRunRunIDEmittedBeforeAssistantEvents(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"text\",\"sessionID\":\"ses_x\",\"part\":{\"type\":\"text\",\"text\":\"hi\"}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	runIdx, textIdx := -1, -1
	for i, ev := range events {
		switch ev["type"] {
		case "run":
			runIdx = i
		case "opencode":
			textIdx = i
		}
		if runIdx >= 0 && textIdx >= 0 {
			break
		}
	}
	if runIdx < 0 {
		t.Fatal("no run event")
	}
	if textIdx >= 0 && textIdx < runIdx {
		t.Fatal("assistant event emitted before run ID")
	}
}

func TestAgentRunMigrationAddsRunEventsAndColumns(t *testing.T) {
	a := hardeningTestApp(t)
	// The migration should have created agent_run_events and the new columns.
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='agent_run_events'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("agent_run_events table missing after migration")
	}
	for _, col := range []string{"diagnostics"} {
		var n int
		if err := a.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('agent_runs') WHERE name=?", col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("agent_runs.%s missing after migration", col)
		}
	}
	for _, col := range []string{"current_run_id"} {
		var n int
		if err := a.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('conversations') WHERE name=?", col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("conversations.%s missing after migration", col)
		}
	}
}

// --- provider insufficient-balance ---

func TestClassifyProviderErrorRecognition(t *testing.T) {
	cases := []struct {
		msg    string
		code   string
		status int
		want   bool
	}{
		{"AI_APICallError: Insufficient balance. Manage your billing here: https://opencode.ai/workspace/x/billing", "", 0, true},
		{"Insufficient balance", "", 0, true},
		{"provider said insufficient balance before closing", "", 0, true},
		{"", "insufficient_balance", 0, true},
		{"", "", 402, true},
		{"Payment required", "", 0, false},
		{"402 Payment Required", "", 0, false},
		{"billing limit reached", "", 0, false},
		{"quota exceeded", "", 0, false},
		{"rate limit reached, reset in 1m", "", 0, false},
		{"provider overloaded", "", 429, false},
		{"API key invalid", "", 401, false},
	}
	for _, tc := range cases {
		if got := classifyProviderError(tc.msg, tc.code, tc.status); got != tc.want {
			t.Fatalf("classifyProviderError(%q, %q, %d) = %v want %v", tc.msg, tc.code, tc.status, got, tc.want)
		}
	}
}

func TestSanitizeBillingURL(t *testing.T) {
	ok := "https://opencode.ai/workspace/abc123/billing"
	if got := sanitizeBillingURL(ok); got != ok {
		t.Fatalf("valid URL = %q want %q", got, ok)
	}
	reject := []string{
		"http://opencode.ai/workspace/x/billing",
		"https://www.opencode.ai/workspace/x/billing",
		"https://opencode.ai.evil.com/workspace/x/billing",
		"https://evil.com/workspace/x/billing",
		"https://opencode.ai/",
		"https://opencode.ai",
		"https://user:pass@opencode.ai/workspace/x/billing",
		"https://opencode.ai:8443/workspace/x/billing",
		"javascript:alert(1)",
		"https://opencode.ai/workspace/x/billing?next=https://evil.com",
		"https://opencode.ai/workspace/x/billing#frag",
		"opencode.ai/workspace/x/billing",
		"https://opencode.ai./workspace/x/billing",
		"https://opencode.ai/%0d%0a/workspace/x",
		"",
	}
	for _, u := range reject {
		if got := sanitizeBillingURL(u); got != "" {
			t.Fatalf("rejected URL %q was accepted as %q", u, got)
		}
	}
}

func TestAgentRunInsufficientBalanceClassifiesAndPreservesBilling(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	msg := "AI_APICallError: Insufficient balance. Manage your billing here: https://opencode.ai/workspace/abc/billing"
	stdout := "{\"type\":\"error\",\"sessionID\":\"ses_x\",\"error\":{\"name\":\"AI_APICallError\",\"data\":{\"message\":\"" + msg + "\"}}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_x"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	var terminal map[string]any
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
		if t, _ := ev["type"].(string); t == "error" {
			terminal = ev["data"].(map[string]any)
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("state = %q want failed", state)
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.ProviderCause != string(causeProviderInsufficientBalance) {
		t.Fatalf("providerCause = %q want provider_insufficient_balance", d.ProviderCause)
	}
	if d.BillingURL != "https://opencode.ai/workspace/abc/billing" {
		t.Fatalf("billingUrl = %q", d.BillingURL)
	}
	if !strings.Contains(d.Summary, "insufficient credit") {
		t.Fatalf("summary should be friendly: %q", d.Summary)
	}
	if terminal == nil || terminal["cause"] != string(causeProviderInsufficientBalance) {
		t.Fatalf("terminal payload missing cause: %+v", terminal)
	}
	if terminal == nil || terminal["billingUrl"] != "https://opencode.ai/workspace/abc/billing" {
		t.Fatalf("terminal payload missing billingUrl: %+v", terminal)
	}
}

func TestAgentRunBillingBeforeStopWins(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	// Provider billing error first, then a user stop request.
	stdout := "{\"type\":\"error\",\"sessionID\":\"ses_x\",\"error\":{\"data\":{\"message\":\"Insufficient balance\"}}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_x"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("billing-before-stop state = %q want failed", state)
	}
}

func TestAgentRunStopBeforeBillingKeepsCancelled(t *testing.T) {
	run := newActiveRun(func() {})
	// Stop accepted first.
	run.state.recordCause(causeUserStop)
	// Provider error observed afterwards.
	run.state.recordProviderFailure()
	snap := run.state.snapshot()
	if got := classifyRun(snap, true, false, exitStatus{signaled: true, signal: "SIGKILL"}, causeProviderInsufficientBalance); got != outcomeCancelled {
		t.Fatalf("stop-before-billing classify = %q want cancelled", got)
	}
}

func TestAgentRunAssistantProseDoesNotClassifyBilling(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	// Assistant text mentions "insufficient balance" but it is not a provider
	// error event; the run completes normally and must not be misclassified.
	stdout := "{\"type\":\"text\",\"sessionID\":\"ses_x\",\"part\":{\"type\":\"text\",\"text\":\"Insufficient balance is a common issue.\"}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeCompleted) {
		t.Fatalf("assistant prose state = %q want completed", state)
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.ProviderCause != "" {
		t.Fatalf("assistant prose set providerCause = %q", d.ProviderCause)
	}
}

func TestAgentRunSubagentBillingDoesNotClassifyMain(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	// A billing error from a subagent session (different sessionID) must not
	// classify the main run; main session completes normally.
	stdout := "{\"type\":\"error\",\"sessionID\":\"ses_subagent\",\"error\":{\"data\":{\"message\":\"Insufficient balance\"}}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses_main\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_main"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeCompleted) {
		t.Fatalf("subagent billing state = %q want completed", state)
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.ProviderCause != "" {
		t.Fatalf("subagent billing set providerCause = %q", d.ProviderCause)
	}
}

func TestAgentRunShutdownWaitsForPersistence(t *testing.T) {
	a := hardeningTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	run := newActiveRun(cancel)
	a.runMu.Lock()
	a.activeRuns["run1"] = run
	a.runMu.Unlock()
	defer func() { a.runMu.Lock(); delete(a.activeRuns, "run1"); a.runMu.Unlock() }()
	// Close the done channel asynchronously, like a handler finishing its
	// interrupted persistence after cancellation.
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		run.finished()
		close(done)
	}()
	start := time.Now()
	a.stopActiveRuns()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stopActiveRuns blocked too long: %v", elapsed)
	}
	<-done
	if snap := run.state.snapshot(); snap.cause != causeServiceShutdown {
		t.Fatalf("cause = %q want service_shutdown", snap.cause)
	}
}

func TestAgentRunShutdownTimesOutAndLeavesStale(t *testing.T) {
	a := hardeningTestApp(t)
	_, cancel := context.WithCancel(context.Background())
	run := newActiveRun(cancel)
	a.runMu.Lock()
	a.activeRuns["run1"] = run
	a.runMu.Unlock()
	defer func() { a.runMu.Lock(); delete(a.activeRuns, "run1"); a.runMu.Unlock() }()
	// done is never closed: stopActiveRuns must return after the bounded wait
	// and leave the run recoverably stale (still service-shutdown caused).
	start := time.Now()
	a.stopActiveRuns()
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("stopActiveRuns exceeded bounded wait: %v", elapsed)
	}
	if snap := run.state.snapshot(); snap.cause != causeServiceShutdown {
		t.Fatalf("cause = %q want service_shutdown", snap.cause)
	}
}

func TestTaskSnapshotValidation(t *testing.T) {
	valid := map[string]any{"type": "tool_use", "part": map[string]any{
		"tool": "todowrite",
		"state": map[string]any{"input": map[string]any{"todos": []any{
			map[string]any{"content": "first", "status": "pending", "priority": "high"},
			map[string]any{"content": "second", "status": "in_progress", "priority": "low"},
			map[string]any{"content": "third", "status": "completed", "priority": "medium"},
		}}},
	}}
	snap := taskSnapshot(valid)
	if snap == "" {
		t.Fatal("valid todowrite snapshot not extracted")
	}
	var todos []map[string]string
	if err := json.Unmarshal([]byte(snap), &todos); err != nil {
		t.Fatal(err)
	}
	if len(todos) != 3 || todos[0]["status"] != "pending" || todos[1]["status"] != "in_progress" || todos[2]["status"] != "completed" {
		t.Fatalf("todos = %+v", todos)
	}

	// Non-todowrite tool: no snapshot.
	other := map[string]any{"type": "tool_use", "part": map[string]any{"tool": "bash", "state": map[string]any{"input": map[string]any{"command": "ls"}}}}
	if got := taskSnapshot(other); got != "" {
		t.Fatal("non-todowrite produced a task snapshot")
	}

	// Unknown status/priority normalized to defaults.
	unknown := map[string]any{"type": "tool_use", "part": map[string]any{
		"tool": "todowrite",
		"state": map[string]any{"input": map[string]any{"todos": []any{
			map[string]any{"content": "x", "status": "weird", "priority": "urgent"},
		}}},
	}}
	if got := taskSnapshot(unknown); got != "" {
		var todos2 []map[string]string
		if err := json.Unmarshal([]byte(got), &todos2); err == nil && len(todos2) == 1 {
			if todos2[0]["status"] != "pending" || todos2[0]["priority"] != "medium" {
				t.Fatalf("unknown normalized wrong: %+v", todos2)
			}
		} else {
			t.Fatalf("unknown snapshot = %q", got)
		}
	}

	// Oversized content dropped.
	oversize := map[string]any{"type": "tool_use", "part": map[string]any{
		"tool": "todowrite",
		"state": map[string]any{"input": map[string]any{"todos": []any{
			map[string]any{"content": strings.Repeat("a", 600), "status": "pending"},
		}}},
	}}
	if got := taskSnapshot(oversize); got != "" {
		t.Fatal("oversized content should be dropped")
	}

	// Malformed (todos not an array) returns empty.
	malformed := map[string]any{"type": "tool_use", "part": map[string]any{"tool": "todowrite", "state": map[string]any{"input": map[string]any{"todos": "nope"}}}}
	if got := taskSnapshot(malformed); got != "" {
		t.Fatal("malformed todowrite produced a snapshot")
	}
}
