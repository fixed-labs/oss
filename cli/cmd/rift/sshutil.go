package main

import (
	"fmt"
	"net"
	"os"
)

// splitHostPort splits "host:port", preserving a bracketed IPv6 host.
func splitHostPort(addr string) (host, port string, err error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", fmt.Errorf("bad bridge address %q: %w", addr, err)
	}
	return h, p, nil
}

// writeKnownHosts writes a single-entry known_hosts pinning host:port to the
// attach bundle's ssh host pubkey, so `ssh` trusts the box with no TOFU/CA.
// Returns the file path and a cleanup fn.
func writeKnownHosts(host, port, sshHostPubkey string) (string, func(), error) {
	f, err := os.CreateTemp("", "devbox-known-hosts-*")
	if err != nil {
		return "", func() {}, err
	}
	// Non-standard port → "[host]:port key" form.
	target := host
	if port != "22" {
		target = fmt.Sprintf("[%s]:%s", host, port)
	}
	if _, err := fmt.Fprintf(f, "%s %s\n", target, sshHostPubkey); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}
