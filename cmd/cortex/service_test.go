package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/cortex-go/cortex/internal/app"
)

type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	handler func(name string, args ...string) (string, int, error)
	out     string
	code    int
	err     error
}

func (f *fakeRunner) Run(name string, args ...string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if f.handler != nil {
		return f.handler(name, args...)
	}
	return f.out, f.code, f.err
}

func (f *fakeRunner) Stream(name string, args ...string) (int, error) {
	_, _, _ = f.Run(name, args...)
	return 0, f.err
}

func (f *fakeRunner) contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func newFakeManager(t *testing.T) (*serviceManager, *fakeRunner, string) {
	t.Helper()
	base := t.TempDir()
	unitPath := filepath.Join(base, "systemd", "user", "cortex.service")
	fr := &fakeRunner{}
	m := &serviceManager{unitName: "cortex.service", unitPath: unitPath, exe: "/usr/local/bin/cortex", run: fr}
	return m, fr, base
}

func testOpts(listen string) serviceOptions {
	return serviceOptions{listen: listen, root: "/home/nick", data: "/home/nick/.config/cortex"}
}

func jsonServer(t *testing.T, code int, body string, ct string) *httptest.Server {
	t.Helper()
	if ct == "" {
		ct = "application/json"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func activeHandler(fr *fakeRunner) func(name string, args ...string) (string, int, error) {
	return func(name string, args ...string) (string, int, error) {
		switch {
		case fr.contains(args, "is-active"):
			return "active", 0, nil
		case fr.contains(args, "is-enabled"):
			return "enabled", 0, nil
		}
		return "", 0, nil
	}
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

func TestBuildCortexUnit(t *testing.T) {
	opts := testOpts("127.0.0.1:9000")
	opts.publicOrigin = "https://cortex.example.com"
	opts.trustProxy = true
	opts.ghDir = "/home/nick/.config/gh"
	unit := buildCortexUnit("/usr/local/bin/cortex", opts)
	if !strings.Contains(unit, cortexUnitMarker) {
		t.Fatal("missing managed marker")
	}
	if !regexp.MustCompile(`(?m)^# cortex-managed: v1 sha256=[0-9a-f]{64}$`).MatchString(unit) {
		t.Fatalf("missing valid integrity header\n%s", unit)
	}
	if !strings.Contains(unit, `ExecStart="/usr/local/bin/cortex"`) {
		t.Fatal("ExecStart must invoke the binary directly")
	}
	if strings.Contains(unit, "sh -c") {
		t.Fatal("unit must not use a shell wrapper")
	}
	for _, want := range []string{`"--listen" "127.0.0.1:9000"`, `"--root" "/home/nick"`, `"--data" "/home/nick/.config/cortex"`, `"--public-origin" "https://cortex.example.com"`, `"--trust-proxy"`, `Environment=HOME=%h`, `Environment=GH_CONFIG_DIR="/home/nick/.config/gh"`, `# cortex-listen: 127.0.0.1:9000`, `# cortex-health: /api/health`, `WantedBy=default.target`} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q\n%s", want, unit)
		}
	}
	if _, err := readManagedUnitBytes(t, []byte(unit)); err != nil {
		t.Fatalf("built unit should validate: %v", err)
	}
}

func readManagedUnitBytes(t *testing.T, data []byte) (unitMeta, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cortex.service")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return unitMeta{}, err
	}
	return readManagedUnit(path)
}

func unitWithMeta(listen, health string) string {
	body := renderCortexUnitBody("/usr/local/bin/cortex", testOpts(listen))
	content := "# cortex-listen: " + listen + "\n# cortex-health: " + health + "\n" + body
	sum := sha256.Sum256([]byte(content))
	return cortexUnitMarker + "\n" + cortexManagedPrefix + "v1 sha256=" + hex.EncodeToString(sum[:]) + "\n" + content
}

func TestValidateNoControl(t *testing.T) {
	if err := validateNoControl("127.0.0.1:7331", "listen"); err != nil {
		t.Fatalf("valid listen rejected: %v", err)
	}
	for _, bad := range []string{"127.0.0.1:7331\nRestart=always", "a\x00b", "a\x0db", "a\x1bb"} {
		if err := validateNoControl(bad, "listen"); err == nil {
			t.Fatalf("control characters accepted: %q", bad)
		}
	}
	m, _, _ := newFakeManager(t)
	bad := testOpts("127.0.0.1:7331")
	bad.listen = "127.0.0.1:7331\nRestart=always"
	if err := m.install(bad, os.Stderr); err == nil {
		t.Fatal("install accepted a control-character listen address")
	}
}

func TestManagedUnitIntegrity(t *testing.T) {
	unit := buildCortexUnit("/usr/local/bin/cortex", testOpts("127.0.0.1:7331"))
	if _, err := readManagedUnitBytes(t, []byte(unit)); err != nil {
		t.Fatalf("valid unit rejected: %v", err)
	}

	t.Run("modified ExecStart", func(t *testing.T) {
		bad := strings.Replace(unit, "/usr/local/bin/cortex", "/usr/bin/cortex", 1)
		if _, err := readManagedUnitBytes(t, []byte(bad)); !errors.Is(err, errModified) {
			t.Fatalf("want errModified, got %v", err)
		}
	})
	t.Run("appended directive", func(t *testing.T) {
		bad := unit + "Environment=FOO=bar\n"
		if _, err := readManagedUnitBytes(t, []byte(bad)); !errors.Is(err, errModified) {
			t.Fatalf("want errModified, got %v", err)
		}
	})
	t.Run("removed directive", func(t *testing.T) {
		bad := strings.Replace(unit, "Restart=on-failure\n", "", 1)
		if _, err := readManagedUnitBytes(t, []byte(bad)); !errors.Is(err, errModified) {
			t.Fatalf("want errModified, got %v", err)
		}
	})
	t.Run("corrupted checksum", func(t *testing.T) {
		re := regexp.MustCompile(`(v1 sha256=)([0-9a-f]{64})`)
		loc := re.FindStringSubmatchIndex(unit)
		hashStart := loc[4]
		repl := "0"
		if unit[hashStart] == '0' {
			repl = "1"
		}
		bad := unit[:hashStart] + repl + unit[hashStart+1:]
		if _, err := readManagedUnitBytes(t, []byte(bad)); !errors.Is(err, errModified) {
			t.Fatalf("want errModified, got %v", err)
		}
	})
	t.Run("duplicate integrity header", func(t *testing.T) {
		lines := strings.SplitN(unit, "\n", 3)
		bad := strings.Join(lines[:2], "\n") + "\n" + lines[1] + "\n" + lines[2]
		if _, err := readManagedUnitBytes(t, []byte(bad)); !errors.Is(err, errMalformed) {
			t.Fatalf("want errMalformed, got %v", err)
		}
	})
	t.Run("missing marker", func(t *testing.T) {
		bad := "# hand written\n[Service]\n"
		if _, err := readManagedUnitBytes(t, []byte(bad)); !errors.Is(err, errNotManaged) {
			t.Fatalf("want errNotManaged, got %v", err)
		}
	})
	t.Run("wrong health path rejected", func(t *testing.T) {
		bad := unitWithMeta("127.0.0.1:7331", "/api/status")
		if _, err := readManagedUnitBytes(t, []byte(bad)); !errors.Is(err, errMalformed) {
			t.Fatalf("health path must be application-owned; want errMalformed, got %v", err)
		}
	})
	t.Run("duplicate metadata rejected even with valid checksum", func(t *testing.T) {
		body := renderCortexUnitBody("/usr/local/bin/cortex", testOpts("127.0.0.1:7331"))
		content := "# cortex-listen: 127.0.0.1:7331\n# cortex-listen: 127.0.0.1:9999\n# cortex-health: /api/health\n" + body
		sum := sha256.Sum256([]byte(content))
		bad := cortexUnitMarker + "\n" + cortexManagedPrefix + "v1 sha256=" + hex.EncodeToString(sum[:]) + "\n" + content
		if _, err := readManagedUnitBytes(t, []byte(bad)); !errors.Is(err, errMalformed) {
			t.Fatalf("duplicate metadata with recomputed checksum; want errMalformed, got %v", err)
		}
	})
	t.Run("malformed metadata with valid checksum", func(t *testing.T) {
		bad := unitWithMeta("", "/api/health")
		if _, err := readManagedUnitBytes(t, []byte(bad)); !errors.Is(err, errMalformed) {
			t.Fatalf("empty listen metadata; want errMalformed, got %v", err)
		}
		bad2 := unitWithMeta("127.0.0.1:7331", "")
		if _, err := readManagedUnitBytes(t, []byte(bad2)); !errors.Is(err, errMalformed) {
			t.Fatalf("empty health metadata; want errMalformed, got %v", err)
		}
		bad3 := unitWithMeta("127.0.0.1:7331\x00x", "/api/health")
		if _, err := readManagedUnitBytes(t, []byte(bad3)); !errors.Is(err, errMalformed) {
			t.Fatalf("control character in metadata; want errMalformed, got %v", err)
		}
	})
}

func TestInstallAndIdempotence(t *testing.T) {
	m, fr, _ := newFakeManager(t)
	if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
		t.Fatalf("install: %v", err)
	}
	unit, err := os.ReadFile(m.unitPath)
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if _, err := readManagedUnitBytes(t, unit); err != nil {
		t.Fatalf("installed unit invalid: %v", err)
	}
	joined := strings.Join(fr.calls, "\n")
	for _, want := range []string{"systemctl --user daemon-reload", "systemctl --user enable cortex.service", "systemctl --user start cortex.service"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("install did not call %q\n%s", want, joined)
		}
	}
	fr.calls = nil
	if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
		t.Fatalf("idempotent reinstall: %v", err)
	}
	if !strings.Contains(strings.Join(fr.calls, "\n"), "systemctl --user start cortex.service") {
		t.Fatal("reinstall did not restart the unit")
	}
}

func TestInstallRefusesForeignOrModifiedUnit(t *testing.T) {
	m, _, _ := newFakeManager(t)
	if err := os.MkdirAll(filepath.Dir(m.unitPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.unitPath, []byte("# hand written\n[Service]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err == nil {
		t.Fatal("install overwrote a foreign unit")
	}
}

func TestInstallRefusesModifiedManagedUnit(t *testing.T) {
	m, _, _ := newFakeManager(t)
	if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
		t.Fatal(err)
	}
	unit, _ := os.ReadFile(m.unitPath)
	tampered := strings.Replace(string(unit), "Restart=on-failure", "Restart=always", 1)
	if err := os.WriteFile(m.unitPath, []byte(tampered), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.install(testOpts("127.0.0.1:7332"), os.Stderr); err == nil {
		t.Fatal("install silently overwrote a modified managed unit")
	}
}

func TestActionsRequireManagedUnit(t *testing.T) {
	m, fr, _ := newFakeManager(t)
	if err := m.action("start", os.Stderr); err == nil {
		t.Fatal("start on a missing unit succeeded")
	}
	if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
		t.Fatal(err)
	}
	unit, _ := os.ReadFile(m.unitPath)
	if err := os.WriteFile(m.unitPath, []byte(strings.Replace(string(unit), "Restart=on-failure", "Restart=always", 1)), 0644); err != nil {
		t.Fatal(err)
	}
	fr.calls = nil
	if err := m.action("restart", os.Stderr); err == nil {
		t.Fatal("restart on a modified managed unit succeeded")
	}
	if len(fr.calls) != 0 {
		t.Fatalf("lifecycle command ran against a modified unit: %v", fr.calls)
	}
}

func TestUninstallFailClosed(t *testing.T) {
	t.Run("active and enabled", func(t *testing.T) {
		m, fr, base := newFakeManager(t)
		dataDir := filepath.Join(base, "data")
		if err := os.MkdirAll(dataDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "conversations.db"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		opts := testOpts("127.0.0.1:7331")
		opts.data = dataDir
		if err := m.install(opts, os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = activeHandler(fr)
		if err := m.uninstall(os.Stderr); err != nil {
			t.Fatalf("uninstall: %v", err)
		}
		if _, err := os.Stat(m.unitPath); !os.IsNotExist(err) {
			t.Fatal("unit still present after uninstall")
		}
		if _, err := os.Stat(filepath.Join(dataDir, "conversations.db")); err != nil {
			t.Fatalf("data removed by uninstall: %v", err)
		}
		joined := strings.Join(fr.calls, "\n")
		for _, want := range []string{"systemctl --user stop cortex.service", "systemctl --user disable cortex.service", "systemctl --user daemon-reload"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("uninstall did not call %q\n%s", want, joined)
			}
		}
	})
	t.Run("inactive and disabled with normal exit codes", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "inactive", 3, nil // normal nonzero for inactive
			case fr.contains(args, "is-enabled"):
				return "disabled", 1, nil // normal nonzero for disabled
			}
			return "", 0, nil
		}
		if err := m.uninstall(os.Stderr); err != nil {
			t.Fatalf("uninstall with inactive/disabled states: %v", err)
		}
		if _, err := os.Stat(m.unitPath); !os.IsNotExist(err) {
			t.Fatal("unit still present after uninstall")
		}
		joined := strings.Join(fr.calls, "\n")
		if strings.Contains(joined, "stop cortex.service") {
			t.Fatalf("stop was attempted on an inactive unit: %s", joined)
		}
		if strings.Contains(joined, "disable cortex.service") {
			t.Fatalf("disable was attempted on a disabled unit: %s", joined)
		}
		if !strings.Contains(joined, "daemon-reload") {
			t.Fatal("daemon-reload not called")
		}
	})
	t.Run("stop failure is not swallowed", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "active", 0, nil
			case fr.contains(args, "stop"):
				return "Failed to stop", 1, nil
			}
			return "", 0, nil
		}
		if err := m.uninstall(os.Stderr); err == nil {
			t.Fatal("uninstall succeeded despite stop failure")
		}
		if _, err := os.Stat(m.unitPath); err != nil {
			t.Fatalf("unit was removed despite stop failure: %v", err)
		}
	})
	t.Run("modified unit is refused", func(t *testing.T) {
		m, _, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		unit, _ := os.ReadFile(m.unitPath)
		if err := os.WriteFile(m.unitPath, []byte(strings.Replace(string(unit), "Restart=on-failure", "Restart=always", 1)), 0644); err != nil {
			t.Fatal(err)
		}
		if err := m.uninstall(os.Stderr); err == nil {
			t.Fatal("uninstall removed a modified managed unit")
		}
		if _, err := os.Stat(m.unitPath); err != nil {
			t.Fatalf("unit removed despite modification: %v", err)
		}
	})
}

func TestUninstallStateQueryFailures(t *testing.T) {
	t.Run("is-active launch failure", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.calls = nil
		fr.handler = func(name string, args ...string) (string, int, error) {
			if fr.contains(args, "is-active") {
				return "", -1, errors.New("systemctl not found")
			}
			return "", 0, nil
		}
		if err := m.uninstall(os.Stderr); err == nil {
			t.Fatal("uninstall ignored an is-active launch failure")
		}
		if _, err := os.Stat(m.unitPath); err != nil {
			t.Fatalf("unit removed despite query failure: %v", err)
		}
		joined := strings.Join(fr.calls, "\n")
		if strings.Contains(joined, "stop cortex.service") || strings.Contains(joined, "disable cortex.service") || strings.Contains(joined, "daemon-reload") {
			t.Fatalf("destructive steps ran after an active-state query failure: %s", joined)
		}
	})
	t.Run("is-active bus failure is not read as inactive", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = func(name string, args ...string) (string, int, error) {
			if fr.contains(args, "is-active") {
				return "Failed to connect to bus: No such file or directory", 1, nil
			}
			return "", 0, nil
		}
		if err := m.uninstall(os.Stderr); err == nil {
			t.Fatal("uninstall treated a bus failure as inactive")
		} else if !strings.Contains(err.Error(), "unrecognized") {
			t.Fatalf("bus failure should surface as unrecognized state, got: %v", err)
		}
		if _, serr := os.Stat(m.unitPath); serr != nil {
			t.Fatalf("unit removed despite bus failure: %v", serr)
		}
	})
	t.Run("unrecognized is-active output", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = func(name string, args ...string) (string, int, error) {
			if fr.contains(args, "is-active") {
				return "something-else", 1, nil
			}
			return "", 0, nil
		}
		if err := m.uninstall(os.Stderr); err == nil {
			t.Fatal("uninstall accepted unrecognized is-active output")
		}
		if _, err := os.Stat(m.unitPath); err != nil {
			t.Fatalf("unit removed despite unrecognized state: %v", err)
		}
	})
	t.Run("is-enabled bus failure", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.calls = nil
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "inactive", 3, nil
			case fr.contains(args, "is-enabled"):
				return "Failed to connect to bus: No such file or directory", 1, nil
			}
			return "", 0, nil
		}
		if err := m.uninstall(os.Stderr); err == nil {
			t.Fatal("uninstall ignored an is-enabled bus failure")
		}
		if _, err := os.Stat(m.unitPath); err != nil {
			t.Fatalf("unit removed despite enablement query failure: %v", err)
		}
		joined := strings.Join(fr.calls, "\n")
		if strings.Contains(joined, "disable cortex.service") || strings.Contains(joined, "daemon-reload") {
			t.Fatalf("disable/reload ran after an enablement query failure: %s", joined)
		}
	})
}

func TestStatus(t *testing.T) {
	t.Run("missing unit", func(t *testing.T) {
		m, _, _ := newFakeManager(t)
		if err := m.status(os.Stderr, "1.0"); err == nil {
			t.Fatal("status of a missing unit should fail")
		}
	})
	t.Run("invalid unit", func(t *testing.T) {
		m, _, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		unit, _ := os.ReadFile(m.unitPath)
		if err := os.WriteFile(m.unitPath, []byte(strings.Replace(string(unit), "Restart=on-failure", "Restart=always", 1)), 0644); err != nil {
			t.Fatal(err)
		}
		if err := m.status(os.Stderr, "1.0"); err == nil {
			t.Fatal("status of an invalid unit should fail")
		}
	})
	t.Run("inactive service", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "inactive", 3, nil
			case fr.contains(args, "is-enabled"):
				return "enabled", 0, nil
			}
			return "", 0, nil
		}
		if err := m.status(os.Stderr, "1.0"); err == nil {
			t.Fatal("status of an inactive service should fail")
		}
	})
	t.Run("failed service", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "failed", 3, nil
			case fr.contains(args, "is-enabled"):
				return "enabled", 0, nil
			}
			return "", 0, nil
		}
		if err := m.status(os.Stderr, "1.0"); err == nil {
			t.Fatal("status of a failed service should fail")
		}
	})
	t.Run("surfaces is-active bus failure", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "Failed to connect to bus", 1, nil
			case fr.contains(args, "is-enabled"):
				return "enabled", 0, nil
			}
			return "", 0, nil
		}
		err := m.status(os.Stderr, "1.0")
		if err == nil {
			t.Fatal("status swallowed an is-active bus failure")
		}
		if !strings.Contains(err.Error(), "unrecognized") {
			t.Fatalf("bus failure should surface as unrecognized state: %v", err)
		}
	})
	t.Run("uses installed listen address", func(t *testing.T) {
		srv := jsonServer(t, 200, `{"ok":true}`, "application/json")
		listen := strings.TrimPrefix(srv.URL, "http://")
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts(listen), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = activeHandler(fr)
		if err := m.status(os.Stderr, "1.0"); err != nil {
			t.Fatalf("status with installed listen failed: %v", err)
		}
	})
	t.Run("404 health response", func(t *testing.T) {
		srv := jsonServer(t, 404, `{"error":"not found"}`, "application/json")
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts(strings.TrimPrefix(srv.URL, "http://")), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = activeHandler(fr)
		if err := m.status(os.Stderr, "1.0"); err == nil {
			t.Fatal("status with a 404 health response should fail")
		}
	})
	t.Run("401 health response", func(t *testing.T) {
		srv := jsonServer(t, 401, `{"error":"unauthorized"}`, "application/json")
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts(strings.TrimPrefix(srv.URL, "http://")), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = activeHandler(fr)
		if err := m.status(os.Stderr, "1.0"); err == nil {
			t.Fatal("status with a 401 health response should fail")
		}
	})
	t.Run("non-JSON 200 health response", func(t *testing.T) {
		srv := jsonServer(t, 200, `ok`, "text/plain")
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts(strings.TrimPrefix(srv.URL, "http://")), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = activeHandler(fr)
		if err := m.status(os.Stderr, "1.0"); err == nil {
			t.Fatal("status with a non-JSON 200 health response should fail")
		}
	})
}

func TestRealCortexHealthAndStatusBoundary(t *testing.T) {
	root := t.TempDir()
	a, err := app.New(app.Options{Root: root, DataDir: filepath.Join(root, "data"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })

	// Unauthenticated /api/health is public and returns a minimal JSON object.
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/health status=%d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Fatalf("/api/health content type=%q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("/api/health body=%q", rec.Body.String())
	}

	// /api/status remains session-protected.
	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/status status=%d want 401", rec.Code)
	}

	// `cortex service status` accepts the real health response contract.
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	listen := strings.TrimPrefix(srv.URL, "http://")
	m, fr, _ := newFakeManager(t)
	if err := m.install(testOpts(listen), os.Stderr); err != nil {
		t.Fatal(err)
	}
	fr.handler = activeHandler(fr)
	if err := m.status(os.Stderr, "1.0"); err != nil {
		t.Fatalf("service status against the real Cortex health endpoint failed: %v", err)
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