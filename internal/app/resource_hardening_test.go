package app

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestOverloadRejectsAndRecovers(t *testing.T) {
	a := hardeningTestApp(t)
	for i := 0; i < cap(a.requestSlots); i++ {
		a.requestSlots <- struct{}{}
	}
	h := a.overload(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("overload=%d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
	<-a.requestSlots
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 204 {
		t.Fatalf("did not recover: %d", w.Code)
	}
}

func TestConcurrentPublicSettingsIsRaceSafe(t *testing.T) {
	a := hardeningTestApp(t)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = a.publicSettings() }()
	}
	wg.Wait()
}
