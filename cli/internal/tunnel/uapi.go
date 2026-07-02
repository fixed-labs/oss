// Package tunnel brings up the laptop end of the E2E WireGuard session in
// USERSPACE — wireguard-go in netstack mode (an in-process gVisor TCP/IP
// stack, like Tailscale's tsnet): no TUN device, no root, no installed
// helper. The tunnel lives entirely inside the process; the
// interactive session dials a Go SSH client directly over this stack
// (DialContext), and the pre-shell secrets reconcile uses the external ssh
// binary against a localhost bridge over the same stack.
//
// This file is the PURE part: assembling the wireguard-go UAPI config string
// from an attach bundle. It's split out so the config logic is unit-tested
// without pulling the wireguard-go device up (which needs a real peer).
package tunnel

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Params is everything needed to configure the laptop's wg0-equivalent for a
// single (laptop, workspace) pairing — derived from the CLI's own keypair +
// the api's AttachBundle.
type Params struct {
	// LaptopPrivateKey is base64 (wg format) — generated per session, never
	// leaves the process.
	LaptopPrivateKey string
	// WorkspacePublicKey is the box's wg pubkey (base64), from the bundle.
	WorkspacePublicKey string
	// RelayEndpoint:RelayPort is where BOTH ends point their peer Endpoint —
	// the per-pairing relay transport (the relay bounces ciphertext).
	RelayEndpoint string
	RelayPort     int
	// WorkspaceWgIP is the box's overlay address — the only allowed-ip (a
	// /128; cryptokey routing).
	WorkspaceWgIP string
	// PersistentKeepalive keeps the laptop→relay path warm so the bouncer
	// learns the laptop's source address (mirrors the workspace side).
	PersistentKeepaliveSeconds int
}

// b64ToHex converts a base64 wg key to the hex wireguard-go UAPI wants.
func b64ToHex(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("decode wg key: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("wg key is %d bytes, want 32", len(raw))
	}
	return hex.EncodeToString(raw), nil
}

// UAPIConfig renders the wireguard-go IpcSet config string for this pairing.
// Format per wireguard-go's UAPI: newline-separated key=value, peer section
// introduced by public_key. allowed_ip is the workspace's /128 only — the
// laptop reaches the box and nothing else.
func (p Params) UAPIConfig() (string, error) {
	if p.RelayEndpoint == "" || p.RelayPort <= 0 {
		return "", fmt.Errorf("relay endpoint/port required")
	}
	if p.WorkspaceWgIP == "" {
		return "", fmt.Errorf("workspace wg ip required")
	}
	privHex, err := b64ToHex(p.LaptopPrivateKey)
	if err != nil {
		return "", fmt.Errorf("laptop key: %w", err)
	}
	pubHex, err := b64ToHex(p.WorkspacePublicKey)
	if err != nil {
		return "", fmt.Errorf("workspace key: %w", err)
	}
	keepalive := p.PersistentKeepaliveSeconds
	if keepalive == 0 {
		keepalive = 25
	}
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", privHex)
	fmt.Fprintf(&b, "public_key=%s\n", pubHex)
	fmt.Fprintf(&b, "endpoint=%s:%d\n", p.RelayEndpoint, p.RelayPort)
	// /128 — the workspace's overlay address, nothing else.
	fmt.Fprintf(&b, "allowed_ip=%s/128\n", p.WorkspaceWgIP)
	fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", keepalive)
	return b.String(), nil
}
