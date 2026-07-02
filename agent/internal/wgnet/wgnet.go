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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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

// laptopIPFile is where we publish the currently-authorized laptop overlay
// IP(s) — one bare IP per line, world-readable on tmpfs — so the unprivileged
// in-VM `devbox run` broker client can discover where to dial WITHOUT `wg show`
// (which needs CAP_NET_ADMIN and so fails for the `dev` run user). The cluster
// owns the peer set and the agent is the only root component that sees it, so it
// exposes the set here on every Reconcile. Kept in sync with the CLI's
// broker.LaptopIPFile (separate Go module — can't share the constant). /run is
// tmpfs, so this never persists to the workspace volume.
const laptopIPFile = "/run/devbox/laptop-wg-ips"

// livePeerWindow is how recently a peer must have completed a WireGuard handshake
// to count as a LIVE connection for the broker discovery file. A peer that handshook
// longer ago than this — i.e. a `devbox connect` whose laptop went away — is a
// strand and is dropped from the file (though it stays authorized in wg0 for its
// lease). The window must exceed WG's rekey (~120s) and the CLI's ssh keepalive
// cadence (oss/cli/internal/session: probe every 15s) so a live-but-idle
// connection is never falsely pruned; persistent-keepalive (25s) keeps a live
// peer's handshake fresh well inside it, while a vanished laptop ages past it.
const livePeerWindow = 3 * time.Minute

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
	laptopIPPath   string           // where the broker discovery file is written; overridable in tests
	now            func() time.Time // injectable clock for liveness; defaults time.Now
	mu             sync.Mutex       // serializes the file write (Reconcile + heartbeat tick both publish)
}

func New(run Runner, privateKeyPath string) *Net {
	return &Net{run: run, privateKeyPath: privateKeyPath, laptopIPPath: laptopIPFile, now: time.Now}
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
	// Publish the LIVE laptop IP(s) for the unprivileged broker client (see
	// laptopIPFile). Done AFTER the peer set is applied so the wg0 dump reflects
	// the desired peers and the file never advertises a laptop that isn't yet
	// routable. A freshly-added peer (handshake not yet completed) is published
	// immediately by the grace rule, so a fresh connect has no gap.
	return n.PublishLiveLaptopIPs()
}

// wgPeer is the subset of a `wg show <iface> dump` peer line we care about.
type wgPeer struct {
	pubkey        string
	allowedIP     string // the laptop's overlay IP (the /128 stripped); "" if none
	lastHandshake int64  // unix seconds; 0 = never handshaked
}

// parseWgDump parses `wg show <iface> dump`. The first line describes the
// interface (privkey, pubkey, listen-port, fwmark) and is skipped; each remaining
// line is a peer: pubkey, psk, endpoint, allowed-ips, latest-handshake (unix secs,
// 0=never), rx, tx, keepalive. We keep the pubkey, the first allowed-ip (the laptop
// /128 with its prefix stripped; "(none)" → ""), and the handshake.
func parseWgDump(out string) []wgPeer {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var peers []wgPeer
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" { // interface line / blanks
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		ip := ""
		if ai := strings.TrimSpace(f[3]); ai != "" && ai != "(none)" {
			// allowed-ips may be a comma-separated list; the laptop peer has one /128.
			first := strings.SplitN(ai, ",", 2)[0]
			ip = strings.SplitN(strings.TrimSpace(first), "/", 2)[0]
		}
		hs, _ := strconv.ParseInt(strings.TrimSpace(f[4]), 10, 64)
		peers = append(peers, wgPeer{pubkey: strings.TrimSpace(f[0]), allowedIP: ip, lastHandshake: hs})
	}
	return peers
}

// PublishLiveLaptopIPs rewrites the broker discovery file with the overlay IPs of
// only the peers whose tunnel is LIVE, so a stale strand left by a closed `devbox
// connect` no longer counts toward the broker's "exactly one connection" rule.
// Derived purely from wg0 (`wg show <iface> dump`), so it needs no cached control-
// plane state and is safe to call both from Reconcile and from the heartbeat tick.
// The wg peer SET is untouched (every authorized peer stays routable for its
// lease); only the file the unprivileged `devbox run` broker client reads is
// narrowed.
//
// A peer is live iff its handshake is 0 (never handshaked yet — a peer just added
// by Reconcile, still bringing its tunnel up; published immediately so a fresh
// connect has no gap) OR within livePeerWindow. A non-zero handshake older than the
// window is a peer that WAS live and went quiet — the strand — and is dropped. An
// empty result writes an empty file (no live connection → `devbox run` reports the
// session is down).
func (n *Net) PublishLiveLaptopIPs() error {
	out, err := n.run("wg", "show", iface, "dump")
	if err != nil {
		return err
	}
	now := n.now().Unix()
	staleAfter := int64(livePeerWindow / time.Second)
	var live []string
	for _, p := range parseWgDump(out) {
		if p.allowedIP == "" {
			continue
		}
		if p.lastHandshake != 0 && now-p.lastHandshake > staleAfter {
			continue // stale: was live, went quiet → strand
		}
		live = append(live, p.allowedIP)
	}
	return n.writeLaptopIPs(live)
}

// writeLaptopIPs atomically writes the given overlay IPs (one bare IP per line) to
// n.laptopIPPath, world-readable so the unprivileged `devbox run` broker client can
// read it. The mutex serializes the two concurrent callers (Reconcile via pullLoop,
// and the heartbeat tick); a UNIQUE temp avoids a shared-temp clobber under that
// concurrency. An empty list writes an empty file.
func (n *Net) writeLaptopIPs(ips []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	var b strings.Builder
	for _, ip := range ips {
		b.WriteString(ip)
		b.WriteByte('\n')
	}
	dir := filepath.Dir(n.laptopIPPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", n.laptopIPPath, err)
	}
	f, err := os.CreateTemp(dir, "laptop-wg-ips.*")
	if err != nil {
		return fmt.Errorf("temp for %s: %w", n.laptopIPPath, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	// CreateTemp makes the file 0600; the broker reads it as the unprivileged `dev`
	// user, so it must be world-readable.
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, n.laptopIPPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, n.laptopIPPath, err)
	}
	return nil
}
