package cortex

import (
	"io/fs"
	"strings"
	"testing"
)

func TestGeneratedFrontendEmbedded(t *testing.T) {
	frontend := PublicFS()
	for _, name := range []string{"index.html", "assets/css/style.css", "assets/js/script.js"} {
		if _, err := fs.Stat(frontend, name); err != nil {
			t.Fatalf("generated frontend %q is not embedded: %v", name, err)
		}
	}
	b, err := fs.ReadFile(frontend, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if !strings.Contains(html, "assets/css/style.css") || !strings.Contains(html, "assets/js/script.js") {
		t.Fatal("generated index does not reference Nift-tracked frontend assets")
	}
}
