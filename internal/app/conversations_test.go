package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultRootWorkspaceResolution verifies a workspace-less selection
// resolves to the configured Cortex root and remains confined there: an empty
// value is the explicit "use default root" state and can never be abused to
// escape the root boundary.
func TestDefaultRootWorkspaceResolution(t *testing.T) {
	root := t.TempDir()
	a, err := New(Options{Root: root, DataDir: filepath.Join(root, "data"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if got, err := a.resolve(""); err != nil || got != a.root {
		t.Fatalf("resolve empty = %q err=%v want root %q", got, err, a.root)
	}
	if got, err := a.resolve("/"); err != nil || got != a.root {
		t.Fatalf("resolve / = %q err=%v want root %q", got, err, a.root)
	}
	if _, err := a.resolve("../escape"); err == nil {
		t.Fatal("empty/default workspace must never permit escape from the configured root")
	}
}

// TestStartAgentRunStoresDefaultRootWorkspaceVerbatim verifies the conversation
// record keeps the explicit empty default-root state for a workspace-less run
// and the resolved absolute path for an explicit workspace.
func TestStartAgentRunStoresDefaultRootWorkspaceVerbatim(t *testing.T) {
	root := t.TempDir()
	a, err := New(Options{Root: root, DataDir: filepath.Join(root, "data"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.startAgentRun("run-empty", "conv-empty", "p", "", "opencode", "deepseek-v4-flash"); err != nil {
		t.Fatal(err)
	}
	var ws string
	if err := a.db.QueryRow("SELECT workspace FROM conversations WHERE id='conv-empty'").Scan(&ws); err != nil || ws != "" {
		t.Fatalf("default-root workspace stored %q err=%v; want empty", ws, err)
	}
	resolved := filepath.Join(root, "repo")
	if err := os.MkdirAll(resolved, 0755); err != nil {
		t.Fatal(err)
	}
	if err := a.startAgentRun("run-explicit", "conv-explicit", "p", resolved, "opencode", "deepseek-v4-flash"); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow("SELECT workspace FROM conversations WHERE id='conv-explicit'").Scan(&ws); err != nil || ws != resolved {
		t.Fatalf("explicit workspace stored %q err=%v; want %q", ws, err, resolved)
	}
}

func TestDurableConversationRoundTrip(t *testing.T) {
	root := t.TempDir()
	a, err := New(Options{Root: root, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := conversation{ID: "conversation-1", Title: "SQLite work", Workspace: root, Provider: "openai", Model: "gpt", Events: []conversationEvent{{Kind: "user", Text: "Build it"}, {Kind: "image", Text: "data:image/png;base64,AAAA", Name: "shot.png"}, {Kind: "assistant", Text: "Done"}}}
	if err := a.saveConversation(&c); err != nil {
		t.Fatal(err)
	}
	items, err := a.loadConversations("SQLite")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Events) != 3 || items[0].Events[1].Text != "data:image/png;base64,AAAA" || items[0].Events[1].Name != "shot.png" {
		t.Fatalf("unexpected conversation: %#v", items)
	}
	c.ArchivedAt = 123
	if err := a.saveConversation(&c); err != nil {
		t.Fatal(err)
	}
	items, _ = a.loadConversations("")
	if items[0].ArchivedAt != 123 {
		t.Fatalf("archive time = %d", items[0].ArchivedAt)
	}
}

// TestConversationStoresEscapingWorkspaceVerbatim verifies conversation
// metadata persists even when the historical workspace is outside the current
// root, and that the availability category is reported for the UI.
func TestConversationStoresEscapingWorkspaceVerbatim(t *testing.T) {
	a, err := New(Options{Root: t.TempDir(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	esc := filepath.Dir(a.root)
	c := conversation{ID: "esc-ws", Workspace: esc, Events: []conversationEvent{{Kind: "user", Text: "keep"}}}
	if err := a.saveConversation(&c); err != nil {
		t.Fatalf("escaping workspace should persist: %v", err)
	}
	if c.Workspace != esc {
		t.Fatalf("workspace was rewritten to %q; must be stored verbatim", c.Workspace)
	}
	items, err := a.loadConversations("")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("loaded %d conversations", len(items))
	}
	if items[0].Workspace != esc || items[0].WorkspaceStatus != wsOutsideRoot {
		t.Fatalf("got workspace=%q status=%q want %q/%q", items[0].Workspace, items[0].WorkspaceStatus, esc, wsOutsideRoot)
	}
	if len(items[0].Events) != 1 || items[0].Events[0].Text != "keep" {
		t.Fatal("escaping-workspace transcript was not persisted")
	}
}

// TestConversationMissingWorkspacePersists verifies a missing and an
// inaccessible workspace both persist transcripts and report their category.
func TestConversationMissingWorkspacePersists(t *testing.T) {
	a, err := New(Options{Root: t.TempDir(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	missing := filepath.Join(a.root, "renamed-away")
	c := conversation{ID: "missing-ws", Workspace: missing, Events: []conversationEvent{{Kind: "user", Text: "survives"}}}
	if err := a.saveConversation(&c); err != nil {
		t.Fatalf("missing workspace should persist: %v", err)
	}
	items, _ := a.loadConversations("")
	if items[0].WorkspaceStatus != wsMissing {
		t.Fatalf("status=%q want %q", items[0].WorkspaceStatus, wsMissing)
	}

	// A path whose parent component is a regular file reports inaccessible
	// (ENOTDIR) deterministically, regardless of the test user's permissions.
	notDir := filepath.Join(a.root, "afile")
	if err := os.WriteFile(notDir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(notDir, "sub")
	c2 := conversation{ID: "inaccessible-ws", Workspace: blocked, Events: []conversationEvent{{Kind: "user", Text: "also survives"}}}
	if err := a.saveConversation(&c2); err != nil {
		t.Fatalf("inaccessible workspace should persist: %v", err)
	}
	items, _ = a.loadConversations("")
	for _, it := range items {
		if it.ID == "inaccessible-ws" {
			if it.WorkspaceStatus != wsInaccessible {
				t.Fatalf("status=%q want %q", it.WorkspaceStatus, wsInaccessible)
			}
			if len(it.Events) != 1 {
				t.Fatal("inaccessible-workspace transcript not persisted")
			}
		}
	}
}

// TestSymlinkEscapeCannotExecute verifies a stored symlink-escape workspace is
// reported as such and is rejected at the execution boundary.
func TestSymlinkEscapeCannotExecute(t *testing.T) {
	a, err := New(Options{Root: t.TempDir(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(a.root, "alias")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	c := conversation{ID: "link-ws", Workspace: link}
	if err := a.saveConversation(&c); err != nil {
		t.Fatal(err)
	}
	items, _ := a.loadConversations("")
	if items[0].WorkspaceStatus != wsSymlinkEsc {
		t.Fatalf("status=%q want %q", items[0].WorkspaceStatus, wsSymlinkEsc)
	}
	if _, err := a.resolve(link); err == nil {
		t.Fatal("symlink-escape workspace passed strict resolve")
	}
}

// TestOutOfRootWorkspaceCannotExecute verifies a stored outside-root workspace
// is rejected at the execution boundary even though it persists as metadata.
func TestOutOfRootWorkspaceCannotExecute(t *testing.T) {
	a, err := New(Options{Root: t.TempDir(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	esc := filepath.Dir(a.root)
	c := conversation{ID: "esc-ws", Workspace: esc}
	if err := a.saveConversation(&c); err != nil {
		t.Fatal(err)
	}
	if _, err := a.resolve(esc); err == nil {
		t.Fatal("outside-root workspace passed strict resolve")
	}
}
