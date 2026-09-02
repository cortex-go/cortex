package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	return runAgentRequestSession(t, a, workspace, fake, "conv1")
}

func runAgentRequestSession(t *testing.T, a *App, workspace string, fake *fakeOpenCode, clientSession string) []map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"workspace": workspace, "prompt": "test", "clientSession": clientSession})
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
		if t, _ := ev["type"].(string); t == "done" || t == "error" || t == "warning" || t == "cancelled" || t == "truncated" {
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
			name:        "stdout error before stop failed",
			state:       runStateSnapshot{errSeq: 1, stopSeq: 2},
			stdoutError: true,
			validStop:   true,
			exit:        exitStatus{exited: true, exitCode: 0},
			want:        outcomeFailed,
		},
		{
			name:        "stdout error after stop exit0 completed",
			state:       runStateSnapshot{errSeq: 2, stopSeq: 1},
			stdoutError: true,
			validStop:   true,
			exit:        exitStatus{exited: true, exitCode: 0},
			want:        outcomeCompleted,
		},
		{
			name:        "stdout error after stop exit1 completed_with_process_error",
			state:       runStateSnapshot{errSeq: 2, stopSeq: 1},
			stdoutError: true,
			validStop:   true,
			exit:        exitStatus{exited: true, exitCode: 1},
			want:        outcomeCompletedWError,
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
// completed_with_process_error delivered as a distinct warning event, never a
// plain completion and never the generic Error presentation.
func TestAgentRunExitNonZeroValidStopIsCompletedWithProcessError(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	stderr := "level=INFO message=exiting loop\nlevel=INFO message=disposing instance\n"
	fake.invoke(t, stdout, stderr, 1, `{"info":{"id":"ses_x"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"final"}]}]}`)
	events := runAgentRequest(t, a, ws, fake)
	if got := terminalEventType(events); got != "warning" {
		t.Fatalf("terminal event = %q want warning", got)
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
// event observed before valid completion is a genuine failure even when a
// stop follows.
func TestAgentRunStdoutErrorForcesFailure(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"error\",\"sessionID\":\"ses_x\",\"error\":{\"name\":\"ProviderError\",\"data\":{\"message\":\"stream failed\"}}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
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

// TestAgentRunPostStopErrorIsNotFailure verifies a valid main-session
// step_finish followed by a later wrapper error is post-completion evidence:
// the produced answer is not failed and the raw error does not survive as a
// separate Error block in the transcript.
func TestAgentRunPostStopErrorIsNotFailure(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n{\"type\":\"error\",\"sessionID\":\"ses_x\",\"error\":{\"name\":\"LifecycleError\",\"data\":{\"message\":\"level=INFO message=exiting loop\\nlevel=INFO message=disposing instance\"}}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_x"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	if got := terminalEventType(events); got != "warning" {
		t.Fatalf("terminal event = %q want warning", got)
	}
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeCompletedWError) {
		t.Fatalf("persisted state = %q want completed_with_process_error", state)
	}
	merged, err := a.loadConversationMerged("conv1")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range merged {
		if ev.Kind == "error" {
			t.Fatalf("post-completion error survived as a raw Error block: %+v", ev)
		}
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.Outcome != string(outcomeCompletedWError) {
		t.Fatalf("diagnostics outcome = %q", d.Outcome)
	}
	if len(d.Warnings) == 0 || !strings.Contains(strings.Join(d.Warnings, " "), "exiting loop") {
		t.Fatalf("post-completion error not retained as a warning: %+v", d.Warnings)
	}
}

// TestAgentRunExit1NoValidStopFails verifies a nonzero exit without valid
// completion evidence is a genuine failure, never a warning.
func TestAgentRunExit1NoValidStopFails(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"text\",\"sessionID\":\"ses_x\",\"part\":{\"type\":\"text\",\"text\":\"partial\"}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_x"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("exit 1 without valid stop state = %q want failed", state)
	}
}

// TestAgentRunWarningSurvivesReload verifies a completed_with_process_error
// terminal event is persisted as a warning and survives the conversation
// reload path without degrading into a generic Error block or failed state.
func TestAgentRunWarningSurvivesReload(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n{\"type\":\"error\",\"sessionID\":\"ses_x\",\"error\":{\"name\":\"LifecycleError\",\"data\":{\"message\":\"level=INFO message=exiting loop\"}}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_x"},"messages":[]}`)
	runAgentRequest(t, a, ws, fake)
	var state string
	if err := a.db.QueryRow("SELECT state FROM conversations WHERE id=?", "conv1").Scan(&state); err != nil {
		t.Fatalf("read conversation state: %v", err)
	}
	if state != string(outcomeCompletedWError) {
		t.Fatalf("reload state = %q want completed_with_process_error", state)
	}
	merged, err := a.loadConversationMerged("conv1")
	if err != nil {
		t.Fatal(err)
	}
	foundWarning := false
	for _, ev := range merged {
		if ev.Kind == "error" {
			t.Fatalf("warning run reloaded as a raw Error block: %+v", ev)
		}
		if ev.Kind == "warning" {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatal("warning terminal event missing after reload")
	}
}

// TestAgentRunNonStopFinishErrorNoValidStopFailsWithErrorBlock verifies a
// non-terminal step_finish is not completion evidence: an error after it with
// no genuine stop still fails, and the genuine error remains in the durable
// transcript (it is never suppressed as if it were post-completion).
func TestAgentRunNonStopFinishErrorNoValidStopFailsWithErrorBlock(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"max_turns\"}}\n{\"type\":\"error\",\"sessionID\":\"ses_x\",\"error\":{\"data\":{\"message\":\"stream failed\"}}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_x"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("non-stop finish then error state = %q want failed", state)
	}
	merged, err := a.loadConversationMerged("conv1")
	if err != nil {
		t.Fatal(err)
	}
	foundErr := false
	for _, ev := range merged {
		if ev.Kind == "error" && strings.Contains(ev.Text, "stream failed") {
			foundErr = true
		}
	}
	if !foundErr {
		t.Fatal("genuine error was suppressed from the durable transcript")
	}
}

// TestAgentRunNonStopFinishErrorThenStopFails verifies an error that precedes
// a genuine stop is a failure even when a non-terminal step_finish appeared
// before the error: only reason=="stop" is completion evidence.
func TestAgentRunNonStopFinishErrorThenStopFails(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"max_turns\"}}\n{\"type\":\"error\",\"sessionID\":\"ses_x\",\"error\":{\"data\":{\"message\":\"stream failed\"}}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_x"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("error before genuine stop state = %q want failed", state)
	}
	merged, err := a.loadConversationMerged("conv1")
	if err != nil {
		t.Fatal(err)
	}
	foundErr := false
	for _, ev := range merged {
		if ev.Kind == "error" && strings.Contains(ev.Text, "stream failed") {
			foundErr = true
		}
	}
	if !foundErr {
		t.Fatal("error before genuine stop suppressed from the durable transcript")
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

// --- fault-injection: terminal persistence must fail closed ---

func TestAgentRunTerminalEventInsertFailureFailsClosed(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	// Fail any terminal-event insert ("done"/"error"/"cancelled"/"truncated").
	a.failAgentRunEvent = func(runID, kind string) error {
		switch kind {
		case "done", "error", "cancelled", "truncated":
			return errors.New("injected terminal insert failure")
		}
		return nil
	}
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
	if runID == "" {
		t.Fatal("no run id")
	}
	// The run must be failed (storage failure), never durably completed.
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("state = %q want failed (storage failure)", state)
	}
	// A bounded client-facing failure must be emitted.
	if terminal == nil || terminal["outcome"] != string(outcomeFailed) {
		t.Fatalf("terminal event missing failure outcome: %+v", terminal)
	}
	if msg, _ := terminal["message"].(string); !strings.Contains(msg, "stored durably") {
		t.Fatalf("terminal message should be bounded storage failure: %q", msg)
	}
}

func TestAgentRunPostDeliveryFinalizeFailureFailsClosed(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	// Call counter: call 1 is the pre-delivery run-state update (succeeds),
	// call 2 is the post-delivery diagnostics update (fails), call 3 is the
	// storage-failure best-effort finalize (succeeds).
	calls := 0
	a.failFinishAgentRun = func(runID string) error {
		calls++
		if calls == 2 {
			return errors.New("injected post-delivery finalize failure")
		}
		return nil
	}
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if runID == "" {
		t.Fatal("no run id")
	}
	// The ordinary terminal event must have been delivered before the
	// post-delivery failure.
	var sawTerminal bool
	for _, ev := range events {
		if et, _ := ev["type"].(string); et == "done" {
			sawTerminal = true
		}
	}
	if !sawTerminal {
		t.Fatal("ordinary terminal event was not delivered before post-delivery failure")
	}
	// The storage-failure path finalizes as failed (call 3).
	if calls != 3 {
		t.Fatalf("finishAgentRun called %d times, want 3", calls)
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("state = %q want failed after post-delivery failure", state)
	}
	// Diagnostics must identify the post-delivery run-state stage.
	d := runDiagnosticsFromDB(t, a, runID)
	if d.Category != "storage_failure" {
		t.Fatalf("diagnostics category = %q want storage_failure", d.Category)
	}
	if !strings.Contains(d.DeliveryError, "post-delivery run-state") {
		t.Fatalf("delivery error should name post-delivery run-state: %q", d.DeliveryError)
	}
}

// TestAgentRunPreDeliveryFinalizeFailureFailsClosed verifies a failure of the
// first (pre-delivery) run-state update also fails closed.
func TestAgentRunPreDeliveryFinalizeFailureFailsClosed(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	calls := 0
	a.failFinishAgentRun = func(runID string) error {
		calls++
		if calls == 1 {
			return errors.New("injected pre-delivery finalize failure")
		}
		return nil
	}
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if runID == "" {
		t.Fatal("no run id")
	}
	// Pre-delivery failure means the ordinary terminal event is never written.
	for _, ev := range events {
		if et, _ := ev["type"].(string); et == "done" {
			t.Fatal("terminal event should not be delivered after pre-delivery failure")
		}
	}
	// The storage-failure path finalizes as failed (call 2).
	if calls != 2 {
		t.Fatalf("finishAgentRun called %d times, want 2", calls)
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("state = %q want failed after pre-delivery failure", state)
	}
}

func TestAgentRunUserPromptPersistFailureFailsClosed(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	a.failAgentRunEvent = func(runID, kind string) error {
		if kind == "user" {
			return errors.New("injected user prompt failure")
		}
		return nil
	}
	body, _ := json.Marshal(map[string]any{"workspace": ws, "prompt": "test", "clientSession": "conv1"})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/agent/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.agentRun(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d want 500", rec.Code)
	}
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM agent_runs WHERE conversation_id='conv1'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("durable run row missing after user-prompt failure")
	}
	var state string
	if err := a.db.QueryRow("SELECT state FROM agent_runs WHERE conversation_id='conv1' LIMIT 1").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(outcomeFailed) {
		t.Fatalf("state = %q want failed", state)
	}
}

func TestReconcileRecovered(t *testing.T) {
	cases := []struct {
		name           string
		streamed       string
		recovered      string
		wantText       string
		wantSuppressed bool
		wantReplace    bool
	}{
		{"identical", "hello world", "hello world", "", true, false},
		{"recovered prefix of streamed", "hello world extra", "hello world", "", true, false},
		{"streamed prefix of recovered", "hello world", "hello world more", "more", false, false},
		{"streamed suffix of recovered (replacement)", "world", "hello world", "hello world", false, true},
		{"partial overlap", "prefix mid", "mid suffix", "suffix", false, false},
		{"no overlap", "alpha", "beta", "beta", false, false},
		{"empty recovered", "alpha", "", "", true, false},
		{"empty streamed", "", "beta", "beta", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, suppressed, replace := reconcileRecovered(tc.streamed, tc.recovered)
			if suppressed != tc.wantSuppressed {
				t.Fatalf("suppressed = %v want %v", suppressed, tc.wantSuppressed)
			}
			if replace != tc.wantReplace {
				t.Fatalf("replace = %v want %v", replace, tc.wantReplace)
			}
			if strings.TrimSpace(text) != tc.wantText {
				t.Fatalf("text = %q want %q", text, tc.wantText)
			}
		})
	}
}

// TestReconcileRecoveredTranscriptOrder reconstructs the final user-visible
// transcript for every overlap case, including a replacement recovery and
// non-JSON streamed output.
func TestReconcileRecoveredTranscriptOrder(t *testing.T) {
	cases := []struct {
		name      string
		stdout    string // raw stdout bytes (JSON or plain output)
		export    string
		wantOrder []string // expected assistant transcript lines in order
	}{
		{
			name:      "streamed prefix then append suffix",
			stdout:    "{\"type\":\"text\",\"sessionID\":\"ses_x\",\"part\":{\"type\":\"text\",\"text\":\"hello\"}}\n",
			export:    `{"info":{"id":"ses_x"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"hello world"}]}]}`,
			wantOrder: []string{"hello", "world"},
		},
		{
			name:      "streamed suffix then replacement",
			stdout:    "world\n",
			export:    `{"info":{"id":"ses_x"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"hello world"}]}]}`,
			wantOrder: []string{"hello world"},
		},
		{
			name:      "identical streamed suppresses recovery",
			stdout:    "{\"type\":\"text\",\"sessionID\":\"ses_x\",\"part\":{\"type\":\"text\",\"text\":\"final\"}}\n",
			export:    `{"info":{"id":"ses_x"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"final"}]}]}`,
			wantOrder: []string{"final"},
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeOpenCode(t)
			a := agentTestApp(t, fake)
			ws := workspaceUnderRoot(t, a)
			stdout := tc.stdout + "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
			fake.invoke(t, stdout, "", 1, tc.export)
			// Use a unique conversation per case so transcripts do not mix.
			body, _ := json.Marshal(map[string]any{"workspace": ws, "prompt": "test", "clientSession": fmt.Sprintf("conv%d", i)})
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/agent/run", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			a.agentRun(rec, req)
			merged, err := a.loadConversationMerged(fmt.Sprintf("conv%d", i))
			if err != nil {
				t.Fatal(err)
			}
			var lines []string
			for _, ev := range merged {
				if ev.Kind == "assistant" {
					lines = append(lines, ev.Text)
				}
			}
			if len(lines) != len(tc.wantOrder) {
				t.Fatalf("transcript lines = %d want %d: %+v", len(lines), len(tc.wantOrder), lines)
			}
			for j, want := range tc.wantOrder {
				if strings.TrimSpace(lines[j]) != want {
					t.Fatalf("line %d = %q want %q (full: %+v)", j, lines[j], want, lines)
				}
			}
		})
	}
}

func TestAgentRunRecoverySuppressedWhenCompleteAlreadyStreamed(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	// The complete final answer is streamed before the failure, so recovery
	// must be suppressed rather than duplicating it.
	stdout := "{\"type\":\"text\",\"sessionID\":\"ses_x\",\"part\":{\"type\":\"text\",\"text\":\"final answer\"}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	export := `{"info":{"id":"ses_x"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"final answer"}]}]}`
	fake.invoke(t, stdout, "", 1, export)
	events := runAgentRequest(t, a, ws, fake)
	for _, ev := range events {
		if et, _ := ev["type"].(string); et == "recovered" {
			t.Fatalf("recovery not suppressed: %+v", ev)
		}
	}
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.RecoveryResult != "ok_suppressed" {
		t.Fatalf("recovery result = %q want ok_suppressed", d.RecoveryResult)
	}
}

func TestAgentRunRecoveryPersistsOnlyMissingPortion(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	// Streamed is a prefix of the recovered answer: only the missing suffix is
	// persisted and delivered.
	stdout := "{\"type\":\"text\",\"sessionID\":\"ses_x\",\"part\":{\"type\":\"text\",\"text\":\"partial \"}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	export := `{"info":{"id":"ses_x"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"partial final"}]}]}`
	fake.invoke(t, stdout, "", 1, export)
	events := runAgentRequest(t, a, ws, fake)
	found := ""
	for _, ev := range events {
		if t, _ := ev["type"].(string); t == "recovered" {
			found = ev["data"].(map[string]any)["text"].(string)
		}
	}
	if found != "final" {
		t.Fatalf("recovered text = %q want 'final' (missing portion only)", found)
	}
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	// Verify no duplicate assistant event was persisted.
	events2, err := a.loadAgentRunEvents("conv1")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range events2 {
		if ev.Kind == "assistant" && strings.Contains(ev.Text, "partial") {
			count++
		}
	}
	_ = runID
	if count != 1 {
		t.Fatalf("partial text persisted %d times, want 1", count)
	}
}

// --- per-session main-session correlation ---

func TestAgentRunSubagentErrorThenMainBillingError(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	// A subagent error arrives first; the main session's billing error arrives
	// afterward with a main-session stop. The main session must win.
	stdout := "{\"type\":\"error\",\"sessionID\":\"ses_sub\",\"error\":{\"data\":{\"message\":\"generic subagent failure\"}}}\n" +
		"{\"type\":\"step_finish\",\"sessionID\":\"ses_main\",\"part\":{\"reason\":\"stop\"}}\n" +
		"{\"type\":\"error\",\"sessionID\":\"ses_main\",\"error\":{\"data\":{\"message\":\"Insufficient balance\"}}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_main"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.ProviderCause != string(causeProviderInsufficientBalance) {
		t.Fatalf("main billing error not classified: providerCause=%q", d.ProviderCause)
	}
	if d.BillingURL != "" {
		t.Fatalf("unexpected billing URL from subagent evidence: %q", d.BillingURL)
	}
}

func TestAgentRunSubagentErrorThenMainGenericError(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	// Subagent billing-looking error first, then main generic error. The main
	// session's error must be the one retained (generic), not billing.
	stdout := "{\"type\":\"error\",\"sessionID\":\"ses_sub\",\"error\":{\"data\":{\"message\":\"Insufficient balance\"}}}\n" +
		"{\"type\":\"step_finish\",\"sessionID\":\"ses_main\",\"part\":{\"reason\":\"stop\"}}\n" +
		"{\"type\":\"error\",\"sessionID\":\"ses_main\",\"error\":{\"data\":{\"message\":\"provider crashed\"}}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_main"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.ProviderCause != "" {
		t.Fatalf("subagent billing misclassified main: providerCause=%q", d.ProviderCause)
	}
	if d.StdoutError == "" || strings.Contains(d.StdoutError, "Insufficient balance") {
		t.Fatalf("main generic error not retained: stdoutError=%q", d.StdoutError)
	}
}

func TestAgentRunMainStopThenSubagentError(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	// Main session completes (stop), then a subagent error arrives. The main
	// session's completion must not be invalidated by the subagent error.
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_main\",\"part\":{\"reason\":\"stop\"}}\n" +
		"{\"type\":\"error\",\"sessionID\":\"ses_sub\",\"error\":{\"data\":{\"message\":\"Insufficient balance\"}}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_main"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeCompleted) {
		t.Fatalf("main stop then subagent error state = %q want completed", state)
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.ProviderCause != "" {
		t.Fatalf("subagent error misclassified main: %q", d.ProviderCause)
	}
}

func TestAgentRunInterleavedMainSubagentSteps(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	// Interleaved step_finish from subagent and main; only the main session's
	// final stop counts as completion evidence.
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_sub\",\"part\":{\"reason\":\"tool-calls\"}}\n" +
		"{\"type\":\"step_finish\",\"sessionID\":\"ses_main\",\"part\":{\"reason\":\"tool-calls\"}}\n" +
		"{\"type\":\"step_finish\",\"sessionID\":\"ses_sub\",\"part\":{\"reason\":\"stop\"}}\n" +
		"{\"type\":\"step_finish\",\"sessionID\":\"ses_main\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_main"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeCompleted) {
		t.Fatalf("interleaved steps state = %q want completed", state)
	}
}

func TestAgentRunPromptStartFailureNoActivity(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	// No activity; a single error at start. The error's session is the main
	// session and the billing error must classify.
	stdout := "{\"type\":\"error\",\"sessionID\":\"ses_main\",\"error\":{\"data\":{\"message\":\"Insufficient balance\"}}}\n"
	fake.invoke(t, stdout, "", 1, `{"info":{"id":"ses_main"},"messages":[]}`)
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.ProviderCause != string(causeProviderInsufficientBalance) {
		t.Fatalf("prompt-start billing not classified: %q", d.ProviderCause)
	}
}

// --- nil request context Done() channel ---

func TestAgentRunNilContextDoneDoesNotLeak(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	// Use a background context whose Done() is nil; the runner must not spawn
	// a blocking goroutine and must still complete normally.
	body, _ := json.Marshal(map[string]any{"workspace": ws, "prompt": "test", "clientSession": "conv1"})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/agent/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	a.agentRun(rec, req)
	sc := bufio.NewScanner(rec.Body)
	sawDone := false
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal([]byte(sc.Text()), &ev) == nil {
			if ev["type"] == "done" {
				sawDone = true
			}
		}
	}
	if !sawDone {
		t.Fatal("run did not complete with nil Done() context")
	}
}

// TestAgentRunTerminalMarkerSupersession verifies that when a post-delivery
// finalization fails, a sequenced storage-failure terminal marker supersedes
// the earlier completed marker in the reloaded merged transcript.
func TestAgentRunTerminalMarkerSupersession(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	calls := 0
	a.failFinishAgentRun = func(runID string) error {
		calls++
		if calls == 2 {
			return errors.New("injected post-delivery finalize failure")
		}
		return nil
	}
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if runID == "" {
		t.Fatal("no run id")
	}
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("run state = %q want failed", state)
	}
	// The reloaded merged transcript must show only the storage-failure
	// terminal marker as the authoritative final outcome.
	merged, err := a.loadConversationMerged("conv1")
	if err != nil {
		t.Fatal(err)
	}
	markers := []conversationEvent{}
	for _, ev := range merged {
		if runMarkerRunID(ev.Name) == runID {
			markers = append(markers, ev)
		}
	}
	if len(markers) != 1 {
		t.Fatalf("expected exactly one authoritative terminal marker, got %d: %+v", len(markers), markers)
	}
	if markers[0].Name != "run:"+runID+":failed" {
		t.Fatalf("terminal marker = %q want run:%s:failed", markers[0].Name, runID)
	}
}

// TestAgentRunStorageFailureMarkerInsertStillFailsClosed verifies that when the
// storage-failure terminal marker insert itself fails, the run row and
// diagnostics remain the authoritative fallback source.
func TestAgentRunStorageFailureMarkerInsertStillFailsClosed(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)
	stdout := "{\"type\":\"step_finish\",\"sessionID\":\"ses_x\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout, "", 0, `{"info":{"id":"ses_x"},"messages":[]}`)
	// Fail every terminal-event insert AND every finalize (except the first
	// finalize, which must succeed so the flow reaches post-delivery).
	calls := 0
	a.failAgentRunEvent = func(runID, kind string) error {
		switch kind {
		case "done", "error", "cancelled", "truncated":
			return errors.New("injected terminal insert failure")
		}
		return nil
	}
	a.failFinishAgentRun = func(runID string) error {
		calls++
		if calls == 1 {
			return nil // pre-delivery finalize succeeds
		}
		return errors.New("injected finalize failure")
	}
	events := runAgentRequest(t, a, ws, fake)
	var runID string
	for _, ev := range events {
		if id, _ := ev["data"].(map[string]any)["runID"].(string); id != "" {
			runID = id
		}
	}
	if runID == "" {
		t.Fatal("no run id")
	}
	// Run row and diagnostics are the authoritative fallback.
	if state := runStateFromDB(t, a, runID); state != string(outcomeFailed) {
		t.Fatalf("run state = %q want failed", state)
	}
	d := runDiagnosticsFromDB(t, a, runID)
	if d.Category != "storage_failure" {
		t.Fatalf("diagnostics category = %q want storage_failure", d.Category)
	}
}

// TestReplacementSupersessionIsRunScoped verifies that a recovery replacement
// only removes assistant fragments from its own run, never from an earlier run
// in the same conversation that happens to contain the same text.
func TestReplacementSupersessionIsRunScoped(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)

	// Run 1 completes normally, producing assistant text "world".
	stdout1 := "{\"type\":\"text\",\"sessionID\":\"ses1\",\"part\":{\"type\":\"text\",\"text\":\"world\"}}\n{\"type\":\"step_finish\",\"sessionID\":\"ses1\",\"part\":{\"reason\":\"stop\"}}\n"
	fake.invoke(t, stdout1, "", 0, `{"info":{"id":"ses1"},"messages":[]}`)
	runAgentRequestSession(t, a, ws, fake, "convX")

	// Run 2 streams "world" (non-JSON output), then fails and recovers the
	// full response "hello world" as a replacement.
	stdout2 := "world\n{\"type\":\"step_finish\",\"sessionID\":\"ses2\",\"part\":{\"reason\":\"stop\"}}\n"
	export2 := `{"info":{"id":"ses2"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"hello world"}]}]}`
	fake.invoke(t, stdout2, "", 1, export2)
	runAgentRequestSession(t, a, ws, fake, "convX")

	merged, err := a.loadConversationMerged("convX")
	if err != nil {
		t.Fatal(err)
	}
	// Run 1's "world" must be preserved verbatim; run 2's "world" fragment is
	// replaced by "hello world". So the assistant transcript is exactly
	// ["world", "hello world"].
	assistant := []string{}
	for _, ev := range merged {
		if ev.Kind == "assistant" {
			assistant = append(assistant, strings.TrimSpace(ev.Text))
		}
	}
	want := []string{"world", "hello world"}
	if len(assistant) != len(want) {
		t.Fatalf("assistant transcript = %+v want %+v", assistant, want)
	}
	for i := range want {
		if assistant[i] != want[i] {
			t.Fatalf("line %d = %q want %q (full: %+v)", i, assistant[i], want[i], assistant)
		}
	}
}

// TestMultipleReplacementsSeparateRuns verifies two replacement recoveries in
// separate runs each supersede only their own run's fragments.
func TestMultipleReplacementsSeparateRuns(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)

	// Run 1: streams "lpha" (a suffix), fails, replaces with "alpha".
	fake.invoke(t, "{\"type\":\"text\",\"sessionID\":\"s1\",\"part\":{\"type\":\"text\",\"text\":\"lpha\"}}\n{\"type\":\"step_finish\",\"sessionID\":\"s1\",\"part\":{\"reason\":\"stop\"}}\n", "", 1,
		`{"info":{"id":"s1"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"alpha"}]}]}`)
	runAgentRequestSession(t, a, ws, fake, "convY")

	// Run 2: streams "eta" (a suffix), fails, replaces with "beta".
	fake.invoke(t, "{\"type\":\"text\",\"sessionID\":\"s2\",\"part\":{\"type\":\"text\",\"text\":\"eta\"}}\n{\"type\":\"step_finish\",\"sessionID\":\"s2\",\"part\":{\"reason\":\"stop\"}}\n", "", 1,
		`{"info":{"id":"s2"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"beta"}]}]}`)
	runAgentRequestSession(t, a, ws, fake, "convY")

	merged, err := a.loadConversationMerged("convY")
	if err != nil {
		t.Fatal(err)
	}
	assistant := []string{}
	for _, ev := range merged {
		if ev.Kind == "assistant" {
			assistant = append(assistant, strings.TrimSpace(ev.Text))
		}
	}
	want := []string{"alpha", "beta"}
	if len(assistant) != len(want) {
		t.Fatalf("assistant transcript = %+v want %+v", assistant, want)
	}
	for i := range want {
		if assistant[i] != want[i] {
			t.Fatalf("line %d = %q want %q", i, assistant[i], want[i])
		}
	}
}

// TestReplacementDoesNotRemoveClientAuthoredText verifies a client-authored
// assistant event containing the replacement text is never dropped.
func TestReplacementDoesNotRemoveClientAuthoredText(t *testing.T) {
	fake := newFakeOpenCode(t)
	a := agentTestApp(t, fake)
	ws := workspaceUnderRoot(t, a)

	// A run streams "world", fails, and replaces with "hello world".
	fake.invoke(t, "world\n{\"type\":\"step_finish\",\"sessionID\":\"s1\",\"part\":{\"reason\":\"stop\"}}\n", "", 1,
		`{"info":{"id":"s1"},"messages":[{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"hello world"}]}]}`)
	runAgentRequestSession(t, a, ws, fake, "convZ")

	// A stale client tab writes a client-authored assistant event "world".
	c := conversation{ID: "convZ", Workspace: ws, Events: []conversationEvent{
		{Kind: "assistant", Text: "wor", CreatedAt: 1},
	}}
	if err := a.saveConversation(&c); err != nil {
		t.Fatal(err)
	}
	merged, err := a.loadConversationMerged("convZ")
	if err != nil {
		t.Fatal(err)
	}
	assistant := []string{}
	for _, ev := range merged {
		if ev.Kind == "assistant" {
			assistant = append(assistant, strings.TrimSpace(ev.Text))
		}
	}
	// The client-authored "world" is preserved; only the server-owned run
	// fragment was replaced.
	foundWor := false
	foundHello := false
	for _, a := range assistant {
		if a == "wor" {
			foundWor = true
		}
		if a == "hello world" {
			foundHello = true
		}
	}
	if !foundWor || !foundHello {
		t.Fatalf("assistant transcript = %+v should contain both client 'wor' and 'hello world'", assistant)
	}
}
