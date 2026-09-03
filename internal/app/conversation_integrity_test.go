package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestConversationsEndpointIsGetOnly verifies the legacy batch-import POST path
// was removed: /api/conversations accepts only GET, and the single-conversation
// PUT /api/conversation remains the sole write path. No migration/import
// endpoint exists.
func TestConversationsEndpointIsGetOnly(t *testing.T) {
	a := hardeningTestApp(t)
	// The handler itself rejects the batch-import POST method.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/conversations", nil)
	a.conversationsAPI(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/conversations must be rejected by the handler, got %d", rec.Code)
	}
	// Through the routed, session-protected server an unauthenticated POST is
	// refused at the auth gate (never a 200 import response), and GET is also
	// gated behind authentication.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/conversations", nil)
	a.httpServer().Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("unauthenticated POST /api/conversations reached an import path")
	}
	// The handler itself accepts GET and returns the conversation list.
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/conversations", nil)
	a.conversationsAPI(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/conversations must be allowed, got %d", rec.Code)
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
