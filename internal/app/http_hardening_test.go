package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hardeningTestApp(t *testing.T) *App {
	t.Helper()
	root := t.TempDir()
	a, err := New(Options{Root: root, DataDir: filepath.Join(root, "data"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestHTTPServerHasBoundedNonStreamingTimeouts(t *testing.T) {
	s := hardeningTestApp(t).httpServer()
	if s.ReadHeaderTimeout != 5*time.Second || s.ReadTimeout != 30*time.Second || s.IdleTimeout != 60*time.Second {
		t.Fatalf("unexpected timeouts: header=%v read=%v idle=%v", s.ReadHeaderTimeout, s.ReadTimeout, s.IdleTimeout)
	}
	if s.WriteTimeout != 0 {
		t.Fatalf("agent streaming requires no global write timeout, got %v", s.WriteTimeout)
	}
	if s.MaxHeaderBytes != 1<<20 {
		t.Fatalf("MaxHeaderBytes=%d", s.MaxHeaderBytes)
	}
}

func TestRoutePolicyRejectsMethodAndContentType(t *testing.T) {
	a := hardeningTestApp(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1/api/auth/login", nil)
	req.Header.Set("Origin", "http://127.0.0.1")
	a.httpServer().Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("method response=%d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/auth/login", strings.NewReader(`{"Password":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "http://127.0.0.1")
	a.httpServer().Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type response=%d", rec.Code)
	}
}

func TestStrictJSONRejectsTrailingAndUnknownData(t *testing.T) {
	for _, body := range []string{`{"Password":"x"}{}`, `{"Password":"x","unexpected":true}`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
		if decodeJSON(rec, req, &struct{ Password string }{}, 1024) || rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q was accepted, status=%d", body, rec.Code)
		}
	}
}

func TestSecurityHeadersCoverSuccessAndFailure(t *testing.T) {
	a := hardeningTestApp(t)
	for _, path := range []string{"/api/auth/state", "/api/auth/login"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		a.httpServer().Handler.ServeHTTP(rec, req)
		for _, header := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Cross-Origin-Opener-Policy", "Permissions-Policy"} {
			if rec.Header().Get(header) == "" {
				t.Fatalf("%s missing on %s status %d", header, path, rec.Code)
			}
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("cache control missing on %s", path)
		}
	}
}

func TestPanicIsContained(t *testing.T) {
	a := hardeningTestApp(t)
	h := a.security(a.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("canary") })))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/canary", nil))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "canary") {
		t.Fatalf("panic response=%d %q", rec.Code, rec.Body.String())
	}
}
