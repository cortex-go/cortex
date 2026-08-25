package app

import (
	"bytes"
	"encoding/json"
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

func TestCollectUsageFromOpenCodeSessionTokens(t *testing.T) {
	v := map[string]any{
		"type":   "step-finish",
		"tokens": map[string]any{"input": float64(1234), "output": float64(321), "reasoning": float64(42)},
		"cost":   float64(0.0123),
	}
	var input, output uint64
	var cost float64
	collectUsage(v, &input, &output, &cost)
	if input != 1234 || output != 321 {
		t.Fatalf("usage = %d/%d", input, output)
	}
	if cost != 0.0123 {
		t.Fatalf("cost = %v", cost)
	}
}

func TestProviderRuntimeConfigUsesSelectedModel(t *testing.T) {
	p, ok := providerByID("openai")
	if !ok {
		t.Fatal("openai provider missing")
	}
	b, err := cortexOpenCodeConfig(p, "gpt-5.2")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["model"] != "openai/gpt-5.2" {
		t.Fatalf("model = %#v", cfg["model"])
	}
	providers, _ := cfg["provider"].(map[string]any)
	openai, _ := providers["openai"].(map[string]any)
	options, _ := openai["options"].(map[string]any)
	if options["apiKey"] != "{env:CORTEX_PROVIDER_API_KEY}" {
		t.Fatalf("api key config = %#v", options["apiKey"])
	}
}

func TestSubscriptionProviderDoesNotEmbedAPIKeyConfig(t *testing.T) {
	p, ok := providerByID("github-copilot")
	if !ok {
		t.Fatal("github copilot provider missing")
	}
	b, err := cortexOpenCodeConfig(p, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("CORTEX_PROVIDER_API_KEY")) {
		t.Fatal("OAuth-backed provider unexpectedly embeds API-key config")
	}
}
