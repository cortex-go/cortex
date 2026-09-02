package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		return h(name, args...)
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

// saw reports whether any previously recorded call contained needle. The
// current call is excluded, so handlers can branch on prior steps (e.g. a
// post-stop state query that must report inactive).
func (f *fakeRunner) saw(needle string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls[:len(f.calls)-1] {
		if strings.Contains(c, needle) {
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

func testOptsHostPort(host, port string) serviceOptions {
	return serviceOptions{host: host, port: port, root: "/home/nick", data: "/home/nick/.config/cortex"}
}

func TestBuildCortexUnitHostPort(t *testing.T) {
	// A default install resolves to 127.0.0.1:7331 and the unit must record it
	// through --host/--port so it survives login, restart and reboot.
	def := buildCortexUnit("/usr/local/bin/cortex", testOptsHostPort("127.0.0.1", "7331"))
	for _, want := range []string{`"--host" "127.0.0.1"`, `"--port" "7331"`, `# cortex-listen: 127.0.0.1:7331`} {
		if !strings.Contains(def, want) {
			t.Fatalf("default unit missing %q\n%s", want, def)
		}
	}
	if strings.Contains(def, "--listen") {
		t.Fatal("default unit must use --host/--port, not legacy --listen")
	}
	if _, err := readManagedUnitBytes(t, []byte(def)); err != nil {
		t.Fatalf("default unit should validate: %v", err)
	}

	// An explicit 0.0.0.0:7401 install records that exact listener.
	wide := buildCortexUnit("/usr/local/bin/cortex", testOptsHostPort("0.0.0.0", "7401"))
	for _, want := range []string{`"--host" "0.0.0.0"`, `"--port" "7401"`, `# cortex-listen: 0.0.0.0:7401`} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide unit missing %q\n%s", want, wide)
		}
	}
	if _, err := readManagedUnitBytes(t, []byte(wide)); err != nil {
		t.Fatalf("wide unit should validate: %v", err)
	}
}

func TestBuildCortexUnitHostPortCanonical(t *testing.T) {
	// Whitespace-surrounded host/port must never leak into the unit metadata or
	// ExecStart; only the canonical trimmed values are recorded.
	opts := serviceOptions{host: "  127.0.0.1  ", port: "  7401  ", root: "/home/nick", data: "/home/nick/.config/cortex"}
	unit := buildCortexUnit("/usr/local/bin/cortex", opts)
	for _, want := range []string{`"--host" "127.0.0.1"`, `"--port" "7401"`, `# cortex-listen: 127.0.0.1:7401`} {
		if !strings.Contains(unit, want) {
			t.Fatalf("canonical unit missing %q\n%s", want, unit)
		}
	}
	for _, bad := range []string{`"  127.0.0.1  "`, `"  7401  "`, `# cortex-listen:   127.0.0.1:7401`} {
		if strings.Contains(unit, bad) {
			t.Fatalf("unit leaked untrimmed value %q\n%s", bad, unit)
		}
	}
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

// fakeSystemd is a stateful model of a per-user systemd manager used by the
// service transaction tests. It holds the unit's enablement and active states,
// answers is-enabled/is-active and the lifecycle verbs against that model, and
// records every call so tests can assert both the exact calls and the final
// state rather than relying on substring assertions. The unit's loaded state is
// derived from the managed unit file's presence, so a rollback that removes the
// unit also makes is-enabled report not-found and is-active report inactive.
type fakeSystemd struct {
	mu       sync.Mutex
	unitPath string
	enable   bool // persistent enablement link
	enableRT bool // runtime enablement link
	mask     bool // persistent mask
	maskRT   bool // runtime mask
	active   string
	// Overrides simulate systemctl reports for prior states that have no
	// corresponding link layer (for example not-found on a loaded unit, or
	// static/alias unit-file states). They are used only to exercise the
	// refuse-before-mutation path.
	overrideEnabled string
	overrideActive  string
	failVerb        string
	calls           []string
}

func newFakeSystemd(unitPath string) *fakeSystemd {
	return &fakeSystemd{unitPath: unitPath, active: "inactive"}
}

func exitForEnabled(word string) int {
	switch word {
	case "enabled", "enabled-runtime", "static", "alias", "indirect", "generated":
		return 0
	case "disabled", "masked", "masked-runtime", "linked", "linked-runtime", "transient":
		return 1
	case "not-found", "unknown":
		return 4
	}
	return 1
}

func exitForActive(word string) int {
	switch word {
	case "active", "reloading":
		return 0
	case "inactive", "dead", "failed", "activating", "deactivating", "maintenance":
		return 3
	case "not-found", "unknown":
		return 4
	}
	return 3
}

// enabledWord derives the is-enabled word from the persistent/runtime
// enablement and mask layers, matching systemd's precedence: a persistent mask
// reports masked, a runtime-only mask masked-runtime, persistent enablement
// enabled, runtime-only enablement enabled-runtime, and otherwise disabled when
// the unit file is present.
func (f *fakeSystemd) enabledWord() string {
	if f.overrideEnabled != "" {
		return f.overrideEnabled
	}
	switch {
	case f.mask:
		return "masked"
	case f.maskRT:
		return "masked-runtime"
	case f.enable:
		return "enabled"
	case f.enableRT:
		return "enabled-runtime"
	}
	if _, err := os.Stat(f.unitPath); err != nil {
		return "not-found"
	}
	return "disabled"
}

func (f *fakeSystemd) activeWord() string {
	if f.overrideActive != "" {
		return f.overrideActive
	}
	if _, err := os.Stat(f.unitPath); err != nil {
		return "inactive"
	}
	return f.active
}

// runner returns a serviceRunner wired to the fake model.
func (f *fakeSystemd) runner() *fakeRunner {
	fr := &fakeRunner{}
	fr.handler = func(name string, args ...string) (string, int, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		verb := ""
		for _, a := range args {
			if a != "--user" {
				verb = a
				break
			}
		}
		fail := f.failVerb != "" && f.failVerb == verb
		if fail {
			f.failVerb = ""
		}
		if verb == "daemon-reload" {
			if fail {
				return "reload failed", 1, nil
			}
			return "", 0, nil
		}
		if verb == "is-enabled" {
			word := f.enabledWord()
			return word, exitForEnabled(word), nil
		}
		if verb == "is-active" {
			word := f.activeWord()
			return word, exitForActive(word), nil
		}
		// enable, enable --runtime, disable, mask, mask --runtime, start,
		// restart, stop
		switch verb {
		case "enable", "disable", "mask":
			if containsStr(args, "--runtime") {
				verb = verb + "-runtime"
			}
			switch verb {
			case "enable":
				// A masked unit cannot be enabled.
				if f.mask || f.maskRT {
					return "Failed to enable unit: masked", 1, nil
				}
				f.enable = true
			case "enable-runtime":
				if f.mask || f.maskRT {
					return "Failed to enable unit: masked", 1, nil
				}
				f.enableRT = true
			case "disable":
				// Normalization: remove both persistent and runtime links.
				f.enable = false
				f.enableRT = false
			case "mask":
				f.mask = true
				f.enable = false
				f.enableRT = false
			case "mask-runtime":
				f.maskRT = true
				f.enable = false
				f.enableRT = false
			}
		case "start", "restart":
			if f.mask || f.maskRT {
				return "Failed to start unit: masked", 1, nil
			}
			f.active = "active"
		case "stop":
			f.active = "inactive"
		}
		if fail {
			return verb + " failed", 1, nil
		}
		return "", 0, nil
	}
	return fr
}

// setState seeds a prior enablement/active pair. Enablement words that have a
// real link representation populate the layers; words that only systemctl could
// report for a synthetic state (not-found, static, dead, unknown, ...) are kept
// as overrides so the refuse-before-mutation path sees them verbatim.
func (f *fakeSystemd) setState(enabled, active string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overrideEnabled = ""
	f.overrideActive = ""
	f.enable = false
	f.enableRT = false
	f.mask = false
	f.maskRT = false
	switch enabled {
	case "enabled":
		f.enable = true
	case "enabled-runtime":
		f.enableRT = true
	case "masked":
		f.mask = true
	case "masked-runtime":
		f.maskRT = true
	case "disabled":
		// No link layers; is-enabled derives from the unit file's presence.
	default:
		f.overrideEnabled = enabled
	}
	switch active {
	case "active", "inactive":
		f.active = active
	default:
		f.active = active
		f.overrideActive = active
	}
}

func (f *fakeSystemd) callsContain(needle string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, needle) {
			return true
		}
	}
	return false
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestInstallFreshAndChanged(t *testing.T) {
	t.Run("fresh install publishes and starts", func(t *testing.T) {
		m, _, _ := newFakeManager(t)
		fs := newFakeSystemd(m.unitPath)
		m.run = fs.runner()
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
		for _, want := range []string{"daemon-reload", "enable cortex.service", "restart cortex.service"} {
			if !fs.callsContain(want) {
				t.Fatalf("fresh install did not call %q\ncalls: %v", want, fs.calls)
			}
		}
		if fs.activeWord() != "active" {
			t.Fatalf("service not started: %q", fs.activeWord())
		}
		if fs.enabledWord() != "enabled" {
			t.Fatalf("service not enabled: %q", fs.enabledWord())
		}
	})

	t.Run("identical reinstall on enabled active service is a true no-op", func(t *testing.T) {
		m, _, _ := newFakeManager(t)
		fs := newFakeSystemd(m.unitPath)
		m.run = fs.runner()
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fs.setState("enabled", "active")
		fs.calls = nil
		fi, _ := os.Stat(m.unitPath)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatalf("no-op reinstall: %v", err)
		}
		for _, forbid := range []string{"daemon-reload", "enable ", "restart ", "start "} {
			if fs.callsContain(forbid) {
				t.Fatalf("no-op reinstall mutated systemd (%q)\ncalls: %v", forbid, fs.calls)
			}
		}
		if fi2, _ := os.Stat(m.unitPath); !fi.ModTime().Equal(fi2.ModTime()) {
			t.Fatal("no-op reinstall rewrote the unit file")
		}
	})

	t.Run("changed configuration restarts the service", func(t *testing.T) {
		m, _, _ := newFakeManager(t)
		fs := newFakeSystemd(m.unitPath)
		m.run = fs.runner()
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fs.setState("enabled", "active")
		fs.calls = nil
		if err := m.install(testOpts("127.0.0.1:7332"), os.Stderr); err != nil {
			t.Fatalf("changed reinstall: %v", err)
		}
		if !fs.callsContain("restart cortex.service") {
			t.Fatalf("changed config did not restart\ncalls: %v", fs.calls)
		}
	})

	t.Run("unchanged unit on inactive service starts it", func(t *testing.T) {
		m, _, _ := newFakeManager(t)
		fs := newFakeSystemd(m.unitPath)
		m.run = fs.runner()
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fs.setState("enabled", "inactive")
		fs.calls = nil
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatalf("inactive reinstall: %v", err)
		}
		if !fs.callsContain("start cortex.service") {
			t.Fatalf("inactive service was not started\ncalls: %v", fs.calls)
		}
		if fs.callsContain("daemon-reload") || fs.callsContain("restart ") {
			t.Fatalf("unchanged inactive reinstall did unnecessary work\ncalls: %v", fs.calls)
		}
	})

	t.Run("unchanged unit on disabled service enables then starts", func(t *testing.T) {
		m, _, _ := newFakeManager(t)
		fs := newFakeSystemd(m.unitPath)
		m.run = fs.runner()
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fs.setState("disabled", "inactive")
		fs.calls = nil
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatalf("disabled reinstall: %v", err)
		}
		if !fs.callsContain("enable cortex.service") || !fs.callsContain("start cortex.service") {
			t.Fatalf("disabled service was not enabled and started\ncalls: %v", fs.calls)
		}
		if fs.callsContain("daemon-reload") {
			t.Fatalf("unchanged disabled reinstall reloaded systemd needlessly\ncalls: %v", fs.calls)
		}
	})
}

// TestInstallRollbackMatrix establishes every accepted prior raw-state pair,
// injects a failure after mutation, runs rollback, and asserts the final unit
// bytes and raw is-enabled/is-active words exactly match the initial state.
// Pairs that cannot be restored exactly by the rollback ordering are asserted
// to be refused before any file or lifecycle mutation.
func TestInstallRollbackMatrix(t *testing.T) {
	pairs := []struct {
		enabled, active string
		restorable      bool
	}{
		{"enabled", "active", true}, {"enabled", "inactive", true},
		{"enabled-runtime", "active", true}, {"enabled-runtime", "inactive", true},
		{"disabled", "active", true}, {"disabled", "inactive", true},
		// Refused: masked states (the install itself cannot enable a masked
		// unit), not exact (dead/unknown/not-found active, not-found enabled),
		// transient/failed, and unit-file enablement states.
		{"enabled", "dead", false}, {"enabled", "unknown", false}, {"enabled", "not-found", false},
		{"enabled-runtime", "failed", false}, {"enabled-runtime", "reloading", false},
		{"disabled", "refreshing", false}, {"disabled", "activating", false},
		{"disabled", "deactivating", false}, {"disabled", "maintenance", false},
		{"masked", "active", false}, {"masked-runtime", "active", false},
		{"masked", "failed", false},
		{"not-found", "active", false}, {"not-found", "inactive", false},
		{"static", "active", false}, {"alias", "active", false}, {"indirect", "active", false},
		{"generated", "active", false}, {"linked", "active", false},
		{"linked-runtime", "active", false}, {"transient", "active", false},
		{"unknown", "active", false},
	}
	for _, p := range pairs {
		t.Run("enabled="+p.enabled+"/active="+p.active, func(t *testing.T) {
			m, _, _ := newFakeManager(t)
			fs := newFakeSystemd(m.unitPath)
			m.run = fs.runner()
			if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
				t.Fatal(err)
			}
			priorUnit, _ := os.ReadFile(m.unitPath)
			fs.setState(p.enabled, p.active)
			fs.calls = nil
			if !p.restorable {
				err := m.install(testOpts("127.0.0.1:7333"), os.Stderr)
				if err == nil {
					t.Fatalf("non-restorable pair (%q/%q) was not refused", p.enabled, p.active)
				}
				after, _ := os.ReadFile(m.unitPath)
				if string(after) != string(priorUnit) {
					t.Fatal("refusal changed the unit file")
				}
				for _, forbid := range []string{"daemon-reload", "enable ", "mask ", "disable ", "restart ", "start ", "stop "} {
					if fs.callsContain(forbid) {
						t.Fatalf("refusal performed a lifecycle mutation (%q)\ncalls: %v", forbid, fs.calls)
					}
				}
				return
			}
			// Restorable: fail the final lifecycle step, run rollback, and assert
			// the final raw state exactly matches the prior raw state.
			fs.failVerb = "restart"
			err := m.install(testOpts("127.0.0.1:7333"), os.Stderr)
			if err == nil {
				t.Fatalf("install should fail at restart for restorable pair (%q/%q)", p.enabled, p.active)
			}
			after, _ := os.ReadFile(m.unitPath)
			if string(after) != string(priorUnit) {
				t.Fatal("rollback did not restore the prior unit bytes")
			}
			ew, _, _ := m.systemctl("is-enabled", m.unitName)
			aw, _, _ := m.systemctl("is-active", m.unitName)
			if strings.TrimSpace(ew) != p.enabled || strings.TrimSpace(aw) != p.active {
				t.Fatalf("rollback final raw state %q/%q want %q/%q", ew, aw, p.enabled, p.active)
			}
			if fs.enabledWord() != p.enabled || fs.activeWord() != p.active {
				t.Fatalf("rollback final model state %q/%q want %q/%q", fs.enabledWord(), fs.activeWord(), p.enabled, p.active)
			}
		})
	}
}

// TestInstallFailureRestoresPriorState exercises every lifecycle step failing
// for both fresh and reinstall paths and asserts the exact restored state via
// the stateful model, the unit bytes, and the presence of the unit file.
// TestInstallRestoresEnablementLayers proves the rollback normalizes
// enablement links before recreating them, so a runtime-only prior never keeps
// the persistent link created by the attempted install.
func TestInstallRestoresEnablementLayers(t *testing.T) {
	cases := []struct {
		prior              string
		wantEnable, wantRT bool
	}{
		{"enabled", true, false},
		{"enabled-runtime", false, true},
		{"disabled", false, false},
	}
	for _, tc := range cases {
		t.Run("prior="+tc.prior, func(t *testing.T) {
			m, _, _ := newFakeManager(t)
			fs := newFakeSystemd(m.unitPath)
			m.run = fs.runner()
			if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
				t.Fatal(err)
			}
			fs.setState(tc.prior, "inactive")
			fs.failVerb = "restart"
			if err := m.install(testOpts("127.0.0.1:7333"), os.Stderr); err == nil {
				t.Fatal("install should fail at restart")
			}
			ew, _, _ := m.systemctl("is-enabled", m.unitName)
			if strings.TrimSpace(ew) != tc.prior {
				t.Fatalf("is-enabled %q want %q", ew, tc.prior)
			}
			if fs.enable != tc.wantEnable || fs.enableRT != tc.wantRT {
				t.Fatalf("links enable=%v enableRT=%v want %v/%v", fs.enable, fs.enableRT, tc.wantEnable, tc.wantRT)
			}
			if fs.mask || fs.maskRT {
				t.Fatal("unexpected mask link after rollback")
			}
		})
	}
}

// TestInstallReachesInstalledStateForAcceptedPriors proves every accepted prior
// state lets the install reach the documented enabled-and-active state, so a
// state is never accepted merely because rollback could recover from an install
// that can never succeed.
func TestInstallReachesInstalledStateForAcceptedPriors(t *testing.T) {
	for _, p := range [][2]string{
		{"enabled", "active"}, {"enabled", "inactive"},
		{"enabled-runtime", "active"}, {"enabled-runtime", "inactive"},
		{"disabled", "active"}, {"disabled", "inactive"},
	} {
		t.Run(p[0]+"/"+p[1], func(t *testing.T) {
			m, _, _ := newFakeManager(t)
			fs := newFakeSystemd(m.unitPath)
			m.run = fs.runner()
			if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
				t.Fatal(err)
			}
			fs.setState(p[0], p[1])
			if err := m.install(testOpts("127.0.0.1:7333"), os.Stderr); err != nil {
				t.Fatalf("accepted prior %s/%s could not reach the installed state: %v", p[0], p[1], err)
			}
			ew, _, _ := m.systemctl("is-enabled", m.unitName)
			aw, _, _ := m.systemctl("is-active", m.unitName)
			if strings.TrimSpace(ew) != "enabled" || strings.TrimSpace(aw) != "active" {
				t.Fatalf("final %q/%q want enabled/active", ew, aw)
			}
		})
	}
}

func TestInstallFailureRestoresPriorState(t *testing.T) {
	steps := []struct {
		verb string
		call string
	}{
		{"daemon-reload", "daemon-reload"},
		{"enable", "enable cortex.service"},
		{"restart", "restart cortex.service"},
	}
	t.Run("fresh install", func(t *testing.T) {
		for _, st := range steps {
			t.Run(st.verb, func(t *testing.T) {
				m, _, _ := newFakeManager(t)
				fs := newFakeSystemd(m.unitPath)
				fr := fs.runner()
				m.run = fr
				fs.failVerb = st.verb
				err := m.install(testOpts("127.0.0.1:7331"), os.Stderr)
				if err == nil {
					t.Fatalf("install with %s failure did not fail", st.verb)
				}
				if _, statErr := os.Stat(m.unitPath); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("failed fresh install left the unit behind")
				}
				if fs.enabledWord() != "not-found" {
					t.Fatalf("failed fresh install left enablement %q", fs.enabledWord())
				}
				if fs.activeWord() != "inactive" {
					t.Fatalf("failed fresh install left active %q", fs.activeWord())
				}
			})
		}
	})
	t.Run("reinstall restores prior unit and lifecycle", func(t *testing.T) {
		for _, st := range steps {
			t.Run(st.verb, func(t *testing.T) {
				m, _, _ := newFakeManager(t)
				fs := newFakeSystemd(m.unitPath)
				m.run = fs.runner()
				if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
					t.Fatal(err)
				}
				priorUnit, _ := os.ReadFile(m.unitPath)
				fs.setState("enabled-runtime", "inactive")
				fs.failVerb = st.verb
				err := m.install(testOpts("127.0.0.1:7333"), os.Stderr)
				if err == nil {
					t.Fatalf("reinstall with %s failure did not fail", st.verb)
				}
				after, _ := os.ReadFile(m.unitPath)
				if string(priorUnit) != string(after) {
					t.Fatal("failed reinstall did not restore the prior unit bytes")
				}
				if fs.enabledWord() != "enabled-runtime" {
					t.Fatalf("rollback did not restore enablement %q", fs.enabledWord())
				}
				if fs.activeWord() != "inactive" {
					t.Fatalf("rollback did not restore active %q", fs.activeWord())
				}
			})
		}
	})
	t.Run("reinstall failure at restart restores enabled active prior", func(t *testing.T) {
		m, _, _ := newFakeManager(t)
		fs := newFakeSystemd(m.unitPath)
		m.run = fs.runner()
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fs.setState("enabled", "active")
		fs.failVerb = "restart"
		if err := m.install(testOpts("127.0.0.1:7333"), os.Stderr); err == nil {
			t.Fatal("reinstall with restart failure did not fail")
		}
		if fs.enabledWord() != "enabled" || fs.activeWord() != "active" {
			t.Fatalf("rollback did not restore enabled+active, got %q/%q", fs.enabledWord(), fs.activeWord())
		}
	})
	t.Run("failed fresh install keeps no enablement link or active service", func(t *testing.T) {
		m, _, _ := newFakeManager(t)
		fs := newFakeSystemd(m.unitPath)
		m.run = fs.runner()
		fs.failVerb = "restart"
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err == nil {
			t.Fatal("install did not fail")
		}
		word, _, _ := m.systemctl("is-enabled", m.unitName)
		if strings.TrimSpace(word) != "not-found" {
			t.Fatalf("unit still reports enablement %q after failed fresh install", word)
		}
		word2, _, _ := m.systemctl("is-active", m.unitName)
		if strings.TrimSpace(word2) != "inactive" {
			t.Fatalf("unit still reports active %q after failed fresh install", word2)
		}
		if _, statErr := os.Stat(m.unitPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("failed fresh install left the unit file")
		}
	})
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
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				if fr.saw("stop cortex.service") {
					return "inactive", 3, nil
				}
				return "active", 0, nil
			case fr.contains(args, "is-enabled"):
				if fr.saw("disable cortex.service") {
					return "disabled", 1, nil
				}
				return "enabled", 0, nil
			}
			return "", 0, nil
		}
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

func TestStatusReportsListenerURL(t *testing.T) {
	srv := jsonServer(t, 200, `{"ok":true}`, "application/json")
	listen := strings.TrimPrefix(srv.URL, "http://")
	m, fr, _ := newFakeManager(t)
	if err := m.install(testOpts(listen), os.Stderr); err != nil {
		t.Fatal(err)
	}
	fr.handler = activeHandler(fr)
	var out bytes.Buffer
	if err := m.status(&out, "1.0"); err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if !strings.Contains(out.String(), "listen:  "+listen) {
		t.Fatalf("status missing listen address:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "url:     http://"+listen) {
		t.Fatalf("status missing effective listener URL:\n%s", out.String())
	}
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

func TestStrictExitFailures(t *testing.T) {
	t.Run("install daemon-reload nonzero prevents enable and start", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		fr.handler = func(name string, args ...string) (string, int, error) {
			if fr.contains(args, "daemon-reload") {
				return "Failed to reload", 1, nil
			}
			return "", 0, nil
		}
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err == nil {
			t.Fatal("install succeeded despite a failed daemon-reload")
		}
		joined := strings.Join(fr.calls, "\n")
		if strings.Contains(joined, "enable cortex.service") || strings.Contains(joined, "restart cortex.service") {
			t.Fatalf("enable/restart ran after a failed daemon-reload: %s", joined)
		}
	})
	t.Run("install enable nonzero prevents restart", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		fr.handler = func(name string, args ...string) (string, int, error) {
			if fr.contains(args, "enable") {
				return "Failed to enable", 1, nil
			}
			return "", 0, nil
		}
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err == nil {
			t.Fatal("install succeeded despite a failed enable")
		}
		if strings.Contains(strings.Join(fr.calls, "\n"), "restart cortex.service") {
			t.Fatal("restart ran after a failed enable")
		}
	})
	t.Run("install restart nonzero reports failure", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		fr.handler = func(name string, args ...string) (string, int, error) {
			if fr.contains(args, "restart") {
				return "Failed to start", 1, nil
			}
			return "", 0, nil
		}
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err == nil {
			t.Fatal("install succeeded despite a failed restart")
		}
	})
	t.Run("lifecycle start/stop/restart nonzero reports failure", func(t *testing.T) {
		for _, verb := range []string{"start", "stop", "restart"} {
			m, fr, _ := newFakeManager(t)
			if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
				t.Fatal(err)
			}
			fr.calls = nil
			fr.handler = func(name string, args ...string) (string, int, error) {
				if fr.contains(args, verb) {
					return "Failed", 1, nil
				}
				return "", 0, nil
			}
			if err := m.action(verb, os.Stderr); err == nil {
				t.Fatalf("%s succeeded despite a nonzero exit", verb)
			}
		}
	})
	t.Run("uninstall stop nonzero preserves the unit", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.calls = nil
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
			t.Fatal("uninstall succeeded despite a failed stop")
		}
		if _, err := os.Stat(m.unitPath); err != nil {
			t.Fatalf("unit removed despite stop failure: %v", err)
		}
		joined := strings.Join(fr.calls, "\n")
		if strings.Contains(joined, "disable cortex.service") || strings.Contains(joined, "daemon-reload") {
			t.Fatalf("disable/reload ran after a failed stop: %s", joined)
		}
	})
	t.Run("uninstall disable nonzero preserves the unit", func(t *testing.T) {
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
				return "enabled", 0, nil
			case fr.contains(args, "disable"):
				return "Failed to disable", 1, nil
			}
			return "", 0, nil
		}
		if err := m.uninstall(os.Stderr); err == nil {
			t.Fatal("uninstall succeeded despite a failed disable")
		}
		if _, err := os.Stat(m.unitPath); err != nil {
			t.Fatalf("unit removed despite disable failure: %v", err)
		}
		if strings.Contains(strings.Join(fr.calls, "\n"), "daemon-reload") {
			t.Fatal("daemon-reload ran after a failed disable")
		}
	})
	t.Run("final daemon-reload nonzero is reported", func(t *testing.T) {
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
				return "disabled", 1, nil
			case fr.contains(args, "daemon-reload"):
				return "Failed to reload", 1, nil
			}
			return "", 0, nil
		}
		err := m.uninstall(os.Stderr)
		if err == nil {
			t.Fatal("uninstall did not report the daemon-reload failure")
		}
		if !strings.Contains(err.Error(), "reloading systemd") {
			t.Fatalf("daemon-reload failure not reported accurately: %v", err)
		}
	})
	t.Run("logs reports nonzero journalctl", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = func(name string, args ...string) (string, int, error) {
			if name == "journalctl" {
				return "no journal found", 1, nil
			}
			return "", 0, nil
		}
		if err := m.logs(false, os.Stderr); err == nil {
			t.Fatal("logs ignored a nonzero journalctl exit")
		}
	})
}

func stateTestManager(t *testing.T, activeOut string, activeCode int, enabledOut string, enabledCode int) (*serviceManager, *fakeRunner) {
	t.Helper()
	m, fr, _ := newFakeManager(t)
	fr.handler = func(name string, args ...string) (string, int, error) {
		switch {
		case fr.contains(args, "is-active"):
			return activeOut, activeCode, nil
		case fr.contains(args, "is-enabled"):
			return enabledOut, enabledCode, nil
		}
		return "", 0, nil
	}
	return m, fr
}

func TestStateExitValidation(t *testing.T) {
	valid := []struct {
		verb, out string
		code      int
		want      svcState
	}{
		// is-active exit-0 states: active and reloading (v252-256), plus
		// refreshing from v257; refreshing accepts either exit.
		{"is-active", "active", 0, stateActive},
		{"is-active", "reloading", 0, stateReloading},
		{"is-active", "refreshing", 0, stateRefreshing},
		{"is-active", "refreshing", 3, stateRefreshing},
		{"is-active", "inactive", 3, stateInactive},
		{"is-active", "dead", 3, stateInactive},
		{"is-active", "failed", 3, stateInactive},
		{"is-active", "activating", 3, stateTransition},
		{"is-active", "deactivating", 3, stateTransition},
		{"is-active", "maintenance", 3, stateTransition},
		{"is-active", "unknown", 3, stateUnknown},
		{"is-active", "not-found", 3, stateUnknown},
		{"is-active", "not-found", 4, stateUnknown},
		// is-enabled: only enabled/enabled-runtime are lifecycle enabled.
		{"is-enabled", "enabled", 0, stateEnabled},
		{"is-enabled", "enabled-runtime", 0, stateEnabled},
		// static/alias/indirect/generated exit 0 but are lifecycle not-enabled.
		{"is-enabled", "static", 0, stateNotEnabled},
		{"is-enabled", "alias", 0, stateNotEnabled},
		{"is-enabled", "indirect", 0, stateNotEnabled},
		{"is-enabled", "generated", 0, stateNotEnabled},
		{"is-enabled", "disabled", 1, stateNotEnabled},
		{"is-enabled", "linked", 1, stateNotEnabled},
		{"is-enabled", "linked-runtime", 1, stateNotEnabled},
		{"is-enabled", "transient", 1, stateNotEnabled},
		{"is-enabled", "not-found", 4, stateNotEnabled},
		{"is-enabled", "not-found", 1, stateNotEnabled},
		{"is-enabled", "masked", 1, stateMasked},
		{"is-enabled", "masked-runtime", 1, stateMasked},
	}
	for _, tc := range valid {
		m, _ := stateTestManager(t, tc.out, tc.code, tc.out, tc.code)
		got, err := m.queryState(tc.verb)
		if err != nil {
			t.Fatalf("%s %q exit %d: unexpected error %v", tc.verb, tc.out, tc.code, err)
		}
		if got != tc.want {
			t.Fatalf("%s %q exit %d = %q want %q", tc.verb, tc.out, tc.code, got, tc.want)
		}
	}
	invalid := []struct {
		verb, out string
		code      int
	}{
		{"is-active", "active", 3},
		{"is-active", "reloading", 3},
		{"is-active", "inactive", 0},
		{"is-active", "dead", 0},
		{"is-active", "failed", 0},
		{"is-active", "activating", 0},
		{"is-active", "maintenance", 0},
		{"is-active", "unknown", 0},
		{"is-active", "not-found", 0},
		{"is-enabled", "enabled", 1},
		{"is-enabled", "enabled-runtime", 1},
		{"is-enabled", "static", 1},
		{"is-enabled", "alias", 1},
		{"is-enabled", "indirect", 1},
		{"is-enabled", "generated", 1},
		{"is-enabled", "disabled", 0},
		{"is-enabled", "linked", 0},
		{"is-enabled", "transient", 0},
		{"is-enabled", "masked", 0},
	}
	for _, tc := range invalid {
		m, _ := stateTestManager(t, tc.out, tc.code, tc.out, tc.code)
		if _, err := m.queryState(tc.verb); err == nil {
			t.Fatalf("%s %q exit %d should be rejected as inconsistent", tc.verb, tc.out, tc.code)
		}
	}
}

func TestTransitionalUninstall(t *testing.T) {
	for _, tc := range []struct {
		state string
		code  int
	}{
		{"activating", 3}, {"deactivating", 3}, {"maintenance", 3}, {"refreshing", 3}, {"reloading", 0},
	} {
		t.Run(tc.state, func(t *testing.T) {
			m, fr, _ := newFakeManager(t)
			if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
				t.Fatal(err)
			}
			fr.calls = nil
			fr.handler = func(name string, args ...string) (string, int, error) {
				switch {
				case fr.contains(args, "is-active"):
					if fr.saw("stop cortex.service") {
						return "inactive", 3, nil
					}
					return tc.state, tc.code, nil
				case fr.contains(args, "is-enabled"):
					return "disabled", 1, nil
				}
				return "", 0, nil
			}
			if err := m.uninstall(os.Stderr); err != nil {
				t.Fatalf("uninstall of a %s service failed: %v", tc.state, err)
			}
			if _, err := os.Stat(m.unitPath); !os.IsNotExist(err) {
				t.Fatal("unit still present after uninstall")
			}
			joined := strings.Join(fr.calls, "\n")
			if !strings.Contains(joined, "stop cortex.service") {
				t.Fatalf("%s service was not stopped before removal: %s", tc.state, joined)
			}
		})
	}
	t.Run("stop succeeds but service still active preserves unit", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.calls = nil
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "active", 0, nil
			}
			return "", 0, nil
		}
		if err := m.uninstall(os.Stderr); err == nil {
			t.Fatal("uninstall succeeded even though the service stayed active")
		}
		if _, err := os.Stat(m.unitPath); err != nil {
			t.Fatalf("unit removed despite the service still being active: %v", err)
		}
		if strings.Contains(strings.Join(fr.calls, "\n"), "daemon-reload") {
			t.Fatal("daemon-reload ran although the service was not safely stopped")
		}
	})
	t.Run("disable succeeds but service still enabled preserves unit", func(t *testing.T) {
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
				return "enabled", 0, nil
			}
			return "", 0, nil
		}
		if err := m.uninstall(os.Stderr); err == nil {
			t.Fatal("uninstall succeeded even though the service stayed enabled")
		}
		if _, err := os.Stat(m.unitPath); err != nil {
			t.Fatalf("unit removed despite the service still being enabled: %v", err)
		}
		if strings.Contains(strings.Join(fr.calls, "\n"), "daemon-reload") {
			t.Fatal("daemon-reload ran although the service was not safely disabled")
		}
	})
}

func TestDisableVerificationFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name             string
		afterDisableOut  string
		afterDisableCode int
		afterDisableErr  error
	}{
		{"unknown", "unknown", 3, nil},
		{"unrecognized", "bogus-state", 1, nil},
		{"launch failure", "", -1, errors.New("bus gone")},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
					if fr.saw("disable cortex.service") {
						return tc.afterDisableOut, tc.afterDisableCode, tc.afterDisableErr
					}
					return "enabled", 0, nil
				}
				return "", 0, nil
			}
			if err := m.uninstall(os.Stderr); err == nil {
				t.Fatalf("uninstall proceeded after a %q disable verification", tc.name)
			}
			if _, err := os.Stat(m.unitPath); err != nil {
				t.Fatalf("unit removed despite failed disable verification: %v", err)
			}
			joined := strings.Join(fr.calls, "\n")
			if strings.Contains(joined, "daemon-reload") {
				t.Fatalf("daemon-reload ran after a failed disable verification: %s", joined)
			}
		})
	}
}

func TestIsEnabledUninstallPolicy(t *testing.T) {
	cases := []struct {
		state string
		code  int
		want  svcState
	}{
		{"enabled", 0, stateEnabled},
		{"enabled-runtime", 0, stateEnabled},
		{"static", 0, stateNotEnabled},
		{"alias", 0, stateNotEnabled},
		{"indirect", 0, stateNotEnabled},
		{"generated", 0, stateNotEnabled},
		{"disabled", 1, stateNotEnabled},
		{"linked", 1, stateNotEnabled},
		{"linked-runtime", 1, stateNotEnabled},
		{"transient", 1, stateNotEnabled},
		{"not-found", 4, stateNotEnabled},
		{"masked", 1, stateMasked},
		{"masked-runtime", 1, stateMasked},
		{"unknown", 1, stateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
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
					if fr.saw("disable cortex.service") {
						return "disabled", 1, nil
					}
					return tc.state, tc.code, nil
				}
				return "", 0, nil
			}
			err := m.uninstall(os.Stderr)
			joined := strings.Join(fr.calls, "\n")
			disabled := strings.Contains(joined, "disable cortex.service")
			if tc.want == stateEnabled {
				if !disabled {
					t.Fatalf("%s should invoke disable", tc.state)
				}
			} else if disabled {
				t.Fatalf("%s must not invoke disable", tc.state)
			}
			if tc.want == stateUnknown {
				if err == nil {
					t.Fatalf("%s should fail closed", tc.state)
				}
				if _, serr := os.Stat(m.unitPath); serr != nil {
					t.Fatalf("unit removed for unknown enablement %s: %v", tc.state, serr)
				}
			} else {
				if err != nil {
					t.Fatalf("uninstall for %s failed: %v", tc.state, err)
				}
				if _, serr := os.Stat(m.unitPath); !os.IsNotExist(serr) {
					t.Fatalf("unit not removed for %s", tc.state)
				}
			}
		})
	}
}

func TestUninstallRollback(t *testing.T) {
	backupFiles := func(dir string) []string {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var out []string
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".cortex.service.unit-backup-") {
				out = append(out, e.Name())
			}
		}
		return out
	}

	t.Run("success removes the unit and leaves no backup artifacts", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "inactive", 3, nil
			case fr.contains(args, "is-enabled"):
				return "disabled", 1, nil
			}
			return "", 0, nil
		}
		if err := m.uninstall(os.Stderr); err != nil {
			t.Fatalf("uninstall: %v", err)
		}
		if _, err := os.Stat(m.unitPath); !os.IsNotExist(err) {
			t.Fatal("unit still present after uninstall")
		}
		if len(backupFiles(filepath.Dir(m.unitPath))) != 0 {
			t.Fatal("backup artifacts remain after a successful uninstall")
		}
	})

	t.Run("reload failure restores the original unit and removes the backup", func(t *testing.T) {
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
		orig, _ := os.ReadFile(m.unitPath)
		origInfo, _ := os.Stat(m.unitPath)
		fr.calls = nil
		reloadCalls := 0
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "inactive", 3, nil
			case fr.contains(args, "is-enabled"):
				return "disabled", 1, nil
			case fr.contains(args, "daemon-reload"):
				reloadCalls++
				if reloadCalls == 1 {
					return "Failed to reload", 1, nil
				}
				return "", 0, nil // best-effort follow-up reload succeeds
			}
			return "", 0, nil
		}
		err := m.uninstall(os.Stderr)
		if err == nil {
			t.Fatal("uninstall did not report the reload failure")
		}
		if !strings.Contains(err.Error(), "restored") {
			t.Fatalf("reload failure did not restore the unit: %v", err)
		}
		got, _ := os.ReadFile(m.unitPath)
		if string(got) != string(orig) {
			t.Fatal("restored unit does not match the original byte-for-byte")
		}
		gotInfo, _ := os.Stat(m.unitPath)
		if !os.SameFile(origInfo, gotInfo) {
			t.Fatal("restored unit is not the original inode")
		}
		if len(backupFiles(filepath.Dir(m.unitPath))) != 0 {
			t.Fatal("backup artifacts remain after restoration")
		}
		if _, err := os.Stat(filepath.Join(dataDir, "conversations.db")); err != nil {
			t.Fatalf("data was lost during rollback: %v", err)
		}
	})

	t.Run("concurrent replacement is preserved and the backup is recoverable", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		orig, _ := os.ReadFile(m.unitPath)
		fr.calls = nil
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "inactive", 3, nil
			case fr.contains(args, "is-enabled"):
				return "disabled", 1, nil
			case fr.contains(args, "daemon-reload"):
				// Another actor replaces the unit while the reload fails.
				_ = os.WriteFile(m.unitPath, []byte("# replacement\n"), 0644)
				return "Failed to reload", 1, nil
			}
			return "", 0, nil
		}
		err := m.uninstall(os.Stderr)
		if err == nil {
			t.Fatal("uninstall did not report the reload failure")
		}
		if !strings.Contains(err.Error(), "concurrently created") || !strings.Contains(err.Error(), "preserved at") {
			t.Fatalf("restoration conflict not reported clearly: %v", err)
		}
		got, _ := os.ReadFile(m.unitPath)
		if string(got) != "# replacement\n" {
			t.Fatalf("concurrently created file was overwritten: %q", got)
		}
		backs := backupFiles(filepath.Dir(m.unitPath))
		if len(backs) != 1 {
			t.Fatalf("expected exactly one retained backup, got %v", backs)
		}
		recovered, _ := os.ReadFile(filepath.Join(filepath.Dir(m.unitPath), backs[0]))
		if string(recovered) != string(orig) {
			t.Fatal("retained backup does not contain the original unit")
		}
		if !strings.Contains(err.Error(), backs[0]) {
			t.Fatalf("reported recovery path does not match the retained backup: %v", err)
		}
	})

	t.Run("second reload failure is surfaced after restoration", func(t *testing.T) {
		m, fr, _ := newFakeManager(t)
		if err := m.install(testOpts("127.0.0.1:7331"), os.Stderr); err != nil {
			t.Fatal(err)
		}
		fr.handler = func(name string, args ...string) (string, int, error) {
			switch {
			case fr.contains(args, "is-active"):
				return "inactive", 3, nil
			case fr.contains(args, "is-enabled"):
				return "disabled", 1, nil
			case fr.contains(args, "daemon-reload"):
				return "Failed to reload", 1, nil
			}
			return "", 0, nil
		}
		err := m.uninstall(os.Stderr)
		if err == nil {
			t.Fatal("uninstall did not report the reload failure")
		}
		if !strings.Contains(err.Error(), "follow-up reload also failed") {
			t.Fatalf("second reload failure not surfaced: %v", err)
		}
	})
}

func TestBackupManagedUnitNoReplace(t *testing.T) {
	dirEntries := func(dir string) []string {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}

	t.Run("random source failure leaves the original intact", func(t *testing.T) {
		orig := randomSuffix
		randomSuffix = func() (string, error) { return "", errors.New("rand failed") }
		t.Cleanup(func() { randomSuffix = orig })
		dir := t.TempDir()
		unit := filepath.Join(dir, "app.service")
		if err := os.WriteFile(unit, []byte("unit"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := backupManagedUnit(unit); err == nil {
			t.Fatal("random-source failure should error")
		}
		if got, _ := os.ReadFile(unit); string(got) != "unit" {
			t.Fatalf("original changed: %q", got)
		}
		if entries := dirEntries(dir); len(entries) != 1 {
			t.Fatalf("unexpected entries after failure: %v", entries)
		}
	})

	t.Run("collision never overwrites a retained backup", func(t *testing.T) {
		orig := randomSuffix
		randomSuffix = func() (string, error) { return "aa", nil }
		t.Cleanup(func() { randomSuffix = orig })
		dir := t.TempDir()
		unit := filepath.Join(dir, "app.service")
		retained := filepath.Join(dir, ".app.service.unit-backup-aa")
		if err := os.WriteFile(unit, []byte("unit"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(retained, []byte("retained"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := backupManagedUnit(unit); err == nil {
			t.Fatal("all candidates collided; should error")
		}
		if got, _ := os.ReadFile(retained); string(got) != "retained" {
			t.Fatalf("retained backup was overwritten: %q", got)
		}
		if got, _ := os.ReadFile(unit); string(got) != "unit" {
			t.Fatalf("original changed: %q", got)
		}
	})

	t.Run("unlink failure aborts and leaves no artifact", func(t *testing.T) {
		origSuffix, origRemove := randomSuffix, removeFile
		randomSuffix = func() (string, error) { return "bb", nil }
		removeFile = func(p string) error { return errors.New("remove failed") }
		t.Cleanup(func() { randomSuffix, removeFile = origSuffix, origRemove })
		dir := t.TempDir()
		unit := filepath.Join(dir, "app.service")
		if err := os.WriteFile(unit, []byte("unit"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := backupManagedUnit(unit); err == nil {
			t.Fatal("unlink failure should error")
		}
		if got, _ := os.ReadFile(unit); string(got) != "unit" {
			t.Fatalf("original changed: %q", got)
		}
		if entries := dirEntries(dir); len(entries) != 1 {
			t.Fatalf("backup artifact left after aborted transaction: %v", entries)
		}
	})
}

func TestRunServiceDispatchErrors(t *testing.T) {
	if code := runService([]string{"--system", "install"}, version); code == 0 {
		t.Fatal("--system mode should be rejected")
	}
	if code := runService([]string{"bogus"}, version); code != 2 {
		t.Fatalf("unknown command exit=%d want 2", code)
	}
}

func TestReleaseMatrixBuilds(t *testing.T) {
	targets := []struct{ goos, goarch string }{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	}
	for _, tc := range targets {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			name := "cortex"
			if tc.goos == "windows" {
				name = "cortex.exe"
			}
			dir := t.TempDir()
			cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name), ".")
			cmd.Env = append(os.Environ(), "GOOS="+tc.goos, "GOARCH="+tc.goarch, "CGO_ENABLED=0")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s/%s build failed: %v\n%s", tc.goos, tc.goarch, err, out)
			}
		})
	}
}
