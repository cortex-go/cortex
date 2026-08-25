package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfinesRoot(t *testing.T) {
	root := t.TempDir()
	a, err := New(Options{Root: root, DataDir: filepath.Join(root, "data"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.resolve("../escape"); err == nil {
		t.Fatal("expected escape rejection")
	}
}
func TestSettingsPersistWithoutPublicKey(t *testing.T) {
	root := t.TempDir()
	a, err := New(Options{Root: root, DataDir: filepath.Join(root, "data"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	a.settings.Keys["opencode"] = "secret"
	if err = a.saveSettings(); err != nil {
		t.Fatal(err)
	}
	p := a.publicSettings()
	for _, x := range p.Providers {
		if _, ok := x["key"]; ok {
			t.Fatal("public settings leaked key")
		}
	}
	b, _ := os.ReadFile(a.settingsPath())
	if len(b) == 0 {
		t.Fatal("settings not written")
	}
}
func TestAssistantTexts(t *testing.T) {
	v := map[string]any{"messages": []any{map[string]any{"info": map[string]any{"role": "assistant"}, "parts": []any{map[string]any{"type": "text", "text": "hello"}}}}}
	got := assistantTexts(v)
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("%#v", got)
	}
}
