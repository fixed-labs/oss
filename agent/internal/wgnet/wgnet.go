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
	"net/netip"
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
	// dumpEndpointField is the column index of a peer's CURRENT endpoint in a
	// `wg show wg0 dump` line. The dump is tab-separated and its peer lines are
	//
	//	pubkey  psk  endpoint  allowed-ips  latest-handshake  rx  tx  keepalive
	//
	// The format's one trap: the FIRST line of the dump describes the interface
	// itself (private-key, public-key, listen-port, fwmark) and is NOT a peer —
	// parse it as one and wg0's own public key becomes a phantom peer that the
	// removal diff then tries to `wg set … remove`.
	//
	// We read the dump rather than `wg show wg0 peers` because `peers` prints
	// only that first column, and Reconcile needs the endpoint (see there).
	dumpEndpointField = 2
	// dumpNone is what the dump prints for an unset optional field. In the
	// endpoint column it means the peer has no endpoint at all — the kernel has
	// nowhere to send to, and won't until one is configured or learned from an
	// authenticated inbound packet.
	dumpNone = "(none)"
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

// peerState is one wg0 peer as the kernel currently holds it.
type peerState struct {
	pubkey string
	// endpoint is the peer's live transport address exactly as the dump printed
	// it, or "" when the peer has none. This is TEXT and must never be compared
	// with ==: the kernel prints an IPv6 address in its own canonical form,
	// which can differ in case and zero-compression from the text we handed to
	// `wg set` for that very same address. Compare with endpointDiffers.
	endpoint string
}

// currentPeerStates reads wg0's live peer set with `wg show wg0 dump`. One read
// serves both halves of a reconcile — the removal diff (pubkeys) and the
// endpoint-change check (endpoints) — which is why Reconcile no longer runs a
// separate `wg show wg0 peers`.
func (n *Net) currentPeerStates() ([]peerState, error) {
	out, err := n.run("wg", "show", iface, "dump")
	if err != nil {
		return nil, err
	}
	var peers []peerState
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		// Line 0 is the interface line, not a peer (see dumpEndpointField).
		if i == 0 {
			continue
		}
		// Fields, not Split("\t"): the real separator is a tab, but no field
		// here (a base64 key, an ip:port, comma-joined allowed-ips, integers)
		// can contain whitespace, so tolerating either separator costs nothing
		// and keeps hand-written test fixtures honest either way.
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		st := peerState{pubkey: f[0]}
		// A line too short to carry an endpoint column leaves endpoint "" — the
		// conservative reading in both directions: the peer still participates
		// in the removal diff (the security-critical half must not silently drop
		// peers it failed to parse), and it compares as CHANGED against any
		// desired endpoint, so the worst case is one extra remove+re-add rather
		// than a peer we never freshen.
		if len(f) > dumpEndpointField && f[dumpEndpointField] != dumpNone {
			st.endpoint = f[dumpEndpointField]
		}
		peers = append(peers, st)
	}
	return peers, nil
}

// endpointDiffers reports whether a peer's live endpoint (`current`, as the
// dump printed it) names a different transport address than the one we are
// about to configure (`desired`, as net.JoinHostPort produced it). It is the
// sole trigger for Reconcile's remove-and-re-add, and it is wrong in both
// directions at a cost:
//
//   - a false POSITIVE tears down a LIVE session, and does it on the
//     steady-state path — every successful pull, forever. This is why the
//     comparison is structural (netip.ParseAddrPort) rather than a string
//     compare: `[fdaa:0:7e:a9d3:a7b:849:5245:0002]:49153` and
//     `[FDAA:0:7E:A9D3:A7B:849:5245:2]:49153` are the same endpoint, and only
//     one of those two spellings came from us.
//   - a false NEGATIVE leaves the box silent on a real endpoint change until
//     WireGuard's own handshake retransmit ladder next fires (measured period
//     5.376s) — the very delay Reconcile exists to remove.
//
// Two deliberate "not different" answers, both chosen because the alternative
// churns a live peer on every single pull with no way to ever converge:
//
//   - desired == "" — the config carries no endpoint for this peer (see
//     Reconcile). We have no opinion to compare against, and `wg set` has no
//     argument that CLEARS an endpoint, so treating this as a difference would
//     remove and re-add the peer forever, each time discarding whatever
//     endpoint the kernel had learned from the peer's own traffic. An absent
//     desired endpoint means "leave it alone", never "it changed".
//   - desired does not parse as a numeric ip:port. The control plane sends the
//     relay's 6PN literal, so this should not occur; if it ever did (a
//     hostname), `wg` would resolve it at set time and the dump would print the
//     RESOLVED literal, which can never match the name textually. We would
//     rather lose the fast-transmit optimisation for a form we don't emit than
//     churn on it.
//
// An unparseable CURRENT endpoint, by contrast, IS a difference: "(none)" (a
// peer with no endpoint yet — the "add an endpoint to an existing peer" case,
// measured at +4.05s to first packet) reads as such, and anything else we
// cannot parse we would rather re-add than leave pointing somewhere unknown.
//
// One caveat worth stating, because it is a property of the RELAY rather than of
// this file: the comparison assumes the live endpoint stays equal to the one we
// configured. WireGuard roams — the kernel rewrites peer->endpoint to the source
// of any authenticated packet — so if the box ever received relayed traffic from
// an address other than the configured one, every steady-state pull would see a
// difference and churn a live peer. It does not today because bounce.bridgeLoop
// writes from internalConn, bound to the relay's internal address on the same
// port the box was told to dial, so observed source == configured endpoint. That
// holds for the dual-socket mode Fly uses; a single-socket relay (bounce.Open
// with internalBindIP nil) would not obviously preserve it, so if that mode is
// ever put in front of a real box, re-check this assumption first.
func endpointDiffers(current, desired string) bool {
	if desired == "" {
		return false
	}
	want, err := netip.ParseAddrPort(desired)
	if err != nil {
		return false
	}
	have, err := netip.ParseAddrPort(current)
	if err != nil {
		return true
	}
	// Unmap so ::ffff:1.2.3.4 and 1.2.3.4 — the same peer — compare equal;
	// parsing has already collapsed case and zero-compression.
	return have.Addr().Unmap() != want.Addr().Unmap() || have.Port() != want.Port()
}

// Removals is the pure diff: which CURRENT peer pubkeys are absent from the
// DESIRED set. (Desired peers are re-applied wholesale by Reconcile — `wg set`
// is idempotent and re-applying catches allowed-ip and lease changes — so only
// removals need computing here. The one change Reconcile does NOT express as a
// plain re-apply is an endpoint change; see there.)
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
//
// A desired peer whose endpoint CHANGED is removed and then re-added, rather
// than `wg set`-over. That is not cosmetic. Kernel WireGuard's set_peer() sends
// a keepalive — the packet that makes the box TRANSMIT, and so triggers a
// handshake initiation when there is no session — only when a peer's
// persistent_keepalive_interval transitions 0 → N, i.e. only when the peer is
// brand new. Every other `wg set` is silent, and REKEY_TIMEOUT (~5s)
// rate-limits initiations regardless, so no keepalive toggle substitutes:
// only making the peer FRESH resets last_sent_handshake. Measured on a real box
// (time from the command to the first packet on the wire):
//
//	fresh peer with endpoint             +4.0 ms
//	re-apply the identical `wg set`      nothing at all
//	add an endpoint to an EXISTING peer  +4.05 s   <- the ladder, not the command
//	change the endpoint port             +2.79 s   <- likewise
//	remove + re-add                      +6.7 ms
//
// The multi-second figures are not emissions caused by the `wg set`; they are
// WireGuard's own handshake retransmit ladder (measured period 5.376s) firing
// and happening to find a usable endpoint. Whenever it is reached, that wait is
// on the connect critical path, because the relay's bouncer cannot forward
// laptop→box until the box has transmitted at least once (bounce.go drops, with
// no queue, while its internal side is unlearned).
//
// Be precise about when that is, though: this is NOT the ordinary warm-claim
// path. The laptop generates a new keypair per connect, so an ordinary connect
// hands Reconcile a peer that does not exist yet — the fresh-peer row above, ~4ms,
// already fast. Measured end to end, on a resumed box the BOX's own first
// handshake initiation was answered in 23-30ms, in 4/4 controlled runs — the
// direction matters here, since the bouncer cannot carry laptop→box traffic until
// the box has transmitted. What this change fixes is the paths where
// a peer already exists and its endpoint moves under it: relay reassignment and
// endpoint churn. Claiming it buys seconds off a normal connect would be wrong.
//
// A peer whose endpoint is UNCHANGED must NOT be re-added. That is the
// steady-state path — it runs on every successful pull — and removing the peer
// there would drop a live session and force a fresh handshake every time the
// config loop goes round. endpointDiffers is what keeps the two apart, and is
// deliberately biased toward "unchanged" in the cases it cannot decide.
func (n *Net) Reconcile(desired []api.Peer) error {
	// One read of the live peer set, taken BEFORE any mutation, serves both
	// halves below. Taking it up front — the removal diff used to read after the
	// sets — is what makes the endpoint comparison possible at all, and it
	// cannot change the removal outcome: the loop below only ever touches
	// DESIRED peers, and Removals is current-minus-desired, so no peer this loop
	// creates (or removes and re-adds) can appear in it.
	current, err := n.currentPeerStates()
	if err != nil {
		return err
	}
	live := make(map[string]peerState, len(current))
	currentKeys := make([]string, 0, len(current))
	for _, st := range current {
		live[st.pubkey] = st
		currentKeys = append(currentKeys, st.pubkey)
	}

	// firstErr is remembered rather than returned on the spot, because with the
	// remove-then-re-add below a mid-loop `return` became DESTRUCTIVE: it can
	// leave a peer that was working before this call removed from wg0 entirely,
	// and it skips the removals pass, so a revoked laptop stays authorized.
	// Neither is an acceptable response to a transient `wg` failure. So: keep
	// going, apply everything that can be applied, and report at the end. The
	// caller (supervisor.pullLoop) treats a non-nil return as "do not advance the
	// cursor", so the whole desired set is re-fetched and re-applied on the next
	// poll — that retry is what repairs a peer this pass left absent.
	// Two error slots, not one plus bookkeeping: a "wg0 is MISSING a peer" error
	// always outranks an ordinary one, because guard 1 rests on the caller seeing
	// it and the two want different operator responses. First of each kind wins.
	var firstErr, firstMissingErr error
	for _, p := range desired {
		endpoint := ""
		if p.RelayEndpoint != "" && p.RelayPort > 0 {
			// net.JoinHostPort brackets IPv6 hosts ([fdaa:…]:port) — the relay's
			// INTERNAL endpoint (what a co-located workspace dials) is a 6PN IPv6,
			// while the laptop side is IPv4; a bare "%s:%d" would mangle IPv6.
			endpoint = net.JoinHostPort(p.RelayEndpoint, strconv.Itoa(p.RelayPort))
		}
		removed := false
		// A desired peer with no wg-ip cannot produce a valid `wg set`
		// (allowed-ips would be a bare "/128"), so its re-add would fail EVERY
		// time, not transiently. Removing first would then delete a working peer
		// and never restore it — strictly worse than the pre-change behaviour,
		// which left it alone with its old endpoint. Skip the destructive path
		// for malformed input and let the plain `wg set` below fail loudly.
		reAddable := p.LaptopWgIP != ""
		if st, exists := live[p.LaptopWgPubkey]; exists && reAddable && endpointDiffers(st.endpoint, endpoint) {
			// Make the peer FRESH so the `wg set` immediately below emits a
			// keepalive instead of waiting on the ladder (see the doc comment).
			// The session this drops was pointed at an endpoint that, by
			// definition, is no longer the one the laptop is reachable through.
			if _, err := n.run("wg", "set", iface, "peer", p.LaptopWgPubkey, "remove"); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				// The remove failed, so the peer still holds its OLD endpoint.
				// Fall through and re-apply over it: that is the pre-existing
				// (silent, but non-destructive) behaviour, and it is strictly
				// better than leaving the stale endpoint in place.
			} else {
				removed = true
			}
		}
		args := []string{"set", iface, "peer", p.LaptopWgPubkey,
			"allowed-ips", p.LaptopWgIP + "/128",
			"persistent-keepalive", persistentKeepalive}
		if endpoint != "" {
			args = append(args, "endpoint", endpoint)
		}
		_, err := n.run("wg", args...)
		if err != nil && removed {
			// The remove SUCCEEDED and the re-add did not: the peer is gone from
			// wg0 right now, which is a state this function never used to be able
			// to produce. Retry once inline. A transient fork/exec or netlink
			// failure is by far the likeliest cause, and one immediate retry
			// resolves it without waiting for the caller's next poll — during
			// which the laptop cannot reach the box at all.
			_, err = n.run("wg", args...)
		}
		if err != nil {
			if removed {
				// Say WHICH failure this is, distinctly. "a wg set failed" is the
				// benign case — the peer keeps its previous configuration. This one
				// means wg0 is actually MISSING an authorized peer until the caller
				// re-applies, so the laptop cannot reach the box at all. The caller
				// logs whatever we return, so the distinction has to live here.
				if firstMissingErr == nil {
					firstMissingErr = fmt.Errorf("peer %s was removed for an endpoint change "+
						"and could not be re-added; wg0 is MISSING this peer until the next "+
						"reconcile: %w", p.LaptopWgPubkey, err)
				}
			} else if firstErr == nil {
				firstErr = err
			}
		}
	}

	// Removals are the security-critical half — a peer left behind keeps a
	// revoked laptop authorized — so they run even when a set above failed.
	for _, pk := range Removals(currentKeys, desired) {
		if _, err := n.run("wg", "set", iface, "peer", pk, "remove"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstMissingErr != nil {
		return firstMissingErr
	}
	return firstErr
}
