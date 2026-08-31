package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	out   string
	err   error
}

func (f *fakeRunner) Run(name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return f.out, f.err
}

func (f *fakeRunner) Stream(name string, args ...string) (int, error) {
	_, _ = f.Run(name, args...)
	return 0, f.err
}

func newFakeManager(t *testing.T, out string, runErr error) (*serviceManager, *fakeRunner, string) {
	t.Helper()
	base := t.TempDir()
	unitPath := filepath.Join(base, "systemd", "user", "cortex.service")
	fr := &fakeRunner{out: out, err: runErr}
	m := &serviceManager{unitName: "cortex.service", unitPath: unitPath, exe: "/usr/local/bin/cortex", run: fr}
	return m, fr, base
}

func TestSystemdQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/usr/bin/cortex", `"/usr/bin/cortex"`},
		{`C:\cortex\app`, `"C:\\cortex\\app"`},
		{"has $dollar", `"has \$dollar"`},
		{`say "hi"`, `"say \"hi\""`},
		{"100%", `"100%%"`},
	}
	for _, c := range cases {
		if got := systemdQuote(c.in); got != c.want {
			t.Fatalf("systemdQuote(%q)=%q want %q", c.in, got, c.want)
		}
	}
	if got := systemdQuote("a`b"); got != "\"a\\`b\"" {
		t.Fatalf("backtick not escaped: %q", got)
	}
}

func TestRenderCortexUnit(t *testing.T) {
	unit := renderCortexUnit("/usr/local/bin/cortex", "127.0.0.1:7331", "/home/nick", "/home/nick/.config/cortex", "https://cortex.example.com", true, "/home/nick/.config/gh")
	if !strings.Contains(unit, cortexUnitMarker) {
		t.Fatal("missing managed marker")
	}
	if !strings.Contains(unit, `ExecStart="/usr/local/bin/cortex"`) {
		t.Fatal("ExecStart must invoke the binary directly")
	}
	if strings.Contains(unit, "sh -c") {
		t.Fatal("unit must not use a shell wrapper")
	}
	for _, want := range []string{`"--listen" "127.0.0.1:7331"`, `"--root" "/home/nick"`, `"--data" "/home/nick/.config/cortex"`, `"--public-origin" "https://cortex.example.com"`, `"--trust-proxy"`, `Environment=HOME=%h`, `Environment=GH_CONFIG_DIR="/home/nick/.config/gh"`, `WorkingDirectory="/usr/local/bin"`, `WantedBy=default.target`} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q\n%s", want, unit)
		}
	}
	unitNoGH := renderCortexUnit("/usr/local/bin/cortex", "127.0.0.1:7331", "/", "/data", "", false, "")
	if strings.Contains(unitNoGH, "GH_CONFIG_DIR") {
		t.Fatal("GH_CONFIG_DIR must be omitted when no host config exists")
	}
	if strings.Contains(unitNoGH, "--public-origin") || strings.Contains(unitNoGH, "--trust-proxy") {
		t.Fatal("optional flags must be omitted when unset")
	}
}

func TestResolveExecutable(t *testing.T) {
	if _, err := resolveExecutable(""); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := resolveExecutable("relative/path"); err == nil {
		t.Fatal("relative path accepted")
	}
	if _, err := resolveExecutable(os.TempDir() + "/cortex"); err == nil {
		t.Fatal("transient temp path accepted")
	}
	if _, err := resolveExecutable("/tmp/go-build123/b001/exe/cortex"); err == nil {
		t.Fatal("go-build path accepted")
	}
	good := "/bin/true"
	got, err := resolveExecutable(good)
	if err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	if got != good {
		t.Fatalf("resolved %q want %q", got, good)
	}
}

func TestInstallAndIdempotence(t *testing.T) {
	m, fr, _ := newFakeManager(t, "active", nil)
	opts := serviceOptions{listen: "127.0.0.1:7331", root: "/home/nick", data: "/home/nick/.config/cortex"}
	if err := m.install(opts, os.Stderr); err != nil {
		t.Fatalf("install: %v", err)
	}
	unit, err := os.ReadFile(m.unitPath)
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if !strings.Contains(string(unit), cortexUnitMarker) {
		t.Fatal("unit lacks marker")
	}
	joined := strings.Join(fr.calls, "\n")
	for _, want := range []string{"systemctl --user daemon-reload", "systemctl --user enable cortex.service", "systemctl --user start cortex.service"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("install did not call %q\n%s", want, joined)
		}
	}
	// Idempotent: reinstall overwrites the managed unit.
	fr.calls = nil
	if err := m.install(opts, os.Stderr); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if !strings.Contains(strings.Join(fr.calls, "\n"), "systemctl --user start cortex.service") {
		t.Fatal("reinstall did not restart the unit")
	}
}

func TestInstallRefusesUnmanagedUnit(t *testing.T) {
	m, _, base := newFakeManager(t, "", nil)
	if err := os.MkdirAll(filepath.Dir(m.unitPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.unitPath, []byte("# hand written\n[Service]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := m.install(serviceOptions{listen: "127.0.0.1:7331", root: "/", data: filepath.Join(base, "data")}, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "not a managed unit") {
		t.Fatalf("install overwrote an unmanaged unit: %v", err)
	}
}

func TestUninstallPreservesData(t *testing.T) {
	m, fr, base := newFakeManager(t, "", nil)
	dataDir := filepath.Join(base, "data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "conversations.db"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.install(serviceOptions{listen: "127.0.0.1:7331", root: "/", data: dataDir}, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if err := m.uninstall(os.Stderr); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(m.unitPath); !os.IsNotExist(err) {
		t.Fatal("unit still present after uninstall")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "conversations.db")); err != nil {
		t.Fatalf("data was removed by uninstall: %v", err)
	}
	joined := strings.Join(fr.calls, "\n")
	for _, want := range []string{"systemctl --user stop cortex.service", "systemctl --user disable cortex.service", "systemctl --user daemon-reload"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("uninstall did not call %q\n%s", want, joined)
		}
	}
}

func TestUninstallRefusesUnmanagedUnit(t *testing.T) {
	m, _, _ := newFakeManager(t, "", nil)
	if err := os.MkdirAll(filepath.Dir(m.unitPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.unitPath, []byte("# mine\n"), 0644); err != nil {
		t.Fatal(err)
	}
	err := m.uninstall(os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "not a managed unit") {
		t.Fatalf("uninstall removed an unmanaged unit: %v", err)
	}
}

func TestStatus(t *testing.T) {
	t.Run("missing unit", func(t *testing.T) {
		m, _, _ := newFakeManager(t, "", nil)
		if err := m.status(os.Stderr, "1.0", "127.0.0.1:7331", "/api/status"); err == nil {
			t.Fatal("status of a missing unit should fail")
		}
	})
	t.Run("failed state", func(t *testing.T) {
		m, _, _ := newFakeManager(t, "failed", nil)
		if err := m.install(serviceOptions{listen: "127.0.0.1:7331", root: "/", data: "/data"}, os.Stderr); err != nil {
			t.Fatal(err)
		}
		if err := m.status(os.Stderr, "1.0", "127.0.0.1:7331", "/api/status"); err == nil {
			t.Fatal("status of a failed unit should fail")
		}
	})
	t.Run("active and healthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
		defer srv.Close()
		m, _, _ := newFakeManager(t, "active", nil)
		if err := m.install(serviceOptions{listen: "127.0.0.1:7331", root: "/", data: "/data"}, os.Stderr); err != nil {
			t.Fatal(err)
		}
		listen := strings.TrimPrefix(srv.URL, "http://")
		if err := m.status(os.Stderr, "1.0", listen, ""); err != nil {
			t.Fatalf("healthy status failed: %v", err)
		}
	})
	t.Run("active but unhealthy", func(t *testing.T) {
		m, _, _ := newFakeManager(t, "active", nil)
		if err := m.install(serviceOptions{listen: "127.0.0.1:7331", root: "/", data: "/data"}, os.Stderr); err != nil {
			t.Fatal(err)
		}
		if err := m.status(os.Stderr, "1.0", "127.0.0.1:1", "/api/status"); err == nil {
			t.Fatal("unhealthy status should fail")
		}
	})
}

func TestActionFailurePropagation(t *testing.T) {
	m, fr, _ := newFakeManager(t, "", nil)
	fr.err = os.ErrNotExist
	fr.out = "Failed to start"
	if err := m.action("start", os.Stderr); err == nil {
		t.Fatal("start failure was swallowed")
	}
}

func TestRunServiceDispatchErrors(t *testing.T) {
	if code := runService([]string{"--system", "install"}, version); code == 0 {
		t.Fatal("--system mode should be rejected")
	}
	if code := runService([]string{"bogus"}, version); code != 2 {
		t.Fatalf("unknown command exit=%d want 2", code)
	}
}