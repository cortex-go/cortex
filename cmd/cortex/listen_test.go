package main

import (
	"flag"
	"os"
	"testing"
)

// listenerFlags parses raw argv through the same flag definitions the CLI uses
// so "provided but empty" (--host "") is distinguishable from "not provided".
func listenerFlags(args ...string) (h, p, l string, hs, ps, ls bool) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("host", "", "")
	fs.String("port", "", "")
	fs.String("listen", "", "")
	_ = fs.Parse(args)
	return fs.Lookup("host").Value.String(),
		fs.Lookup("port").Value.String(),
		fs.Lookup("listen").Value.String(),
		flagProvided(fs, "host"),
		flagProvided(fs, "port"),
		flagProvided(fs, "listen")
}

func TestResolveListenerDefaults(t *testing.T) {
	// Simulate an untouched shell where CORTEX_HOST/CORTEX_PORT are absent.
	os.Unsetenv("CORTEX_HOST")
	os.Unsetenv("CORTEX_PORT")
	h, p, l, hs, ps, ls := listenerFlags()
	addr, err := resolveListener(h, p, l, hs, ps, ls)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:7331" {
		t.Fatalf("default listener = %q want 127.0.0.1:7331", addr)
	}
}

func TestResolveListenerEnvOnly(t *testing.T) {
	t.Setenv("CORTEX_HOST", "0.0.0.0")
	t.Setenv("CORTEX_PORT", "7401")
	h, p, l, hs, ps, ls := listenerFlags()
	addr, err := resolveListener(h, p, l, hs, ps, ls)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "0.0.0.0:7401" {
		t.Fatalf("env listener = %q want 0.0.0.0:7401", addr)
	}
}

func TestResolveListenerCLIOnly(t *testing.T) {
	t.Setenv("CORTEX_HOST", "192.0.2.1")
	t.Setenv("CORTEX_PORT", "9999")
	h, p, l, hs, ps, ls := listenerFlags("--host", "127.0.0.1", "--port", "7401")
	addr, err := resolveListener(h, p, l, hs, ps, ls)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:7401" {
		t.Fatalf("cli listener = %q want 127.0.0.1:7401", addr)
	}
}

func TestResolveListenerCLIOverridesEnv(t *testing.T) {
	t.Setenv("CORTEX_HOST", "0.0.0.0")
	t.Setenv("CORTEX_PORT", "9000")
	h, p, l, hs, ps, ls := listenerFlags("--host", "127.0.0.1", "--port", "7401")
	addr, err := resolveListener(h, p, l, hs, ps, ls)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:7401" {
		t.Fatalf("cli should override env: %q", addr)
	}
}

func TestResolveListenerInvalidPorts(t *testing.T) {
	for _, p := range []string{"abc", "0", "-5", "65536", "70000", "7 4 0 1", "7401x"} {
		h, pp, l, hs, ps, ls := listenerFlags("--host", "127.0.0.1", "--port", p)
		if _, err := resolveListener(h, pp, l, hs, ps, ls); err == nil {
			t.Fatalf("invalid --port %q accepted", p)
		}
	}
	// Invalid CORTEX_PORT must not silently fall back to 7331.
	for _, p := range []string{"abc", "0", "-5", "65536", "70000"} {
		t.Setenv("CORTEX_HOST", "127.0.0.1")
		t.Setenv("CORTEX_PORT", p)
		h, pp, l, hs, ps, ls := listenerFlags()
		if _, err := resolveListener(h, pp, l, hs, ps, ls); err == nil {
			t.Fatalf("invalid CORTEX_PORT %q accepted", p)
		}
	}
}

func TestResolveListenerEmptyEnvFails(t *testing.T) {
	t.Setenv("CORTEX_HOST", "127.0.0.1")
	t.Setenv("CORTEX_PORT", "")
	h, p, l, hs, ps, ls := listenerFlags()
	if _, err := resolveListener(h, p, l, hs, ps, ls); err == nil {
		t.Fatal("empty CORTEX_PORT accepted")
	}
	t.Setenv("CORTEX_HOST", "")
	t.Setenv("CORTEX_PORT", "7331")
	if _, err := resolveListener(h, p, l, hs, ps, ls); err == nil {
		t.Fatal("empty CORTEX_HOST accepted")
	}
}

func TestResolveListenerEmptyCLIValuesFail(t *testing.T) {
	t.Setenv("CORTEX_HOST", "127.0.0.1")
	t.Setenv("CORTEX_PORT", "7331")
	h, p, l, hs, ps, ls := listenerFlags("--host", "", "--port", "7331")
	if _, err := resolveListener(h, p, l, hs, ps, ls); err == nil {
		t.Fatal("empty --host accepted")
	}
	t.Setenv("CORTEX_HOST", "127.0.0.1")
	t.Setenv("CORTEX_PORT", "7331")
	h, p, l, hs, ps, ls = listenerFlags("--host", "127.0.0.1", "--port", "")
	if _, err := resolveListener(h, p, l, hs, ps, ls); err == nil {
		t.Fatal("empty --port accepted")
	}
}

func TestResolveListenerIPv6(t *testing.T) {
	t.Setenv("CORTEX_HOST", "::1")
	t.Setenv("CORTEX_PORT", "7331")
	h, p, l, hs, ps, ls := listenerFlags()
	addr, err := resolveListener(h, p, l, hs, ps, ls)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "[::1]:7331" {
		t.Fatalf("IPv6 listener = %q want [::1]:7331", addr)
	}
}

func TestResolveListenerLegacyListen(t *testing.T) {
	t.Setenv("CORTEX_HOST", "0.0.0.0")
	t.Setenv("CORTEX_PORT", "9000")
	h, p, l, hs, ps, ls := listenerFlags("--listen", "127.0.0.1:7331")
	addr, err := resolveListener(h, p, l, hs, ps, ls)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:7331" {
		t.Fatalf("legacy --listen = %q", addr)
	}
	h2, p2, l2, hs2, ps2, ls2 := listenerFlags("--host", "127.0.0.1", "--port", "7401", "--listen", "127.0.0.1:7331")
	if _, err := resolveListener(h2, p2, l2, hs2, ps2, ls2); err == nil {
		t.Fatal("--listen combined with --host/--port accepted")
	}
}

func TestValidatePort(t *testing.T) {
	for _, ok := range []string{"1", "7331", "65535"} {
		if err := validatePort(ok); err != nil {
			t.Fatalf("valid port %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "0", "-1", "65536", "7331.5", "x"} {
		if err := validatePort(bad); err == nil {
			t.Fatalf("invalid port %q accepted", bad)
		}
	}
}

func TestResolveListenerTrimsWhitespace(t *testing.T) {
	// Whitespace-surrounded CLI values must resolve canonically.
	h, p, l, hs, ps, ls := listenerFlags("--host", "  127.0.0.1  ", "--port", "  7401  ")
	host, port, err := resolveHostPort(h, p, hs, ps)
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" || port != "7401" {
		t.Fatalf("cli trimmed host/port = %q/%q want 127.0.0.1/7401", host, port)
	}
	addr, err := resolveListener(h, p, l, hs, ps, ls)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:7401" {
		t.Fatalf("cli listener = %q want 127.0.0.1:7401", addr)
	}

	// Whitespace-surrounded environment values must resolve canonically.
	t.Setenv("CORTEX_HOST", "  0.0.0.0  ")
	t.Setenv("CORTEX_PORT", "  7402  ")
	h, p, l, hs, ps, ls = listenerFlags()
	host, port, err = resolveHostPort(h, p, hs, ps)
	if err != nil {
		t.Fatal(err)
	}
	if host != "0.0.0.0" || port != "7402" {
		t.Fatalf("env trimmed host/port = %q/%q want 0.0.0.0/7402", host, port)
	}
	addr, err = resolveListener(h, p, l, hs, ps, ls)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "0.0.0.0:7402" {
		t.Fatalf("env listener = %q want 0.0.0.0:7402", addr)
	}
}

func TestResolveListenerWhitespaceOnlyFails(t *testing.T) {
	// Whitespace-only values are empty after trimming and must fail.
	h, p, l, hs, ps, ls := listenerFlags("--host", "   ", "--port", "   ")
	if _, err := resolveListener(h, p, l, hs, ps, ls); err == nil {
		t.Fatal("whitespace-only --host/--port accepted")
	}
	t.Setenv("CORTEX_HOST", "   ")
	t.Setenv("CORTEX_PORT", "   ")
	h, p, l, hs, ps, ls = listenerFlags()
	if _, err := resolveListener(h, p, l, hs, ps, ls); err == nil {
		t.Fatal("whitespace-only CORTEX_HOST/CORTEX_PORT accepted")
	}
	// Whitespace-only host with a valid port must still fail.
	t.Setenv("CORTEX_HOST", "   ")
	t.Setenv("CORTEX_PORT", "7401")
	h, p, l, hs, ps, ls = listenerFlags()
	if _, err := resolveListener(h, p, l, hs, ps, ls); err == nil {
		t.Fatal("whitespace-only host accepted with valid port")
	}
}
