package app

import (
	"path/filepath"
	"testing"
)

func TestDurableConversationRoundTrip(t *testing.T) {
	root := t.TempDir()
	a, err := New(Options{Root: root, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := conversation{ID: "conversation-1", Title: "SQLite work", Workspace: root, Provider: "openai", Model: "gpt", Events: []conversationEvent{{Kind: "user", Text: "Build it"}, {Kind: "assistant", Text: "Done"}}}
	if err := a.saveConversation(&c); err != nil {
		t.Fatal(err)
	}
	items, err := a.loadConversations("SQLite")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Events) != 2 || items[0].Events[1].Text != "Done" {
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

func TestConversationRejectsEscapingWorkspace(t *testing.T) {
	a, err := New(Options{Root: t.TempDir(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := conversation{ID: "bad", Workspace: filepath.Dir(a.root)}
	if err := a.saveConversation(&c); err == nil {
		t.Fatal("escaping workspace accepted")
	}
}
