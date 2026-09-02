package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Default host and port for the shared Stackyard host/port standard. Cortex's
// default listener is 127.0.0.1:7331; both remain configurable.
const (
	defaultHost = "127.0.0.1"
	defaultPort = "7331"
)

// flagProvided reports whether name was explicitly supplied on the command
// line. Standard flag strings cannot otherwise distinguish `--port ""` from an
// absent flag, and the contract requires empty CLI values to fail.
func flagProvided(fs *flag.FlagSet, name string) bool {
	provided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			provided = true
		}
	})
	return provided
}

// validatePort reports whether p is a valid TCP port: an integer from 1
// through 65535. It never silently falls back to a default.
func validatePort(p string) error {
	n, err := strconv.Atoi(strings.TrimSpace(p))
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port must be an integer from 1 through 65535; got %q", p)
	}
	return nil
}

// resolveHostPort computes the effective bind host and port. Precedence per
// field is CLI flag > environment variable > default. A port that is present
// but empty, malformed, zero, negative or greater than 65535 is an error; an
// empty host value is rejected rather than silently meaning "all interfaces".
func resolveHostPort(hostFlag, portFlag string, hostSet, portSet bool) (host, port string, err error) {
	host = defaultHost
	if hostSet {
		if strings.TrimSpace(hostFlag) == "" {
			return "", "", errors.New("--host is set but empty")
		}
		host = hostFlag
	} else if v, ok := os.LookupEnv("CORTEX_HOST"); ok {
		if strings.TrimSpace(v) == "" {
			return "", "", errors.New("CORTEX_HOST is set but empty")
		}
		host = v
	}
	port = defaultPort
	if portSet {
		if err := validatePort(portFlag); err != nil {
			return "", "", fmt.Errorf("--port: %w", err)
		}
		port = portFlag
	} else if v, ok := os.LookupEnv("CORTEX_PORT"); ok {
		if err := validatePort(v); err != nil {
			return "", "", fmt.Errorf("CORTEX_PORT: %w", err)
		}
		port = v
	}
	return host, port, nil
}

// resolveListener computes the effective HTTP listen address from the CLI
// flags and environment. The legacy --listen single-address form is preserved
// for compatibility and cannot be combined with --host/--port. IPv6 hosts are
// bracketed via net.JoinHostPort.
func resolveListener(hostFlag, portFlag, listenFlag string, hostSet, portSet, listenSet bool) (string, error) {
	if listenSet {
		if hostSet || portSet {
			return "", errors.New("--listen cannot be combined with --host or --port")
		}
		if listenFlag == "" {
			return "", errors.New("--listen is set but empty")
		}
		return listenFlag, nil
	}
	host, port, err := resolveHostPort(hostFlag, portFlag, hostSet, portSet)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, port), nil
}
