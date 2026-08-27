package app

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceBoundaryRejectsSymlinkEscape(t *testing.T) {
	a := hardeningTestApp(t)
	root := a.root
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := a.resolve("escape/secret"); err == nil {
		t.Fatal("symlink escape accepted")
	}
	r := httptest.NewRequest("GET", "/api/file?path=escape/secret", nil)
	w := httptest.NewRecorder()
	a.fileAPI(w, r)
	if w.Code == 200 || strings.Contains(w.Body.String(), "host secret") {
		t.Fatalf("outside file exposed: %d %q", w.Code, w.Body.String())
	}
}

func TestWorkspaceBoundaryAllowsCanonicalInRootPath(t *testing.T) {
	a := hardeningTestApp(t)
	root := a.root
	if err := os.Mkdir(filepath.Join(root, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p, err := a.resolve("alias")
	if err != nil || p != filepath.Join(root, "real") {
		t.Fatalf("in-root link: %q %v", p, err)
	}
}

func TestConversationCannotRestoreEscapingWorkspace(t *testing.T) {
	a := hardeningTestApp(t)
	root := a.root
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	c := conversation{ID: "c1", Workspace: "escape"}
	if err := a.saveConversation(&c); err == nil {
		t.Fatal("escaping restored workspace accepted")
	}
}
