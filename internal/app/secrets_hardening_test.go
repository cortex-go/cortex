package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsAreOwnerOnlyAndCorruptionFailsClosed(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	a, err := New(Options{Root: root, DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	a.settings.Keys["openai"] = "sk-test-secret"
	if err := a.saveSettings(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(a.settingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("settings mode %o", st.Mode().Perm())
	}
	a.Close()
	if err := os.WriteFile(filepath.Join(data, "settings.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Root: root, DataDir: data}); err == nil {
		t.Fatal("corrupt settings silently accepted")
	}
}

func TestAllConfiguredSecretsAreRedacted(t *testing.T) {
	a := hardeningTestApp(t)
	a.settings.Keys["openai"] = "first-secret"
	a.settings.Keys["anthropic"] = "second-secret"
	a.settings.Auth.GoogleClientSecret = "oauth-secret"
	got := a.redactSecrets("first-secret second-secret oauth-secret")
	if strings.Contains(got, "secret") {
		t.Fatalf("secret leaked: %q", got)
	}
}

func TestProviderValuesRejectControls(t *testing.T) {
	for _, value := range []string{"", "model\nINJECT", "model\\path", "model\x7f"} {
		if validProviderValue(value) {
			t.Fatalf("accepted %q", value)
		}
	}
	if !validProviderValue("anthropic/claude-sonnet-4") {
		t.Fatal("valid model rejected")
	}
}
