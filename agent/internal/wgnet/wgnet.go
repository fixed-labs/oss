// Package wgnet owns wg0: bring-up at boot and reconciliation of the
// authorized-peer set from the pulled agent config. The cluster owns
// addressing (the deterministic ULA wg-ip arrives via env; each laptop's
// /128 arrives per-peer); the VM owns only its keypair.
//
// Everything shells out to iproute2 + wireguard-tools (both part of the
// base-image contract, which also provides the WireGuard kernel module). The
// runner is injectable for tests.
package wgnet

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"

	"github.com/fixed-labs/oss/agent/internal/api"
)

const (
	iface = "wg0"
	// ListenPort is wg0's UDP port. The workspace dials OUT to its relay (the
	// per-pairing relay port is the rendezvous), so this is not exposed.
	ListenPort = 51820
	// persistentKeepalive keeps the workspace→relay path warm so the relay's
	// bouncer learns (and re-learns, after NAT/host changes) the workspace's
	// source address from a steady packet flow — the workspace side must
	// initiate, since the bouncer only ever forwards between two LEARNED
	// addresses.
	persistentKeepalive = "25"
	// overlayRoute is the overlay ULA block (the wg-ula-prefix "fd5e:de7b", a
	// /32). EVERY peer wg-ip — every
	// laptop AND every workspace — is a deterministic /128 inside it, and a
	// laptop's /128 sits in a different /64 than this box's own /128. We install
	// this as a kernel ROUTE via wg0 at bring-up so the kernel sends RETURN
	// traffic (e.g. the SSH SYN-ACK) to a laptop's /128 OUT THE TUNNEL. `wg set …
	// allowed-ips` is wg cryptokey routing only, NOT a kernel route, and we don't
	// use wg-quick (which would install allowed-ips as routes) — so without this
	// the box has no route to the laptop's /128, the SYN-ACK is misrouted out the
	// 6PN default (eth0) and dropped, and `devbox connect` hangs at "Opening
	// shell" with the box stuck in TCP SYN-RECV. (Literal, not imported — the
	// agent is a standalone Go binary that shares no constants with the
	// control plane.)
	overlayRoute = "fd5e:de7b::/32"
	// overlayMTU caps wg0 below the underlay so encapsulated frames fit. The box
	// reaches its relay over Fly's 6PN (eth0 MTU 1420); WireGuard over IPv6 adds
	// 80 bytes (IPv6 40 + UDP 8 + wg 32), so a 1420-byte inner packet becomes a
	// 1500-byte wire frame that eth0 CANNOT egress — large frames (e.g. the SSH
	// KEXINIT, ~1.4 KB) are silently dropped while the TCP handshake (small)
	// succeeds, so `devbox connect` reaches ESTAB then hangs in KEX. 1280 (the
	// IPv6 minimum link MTU) clears the 1420-80=1340 ceiling with margin for the
	// relay's IPv4 laptop leg. The CLI's userspace tunnel uses the same value
	// (oss/cli/internal/tunnel/netstack.go).
	overlayMTU = "1280"
)

// Runner executes a command, returning combined output on error. Injectable
// for tests; exec in production.
type Runner func(name string, args ...string) (string, error)

func ExecRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, string(out))
	}
	return string(out), nil
}

type Net struct {
	run            Runner
	privateKeyPath string
}

func New(run Runner, privateKeyPath string) *Net {
	return &Net{run: run, privateKeyPath: privateKeyPath}
}

// Up creates + configures + raises wg0 (idempotent: link-exists errors are
// tolerated; addr replace and wg set re-apply cleanly).
func (n *Net) Up(wgIP string) error {
	if out, err := n.run("ip", "link", "add", iface, "type", "wireguard"); err != nil {
		if !strings.Contains(out, "File exists") && !strings.Contains(err.Error(), "File exists") {
			return err
		}
	}
	if _, err := n.run("wg", "set", iface,
		"listen-port", fmt.Sprintf("%d", ListenPort),
		"private-key", n.privateKeyPath); err != nil {
		return err
	}
	if _, err := n.run("ip", "-6", "addr", "replace", wgIP+"/128", "dev", iface); err != nil {
		return err
	}
	// Cap the MTU below the underlay (see overlayMTU) BEFORE bringing the link
	// up, so the first packets already use the safe size.
	if _, err := n.run("ip", "link", "set", iface, "mtu", overlayMTU); err != nil {
		return err
	}
	if _, err := n.run("ip", "link", "set", iface, "up"); err != nil {
		return err
	}
	// Route the whole overlay /32 into wg0 (see overlayRoute) so the kernel
	// routes RETURN traffic to peer wg-ips out the tunnel instead of the 6PN
	// default. `replace` is idempotent (Up re-runs on every boot).
	if _, err := n.run("ip", "-6", "route", "replace", overlayRoute, "dev", iface); err != nil {
		return err
	}
	return nil
}

// CurrentPeers lists wg0's configured peer public keys (`wg show wg0 peers`).
func (n *Net) CurrentPeers() ([]string, error) {
	out, err := n.run("wg", "show", iface, "peers")
	if err != nil {
		return nil, err
	}
	var peers []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			peers = append(peers, l)
		}
	}
	return peers, nil
}

// Removals is the pure diff: which CURRENT peer pubkeys are absent from the
// DESIRED set. (Desired peers are always re-applied wholesale — `wg set` is
// idempotent and re-applying catches endpoint/allowed-ip/lease changes — so
// only removals need computing.)
func Removals(current []string, desired []api.Peer) []string {
	want := make(map[string]bool, len(desired))
	for _, p := range desired {
		want[p.LaptopWgPubkey] = true
	}
	var gone []string
	for _, c := range current {
		if !want[c] {
			gone = append(gone, c)
		}
	}
	return gone
}

// Reconcile applies the pulled desired peer set to wg0: every desired peer is
// (re-)set — endpoint = its pairing's relay transport, allowed-ips = the
// laptop's /128 (cryptokey routing IS the authorization) —
// and every no-longer-desired peer is removed (a stale peer would keep a
// revoked laptop authorized — the security-critical half).
func (n *Net) Reconcile(desired []api.Peer) error {
	for _, p := range desired {
		args := []string{"set", iface, "peer", p.LaptopWgPubkey,
			"allowed-ips", p.LaptopWgIP + "/128",
			"persistent-keepalive", persistentKeepalive}
		if p.RelayEndpoint != "" && p.RelayPort > 0 {
			// net.JoinHostPort brackets IPv6 hosts ([fdaa:…]:port) — the relay's
			// INTERNAL endpoint (what a co-located workspace dials) is a 6PN IPv6,
			// while the laptop side is IPv4; a bare "%s:%d" would mangle IPv6.
			args = append(args, "endpoint", net.JoinHostPort(p.RelayEndpoint, strconv.Itoa(p.RelayPort)))
		}
		if _, err := n.run("wg", args...); err != nil {
			return err
		}
	}
	current, err := n.CurrentPeers()
	if err != nil {
		return err
	}
	for _, pk := range Removals(current, desired) {
		if _, err := n.run("wg", "set", iface, "peer", pk, "remove"); err != nil {
			return err
		}
	}
	return nil
}
