package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// cortexUnitMarker marks unit files written by `cortex service`.
const cortexUnitMarker = "# Managed by cortex. Do not edit manually."

// cortexManagedPrefix introduces the versioned integrity header. The header is
// followed by a SHA-256 of everything below it (managed metadata plus the unit
// body), so any hand edit is detected on the next write, action or uninstall.
const cortexManagedPrefix = "# cortex-managed: "

// cortexHealthPath is the public, read-only liveness endpoint the service
// health check targets. /api/status is intentionally session-protected.
const cortexHealthPath = "/api/health"

var (
	errNotManaged = errors.New("not a managed unit")
	errMalformed  = errors.New("malformed managed unit header")
	errModified   = errors.New("managed unit body no longer matches its recorded checksum")
)

// serviceRunner abstracts systemctl/journalctl so the CLI is testable without
// touching a real systemd user manager. Run returns the captured combined
// output, the process exit code (0 on success, -1 when the command could not
// be launched) and a launch error only.
type serviceRunner interface {
	Run(name string, args ...string) (string, int, error)
	Stream(name string, args ...string) (int, error)
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, int, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode(), nil
	}
	return string(out), -1, err
}

func (execRunner) Stream(name string, args ...string) (int, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

type serviceManager struct {
	unitName string
	unitPath string
	exe      string
	run      serviceRunner
}

type unitMeta struct {
	listen string
	health string
}

type serviceOptions struct {
	listen       string
	root         string
	data         string
	publicOrigin string
	trustProxy   bool
	ghDir        string
}

func userUnitPath(unitName string) string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.Getenv("HOME")
	}
	return filepath.Join(base, "systemd", "user", unitName)
}

func (m *serviceManager) systemctl(args ...string) (string, int, error) {
	return m.run.Run("systemctl", append([]string{"--user"}, args...)...)
}

// svcState is a deliberately resolved systemd state category. It preserves
// enough information to distinguish definitely-active, safely-stopped,
// transitional and unknown states so uninstall can fail closed correctly.
type svcState string

const (
	stateActive     svcState = "active"
	stateInactive   svcState = "inactive"
	stateTransition svcState = "transitioning"
	stateUnknown    svcState = "unknown"
	stateEnabled    svcState = "enabled"
	stateDisabled   svcState = "disabled"
)

func stateName(s svcState) string { return string(s) }

// classifyActive maps an is-active output word to a state category and whether
// that state requires exit 0.
func classifyActive(word string) (svcState, bool, bool) {
	switch word {
	case "active":
		return stateActive, true, true
	case "inactive", "dead", "failed":
		return stateInactive, false, true
	case "activating", "deactivating", "reloading":
		return stateTransition, false, true
	case "unknown", "not-found":
		return stateUnknown, false, true
	}
	return "", false, false
}

// classifyEnabled maps an is-enabled output word to a state category and
// whether that state requires exit 0.
func classifyEnabled(word string) (svcState, bool, bool) {
	switch word {
	case "enabled":
		return stateEnabled, true, true
	case "disabled", "static", "indirect", "generated", "transient", "not-found":
		return stateDisabled, false, true
	case "unknown":
		return stateUnknown, false, true
	}
	return "", false, false
}

// queryState resolves an is-active or is-enabled state and validates the
// output/exit pair together: positive states require exit 0, negative states
// require a documented nonzero exit, launch failures surface as errors, and
// inconsistent or unrecognized pairs surface as errors.
func (m *serviceManager) queryState(verb string) (svcState, error) {
	out, code, err := m.systemctl(verb, m.unitName)
	if err != nil {
		return "", fmt.Errorf("cannot run systemctl %s %s: %w", verb, m.unitName, err)
	}
	word := strings.TrimSpace(out)
	var st svcState
	var needZero bool
	var ok bool
	switch verb {
	case "is-active":
		st, needZero, ok = classifyActive(word)
	case "is-enabled":
		st, needZero, ok = classifyEnabled(word)
	}
	if !ok {
		return "", fmt.Errorf("systemctl %s %s returned unrecognized state %q (exit %d)", verb, m.unitName, word, code)
	}
	if needZero && code != 0 {
		return "", fmt.Errorf("systemctl %s %s reported %q but exited %d; inconsistent state result", verb, m.unitName, word, code)
	}
	if !needZero && code == 0 {
		return "", fmt.Errorf("systemctl %s %s reported %q but exited 0; inconsistent state result", verb, m.unitName, word)
	}
	return st, nil
}

// bounded caps captured command output used in errors.
func bounded(s string) string {
	const max = 2000
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// systemctlSuccess runs a systemctl operation that must exit zero; launch
// failures and nonzero exits are both errors. Call sites must never discard
// the exit code at strict-operation sites.
func (m *serviceManager) systemctlSuccess(args ...string) error {
	out, code, err := m.systemctl(args...)
	if err != nil {
		return fmt.Errorf("cannot run systemctl %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		return fmt.Errorf("systemctl %s exited %d: %s", strings.Join(args, " "), code, bounded(strings.TrimSpace(out)))
	}
	return nil
}

// validateNoControl rejects CR, LF, NUL and other control characters so no
// user-supplied value can inject directives into a systemd unit or confuse
// status metadata.
func validateNoControl(v, what string) error {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return fmt.Errorf("%s %q contains a control character", what, v)
		}
	}
	return nil
}

// systemdQuote escapes a value for a systemd ExecStart/Environment directive.
func systemdQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '%':
			b.WriteString("%%")
		case '"', '\\', '$', '`':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// hostGHConfigDir resolves the host GitHub CLI configuration directory when
// hosts.yml exists. It is used to preserve gh access for the user service.
func hostGHConfigDir() string {
	if v := strings.TrimSpace(os.Getenv("GH_CONFIG_DIR")); v != "" {
		if _, err := os.Stat(filepath.Join(v, "hosts.yml")); err == nil {
			return v
		}
	}
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		d := filepath.Join(v, "gh")
		if _, err := os.Stat(filepath.Join(d, "hosts.yml")); err == nil {
			return d
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		d := filepath.Join(home, ".config", "gh")
		if _, err := os.Stat(filepath.Join(d, "hosts.yml")); err == nil {
			return d
		}
	}
	return ""
}

// renderCortexUnitBody renders the systemd directives (no managed header).
func renderCortexUnitBody(exe string, opts serviceOptions) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Cortex coding agent\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=" + systemdQuote(exe))
	b.WriteString(" " + systemdQuote("--listen") + " " + systemdQuote(opts.listen))
	b.WriteString(" " + systemdQuote("--root") + " " + systemdQuote(opts.root))
	b.WriteString(" " + systemdQuote("--data") + " " + systemdQuote(opts.data))
	if opts.publicOrigin != "" {
		b.WriteString(" " + systemdQuote("--public-origin") + " " + systemdQuote(opts.publicOrigin))
	}
	if opts.trustProxy {
		b.WriteString(" " + systemdQuote("--trust-proxy"))
	}
	b.WriteString("\n")
	b.WriteString("WorkingDirectory=" + systemdQuote(filepath.Dir(exe)) + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("Environment=HOME=%h\n")
	if opts.ghDir != "" {
		b.WriteString("Environment=GH_CONFIG_DIR=" + systemdQuote(opts.ghDir) + "\n")
	}
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// buildCortexUnit renders the full managed unit: a marker line, a versioned
// integrity header carrying the SHA-256 of the managed content below it, the
// runtime metadata (listen/health) used by `service status`, and the body.
func buildCortexUnit(exe string, opts serviceOptions) string {
	content := "# cortex-listen: " + opts.listen + "\n# cortex-health: " + cortexHealthPath + "\n" + renderCortexUnitBody(exe, opts)
	sum := sha256.Sum256([]byte(content))
	header := cortexUnitMarker + "\n" + cortexManagedPrefix + "v1 sha256=" + hex.EncodeToString(sum[:]) + "\n"
	return header + content
}

// readManagedUnit validates a unit's managed integrity and returns its runtime
// metadata. It returns errNotManaged when the marker is absent, errMalformed
// for duplicate/malformed headers or metadata, and errModified when the body
// below the header no longer matches the recorded checksum. Metadata must
// appear exactly once, contain no control characters, and the health path must
// be the application-owned health endpoint.
func readManagedUnit(path string) (unitMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return unitMeta{}, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 3 || lines[0] != cortexUnitMarker {
		return unitMeta{}, errNotManaged
	}
	count := 0
	for _, ln := range lines {
		if strings.HasPrefix(ln, cortexManagedPrefix) {
			count++
		}
	}
	if count != 1 || !strings.HasPrefix(lines[1], cortexManagedPrefix) {
		return unitMeta{}, errMalformed
	}
	sm := regexp.MustCompile(`^# cortex-managed: v1 sha256=([0-9a-f]{64})$`).FindStringSubmatch(lines[1])
	if sm == nil {
		return unitMeta{}, errMalformed
	}
	content := strings.Join(lines[2:], "\n")
	sum := sha256.Sum256([]byte(content))
	if hex.EncodeToString(sum[:]) != sm[1] {
		return unitMeta{}, errModified
	}
	meta := unitMeta{}
	listenSeen, healthSeen := 0, 0
	for _, ln := range lines[2:] {
		switch {
		case strings.HasPrefix(ln, "# cortex-listen: "):
			listenSeen++
			if listenSeen > 1 {
				return unitMeta{}, errMalformed
			}
			meta.listen = strings.TrimSpace(strings.TrimPrefix(ln, "# cortex-listen: "))
		case strings.HasPrefix(ln, "# cortex-health: "):
			healthSeen++
			if healthSeen > 1 {
				return unitMeta{}, errMalformed
			}
			meta.health = strings.TrimSpace(strings.TrimPrefix(ln, "# cortex-health: "))
		}
	}
	if listenSeen != 1 || healthSeen != 1 || meta.listen == "" || meta.health == "" {
		return unitMeta{}, errMalformed
	}
	if meta.health != cortexHealthPath {
		return unitMeta{}, errMalformed
	}
	if err := validateNoControl(meta.listen, "listen"); err != nil {
		return unitMeta{}, errMalformed
	}
	return meta, nil
}

// writeManagedUnit writes a unit atomically. An existing file is only replaced
// when it is a valid unmodified managed unit; hand-edited or foreign units are
// refused rather than silently overwritten.
func writeManagedUnit(path, content string) error {
	if _, err := os.Stat(path); err == nil {
		if _, err := readManagedUnit(path); err != nil {
			return fmt.Errorf("refusing to overwrite %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cortex-unit-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// resolveExecutable validates the executable path used in a unit. The supplied
// path must already be absolute; empty, relative or transient (build-cache,
// temp) paths are rejected so we never install a broken or ephemeral unit.
func resolveExecutable(exe string) (string, error) {
	if strings.TrimSpace(exe) == "" {
		return "", errors.New("empty executable path")
	}
	if !filepath.IsAbs(exe) {
		return "", fmt.Errorf("executable path %q is not absolute", exe)
	}
	abs := filepath.Clean(exe)
	if strings.HasPrefix(abs, os.TempDir()) {
		return "", fmt.Errorf("executable path %q is transient; install cortex somewhere stable first", abs)
	}
	if strings.Contains(abs, string(filepath.Separator)+"go-build"+string(filepath.Separator)) {
		return "", fmt.Errorf("executable path %q looks like a Go build cache path", abs)
	}
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		return "", fmt.Errorf("executable %q is not a file", abs)
	}
	return abs, nil
}

// healthCheck requires a 2xx JSON object response from the expected endpoint.
func healthCheck(url string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("expected 2xx, got HTTP %d", resp.StatusCode)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		return fmt.Errorf("expected a JSON response, got %q", resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("expected a JSON object response: %v", err)
	}
	return nil
}

// requireManaged verifies the unit file at the expected path is a valid
// unmodified managed unit before any lifecycle operation.
func (m *serviceManager) requireManaged(verb string) error {
	if _, err := readManagedUnit(m.unitPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("refusing to %s %s: unit is not installed", verb, m.unitName)
		}
		return fmt.Errorf("refusing to %s %s: %w", verb, m.unitName, err)
	}
	return nil
}

func (m *serviceManager) install(opts serviceOptions, out io.Writer) error {
	for _, v := range []struct{ val, name string }{
		{opts.listen, "listen"}, {opts.root, "root"}, {opts.data, "data"}, {opts.publicOrigin, "public-origin"}, {opts.ghDir, "gh config dir"},
	} {
		if err := validateNoControl(v.val, v.name); err != nil {
			return err
		}
	}
	unit := buildCortexUnit(m.exe, opts)
	if err := writeManagedUnit(m.unitPath, unit); err != nil {
		return err
	}
	for _, step := range []struct {
		verb string
		args []string
	}{
		{"reloading systemd", []string{"daemon-reload"}},
		{"enabling", []string{"enable", m.unitName}},
		{"starting", []string{"start", m.unitName}},
	} {
		if err := m.systemctlSuccess(step.args...); err != nil {
			return fmt.Errorf("%s %s: %w", step.verb, m.unitName, err)
		}
	}
	active, _, _ := m.systemctl("is-active", m.unitName)
	fmt.Fprintf(out, "unit:   %s\n", m.unitName)
	fmt.Fprintf(out, "file:   %s\n", m.unitPath)
	fmt.Fprintf(out, "state:  %s\n", strings.TrimSpace(active))
	fmt.Fprintf(out, "url:    http://%s\n", opts.listen)
	return nil
}

func (m *serviceManager) action(verb string, out io.Writer) error {
	if err := m.requireManaged(verb); err != nil {
		return err
	}
	o, code, err := m.systemctl(verb, m.unitName)
	if out != nil && strings.TrimSpace(o) != "" {
		fmt.Fprintln(out, strings.TrimSpace(o))
	}
	if err != nil {
		return fmt.Errorf("cannot run systemctl %s %s: %w", verb, m.unitName, err)
	}
	if code != 0 {
		return fmt.Errorf("systemctl %s %s exited %d: %s", verb, m.unitName, code, bounded(strings.TrimSpace(o)))
	}
	return nil
}

func (m *serviceManager) status(out io.Writer, version string) error {
	meta, err := readManagedUnit(m.unitPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is not installed (no unit at %s)", m.unitName, m.unitPath)
		}
		return fmt.Errorf("%s unit is not valid: %w", m.unitName, err)
	}
	enabled, err := m.queryState("is-enabled")
	if err != nil {
		return fmt.Errorf("cannot determine %s enablement state: %w", m.unitName, err)
	}
	active, err := m.queryState("is-active")
	if err != nil {
		return fmt.Errorf("cannot determine %s service state: %w", m.unitName, err)
	}
	pid, _, _ := m.systemctl("show", "-p", "MainPID", "--value", m.unitName)
	fmt.Fprintf(out, "unit:    %s\n", m.unitName)
	fmt.Fprintf(out, "file:    %s\n", m.unitPath)
	fmt.Fprintf(out, "enabled: %s\n", enabled)
	fmt.Fprintf(out, "active:  %s\n", active)
	fmt.Fprintf(out, "pid:     %s\n", strings.TrimSpace(pid))
	fmt.Fprintf(out, "version: %s\n", version)
	fmt.Fprintf(out, "listen:  %s\n", meta.listen)
	if active != stateActive {
		return fmt.Errorf("%s is %q; expected active", m.unitName, active)
	}
	if err := healthCheck("http://" + meta.listen + meta.health); err != nil {
		fmt.Fprintf(out, "health:  unreachable (%v)\n", err)
		return fmt.Errorf("service is active but its health check failed: %v", err)
	}
	fmt.Fprintln(out, "health:  ok")
	return nil
}

func (m *serviceManager) logs(follow bool, out io.Writer) error {
	if err := m.requireManaged("view logs for"); err != nil {
		return err
	}
	args := []string{"--user-unit", m.unitName}
	if follow {
		args = append(args, "-f")
		code, err := m.run.Stream("journalctl", args...)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("journalctl exited with status %d", code)
		}
		return nil
	}
	o, code, err := m.run.Run("journalctl", args...)
	if err != nil {
		return fmt.Errorf("cannot run journalctl: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("journalctl exited %d: %s", code, bounded(strings.TrimSpace(o)))
	}
	fmt.Fprint(out, o)
	return nil
}

func (m *serviceManager) uninstall(out io.Writer) error {
	if _, err := readManagedUnit(m.unitPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is not installed", m.unitName)
		}
		return fmt.Errorf("refusing to uninstall %s: %w", m.unitName, err)
	}
	active, err := m.queryState("is-active")
	if err != nil {
		return fmt.Errorf("cannot determine %s state before uninstall: %w", m.unitName, err)
	}
	if active == stateActive || active == stateTransition {
		if err := m.systemctlSuccess("stop", m.unitName); err != nil {
			return fmt.Errorf("stop %s failed: %w", m.unitName, err)
		}
		after, err := m.queryState("is-active")
		if err != nil {
			return fmt.Errorf("cannot verify %s stopped after stop: %w", m.unitName, err)
		}
		if after != stateInactive {
			return fmt.Errorf("%s still reports %q after stop; not removing the unit", m.unitName, stateName(after))
		}
	} else if active == stateInactive {
		fmt.Fprintf(out, "note: %s is inactive; nothing to stop\n", m.unitName)
	} else {
		return fmt.Errorf("%s is in %q; cannot confirm it is safely stopped before uninstall", m.unitName, stateName(active))
	}
	enabled, err := m.queryState("is-enabled")
	if err != nil {
		return fmt.Errorf("cannot determine %s enablement before uninstall: %w", m.unitName, err)
	}
	if enabled == stateEnabled {
		if err := m.systemctlSuccess("disable", m.unitName); err != nil {
			return fmt.Errorf("disable %s failed: %w", m.unitName, err)
		}
		after, err := m.queryState("is-enabled")
		if err != nil {
			return fmt.Errorf("cannot verify %s disabled after disable: %w", m.unitName, err)
		}
		if after == stateEnabled {
			return fmt.Errorf("%s still reports enabled after disable; not removing the unit", m.unitName)
		}
	} else if enabled == stateDisabled {
		fmt.Fprintf(out, "note: %s is not enabled; nothing to disable\n", m.unitName)
	} else {
		return fmt.Errorf("%s enablement is %q; cannot confirm it is disabled before uninstall", m.unitName, stateName(enabled))
	}
	if err := os.Remove(m.unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := m.systemctlSuccess("daemon-reload"); err != nil {
		return fmt.Errorf("reloading systemd after removing %s: %w", m.unitName, err)
	}
	fmt.Fprintf(out, "Removed %s. Application configuration, conversations and data were preserved.\n", m.unitName)
	return nil
}

func runService(args []string, version string) int {
	cmd := "status"
	rest := args
	for i, a := range args {
		if a != "" && !strings.HasPrefix(a, "-") {
			cmd = a
			rest = append(append([]string{}, args[:i]...), args[i+1:]...)
			break
		}
	}
	fs := flag.NewFlagSet("cortex service "+cmd, flag.ContinueOnError)
	system := fs.Bool("system", false, "install a system-wide unit (not yet supported; user mode is the default)")
	follow := fs.Bool("follow", false, "follow new journal output")
	listen := fs.String("listen", defaultListen, "HTTP listen address recorded in the unit")
	root := fs.String("root", "", "workspace root recorded in the unit")
	data := fs.String("data", "", "data directory recorded in the unit")
	trustProxy := fs.Bool("trust-proxy", false, "trust forwarding headers from a direct loopback reverse proxy")
	publicOrigin := fs.String("public-origin", "", "canonical external origin recorded in the unit")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if *system {
		fmt.Fprintln(os.Stderr, "cortex: system-wide service mode is not yet supported; use user mode (default) or the foreground command")
		return 2
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintln(os.Stderr, "cortex: systemctl not found; is systemd installed?")
		return 1
	}
	m := &serviceManager{
		unitName: "cortex.service",
		unitPath: userUnitPath("cortex.service"),
		run:      execRunner{},
	}
	switch cmd {
	case "install":
		if *root == "" {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				*root = home
			}
		}
		if *data == "" {
			if d, err := os.UserConfigDir(); err == nil {
				*data = filepath.Join(d, "cortex")
			}
		}
		if !filepath.IsAbs(*root) {
			fmt.Fprintln(os.Stderr, "cortex: --root must be an absolute path")
			return 2
		}
		if !filepath.IsAbs(*data) {
			fmt.Fprintln(os.Stderr, "cortex: --data must be an absolute path")
			return 2
		}
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		exe, err = resolveExecutable(exe)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		m.exe = exe
		for _, v := range []struct{ val, name string }{
			{*listen, "listen"}, {*root, "root"}, {*data, "data"}, {*publicOrigin, "public-origin"},
		} {
			if err := validateNoControl(v.val, v.name); err != nil {
				fmt.Fprintln(os.Stderr, "cortex:", err)
				return 2
			}
		}
		ghDir := hostGHConfigDir()
		if err := validateNoControl(ghDir, "gh config dir"); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 2
		}
		opts := serviceOptions{listen: *listen, root: *root, data: *data, publicOrigin: *publicOrigin, trustProxy: *trustProxy, ghDir: ghDir}
		if err := m.install(opts, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		return 0
	case "start", "stop", "restart":
		if err := m.action(cmd, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		return 0
	case "status":
		if err := m.status(os.Stdout, version); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		return 0
	case "logs":
		if err := m.logs(*follow, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		return 0
	case "uninstall":
		if err := m.uninstall(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "cortex:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "cortex: unknown service command %q\n\nUsage: cortex service <install|start|stop|restart|status|logs|uninstall> [flags]\n", cmd)
		return 2
	}
}