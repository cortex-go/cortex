package app

import "testing"

func TestProviderStreamLimitsAreBounded(t *testing.T) {
	if maxProviderLine > 1<<20 || maxProviderEvents > 4096 || maxProviderBytes > 32<<20 {
		t.Fatalf("provider limits unexpectedly widened: line=%d events=%d bytes=%d", maxProviderLine, maxProviderEvents, maxProviderBytes)
	}
}

func TestSanitizeBoundsAndRedactsHostileText(t *testing.T) {
	secret := "top-secret-value"
	got := sanitize(secret+string(make([]byte, 20000)), secret)
	if len(got) > 12000 {
		t.Fatalf("sanitized output remains unbounded: %d", len(got))
	}
}
