package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzRecordID(f *testing.F) {
	for _, s := range []string{"valid-id", "../escape", "", "a b", "日本語"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { _ = validRecordID(s) })
}

func FuzzProviderValue(f *testing.F) {
	for _, s := range []string{"openai/gpt-5", "model\nheader", "model\\path", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { _ = validProviderValue(s) })
}

func FuzzWorkspaceResolveNeverEscapes(f *testing.F) {
	f.Add(".")
	f.Add("../outside")
	f.Add("/tmp")
	f.Fuzz(func(t *testing.T, value string) {
		a := hardeningTestApp(t)
		if p, err := a.resolve(value); err == nil && !pathWithin(a.root, p) {
			t.Fatalf("resolved outside root: %q", p)
		}
	})
}

func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
