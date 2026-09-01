package app

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestConversationImportIsPerRecord verifies the import handles each record
// independently: a structurally invalid record is reported without rolling
// back or discarding the valid records around it.
func TestConversationImportIsPerRecord(t *testing.T) {
	a := hardeningTestApp(t)
	payload := map[string]any{"conversations": []any{
		map[string]any{"id": "valid-one", "workspace": a.root, "events": []any{}},
		map[string]any{"id": "invalid id", "workspace": a.root, "events": []any{}},
		map[string]any{"id": "valid-two", "workspace": a.root, "events": []any{map[string]any{"kind": "user", "text": "keep me"}}},
	}}
	b, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", "/api/conversations", bytes.NewReader(b))
	w := httptest.NewRecorder()
	a.conversationsAPI(w, r)
	if w.Code != 200 {
		t.Fatalf("import status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Imported []string            `json:"imported"`
		Rejected []map[string]string `json:"rejected"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Imported) != 2 || len(resp.Rejected) != 1 {
		t.Fatalf("imported=%v rejected=%v", resp.Imported, resp.Rejected)
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM conversations").Scan(&count); err != nil || count != 2 {
		t.Fatalf("valid records not retained count=%d err=%v", count, err)
	}
	var events int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM conversation_events WHERE conversation_id='valid-two'").Scan(&events); err != nil || events != 1 {
		t.Fatalf("valid transcript discarded events=%d err=%v", events, err)
	}
}

// TestConversationImportRetryIsIdempotent verifies re-running the same import
// does not duplicate records or events.
func TestConversationImportRetryIsIdempotent(t *testing.T) {
	a := hardeningTestApp(t)
	payload := map[string]any{"conversations": []any{
		map[string]any{"id": "retry-one", "workspace": a.root, "events": []any{map[string]any{"kind": "user", "text": "once"}}},
	}}
	b, _ := json.Marshal(payload)
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/api/conversations", bytes.NewReader(b))
		w := httptest.NewRecorder()
		a.conversationsAPI(w, r)
		if w.Code != 200 {
			t.Fatalf("retry %d failed: %s", i, w.Body.String())
		}
	}
	var count, events int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM conversations").Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry duplicated conversations count=%d err=%v", count, err)
	}
	if err := a.db.QueryRow("SELECT COUNT(*) FROM conversation_events WHERE conversation_id='retry-one'").Scan(&events); err != nil || events != 1 {
		t.Fatalf("retry duplicated events events=%d err=%v", events, err)
	}
}

func TestConversationUnavailableWorkspaceImports(t *testing.T) {
	a := hardeningTestApp(t)
	missing := a.root + "/gone-repo"
	payload := map[string]any{"conversations": []any{
		map[string]any{"id": "missing-ws", "workspace": missing, "events": []any{map[string]any{"kind": "user", "text": "transcript survives"}}},
		map[string]any{"id": "valid-ws", "workspace": a.root, "events": []any{}},
	}}
	b, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", "/api/conversations", bytes.NewReader(b))
	w := httptest.NewRecorder()
	a.conversationsAPI(w, r)
	if w.Code != 200 {
		t.Fatalf("import status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Imported []string            `json:"imported"`
		Rejected []map[string]string `json:"rejected"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Imported) != 2 || len(resp.Rejected) != 0 {
		t.Fatalf("unavailable-workspace record must import: imported=%v rejected=%v", resp.Imported, resp.Rejected)
	}
	items, err := a.loadConversations("")
	if err != nil {
		t.Fatal(err)
	}
	var missingStatus, validStatus, transcript string
	for _, it := range items {
		switch it.ID {
		case "missing-ws":
			missingStatus = it.WorkspaceStatus
			if len(it.Events) != 1 || it.Events[0].Text != "transcript survives" {
				t.Fatal("missing-workspace transcript was not persisted")
			}
		case "valid-ws":
			validStatus = it.WorkspaceStatus
		}
	}
	if missingStatus != wsMissing {
		t.Fatalf("missing workspace status=%q want %q", missingStatus, wsMissing)
	}
	if validStatus != wsAvailable {
		t.Fatalf("valid workspace status=%q want %q", validStatus, wsAvailable)
	}
	_ = transcript
}

func TestRestartResolvesInterruptedRun(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	a, err := New(Options{Root: root, DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.startAgentRun("run1", "conversation1", "work", root, "openai", "gpt"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	a, err = New(Options{Root: root, DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var state string
	if err := a.db.QueryRow("SELECT state FROM agent_runs WHERE id='run1'").Scan(&state); err != nil || state != "interrupted" {
		t.Fatalf("state=%q err=%v", state, err)
	}
}
