package app

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestConversationImportIsAtomic(t *testing.T) {
	a := hardeningTestApp(t)
	payload := map[string]any{"conversations": []any{
		map[string]any{"id": "valid-one", "workspace": a.root, "events": []any{}},
		map[string]any{"id": "invalid id", "workspace": a.root, "events": []any{}},
	}}
	b, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", "/api/conversations", bytes.NewReader(b))
	w := httptest.NewRecorder()
	a.conversationsAPI(w, r)
	if w.Code == 200 {
		t.Fatal("invalid batch accepted")
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM conversations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial import count=%d err=%v", count, err)
	}
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
