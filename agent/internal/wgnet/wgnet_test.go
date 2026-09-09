package wgnet

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// recorder captures commands and replays canned outputs.
type recorder struct {
	calls   []string
	outputs map[string]string // command prefix → stdout
	fails   map[string]string // command prefix → error output (every time)
	// failOnce fails only the FIRST call matching each prefix, so a test can
	// distinguish a transient failure from a persistent one.
	failOnce map[string]string
	failed   map[string]bool
}

func (r *recorder) run(name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, cmd)
	for prefix, out := range r.fails {
		if strings.HasPrefix(cmd, prefix) {
			return out, fmt.Errorf("exit 2: %s", out)
		}
	}
	for prefix, out := range r.failOnce {
		if strings.HasPrefix(cmd, prefix) && !r.failed[prefix] {
			if r.failed == nil {
				r.failed = map[string]bool{}
			}
			r.failed[prefix] = true
			return out, fmt.Errorf("exit 2: %s", out)
		}
	}
	for prefix, out := range r.outputs {
		if strings.HasPrefix(cmd, prefix) {
			return out, nil
		}
	}
	return "", nil
}

// hasCall reports whether any recorded command contains `substr`. Used to
// assert the ABSENCE of a `wg set … remove` on the steady-state path — the one
// outcome this package must never produce by accident.
func (r *recorder) hasCall(substr string) bool {
	for _, c := range r.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// dumpPeer is one peer line's worth of a `wg show wg0 dump` fixture. An empty
// endpoint renders as the dump's "(none)".
type dumpPeer struct{ pubkey, endpoint string }

// wgDump renders a realistic `wg show wg0 dump` body: the INTERFACE line first
// (private-key, public-key, listen-port, fwmark — not a peer, and the one thing
// a parser of this format must skip), then one tab-separated line per peer:
//
//	pubkey  psk  endpoint  allowed-ips  latest-handshake  rx  tx  keepalive
func wgDump(peers ...dumpPeer) string {
	var b strings.Builder
	b.WriteString("PRIVKEY=\tIFACEPUBKEY=\t51820\toff\n")
	for _, p := range peers {
		endpoint := p.endpoint
		if endpoint == "" {
			endpoint = "(none)"
		}
		fmt.Fprintf(&b, "%s\t(none)\t%s\tfd5e:de7b::aa/128\t1754500000\t3184\t2960\t25\n",
			p.pubkey, endpoint)
	}
	return b.String()
}

// dumpOutput wires a dump fixture into the recorder as the answer to
// `wg show wg0 dump`.
func dumpOutput(peers ...dumpPeer) map[string]string {
	return map[string]string{"wg show wg0 dump": wgDump(peers...)}
}

func TestUpSequence(t *testing.T) {
	r := &recorder{}
	n := New(r.run, "/var/lib/devboxes/wg.key")
	if err := n.Up("fd5e:de7b::1"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	want := []string{
		"ip link add wg0 type wireguard",
		"wg set wg0 listen-port 51820 private-key /var/lib/devboxes/wg.key",
		"ip -6 addr replace fd5e:de7b::1/128 dev wg0",
		"ip link set wg0 mtu 1280",
		"ip link set wg0 up",
		"ip -6 route replace fd5e:de7b::/32 dev wg0",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
}

func TestUpToleratesExistingLink(t *testing.T) {
	r := &recorder{fails: map[string]string{"ip link add": "RTNETLINK answers: File exists"}}
	n := New(r.run, "/k")
	if err := n.Up("fd5e:de7b::1"); err != nil {
		t.Fatalf("Up should tolerate existing link: %v", err)
	}
}

func TestRemovalsDiff(t *testing.T) {
	current := []string{"A", "B", "C"}
	desired := []api.Peer{{LaptopWgPubkey: "B"}, {LaptopWgPubkey: "D"}}
	if got := Removals(current, desired); !reflect.DeepEqual(got, []string{"A", "C"}) {
		t.Fatalf("Removals: %v", got)
	}
	if got := Removals(nil, desired); got != nil {
		t.Fatalf("Removals from empty current: %v", got)
	}
}

func TestCurrentPeerStatesParsesDumpAndSkipsTheInterfaceLine(t *testing.T) {
	// The dump's first line is the interface (private-key, public-key,
	// listen-port, fwmark). Mistaking it for a peer would make wg0's OWN public
	// key look like a peer, which the removal diff would then try to remove.
	r := &recorder{outputs: dumpOutput(
		dumpPeer{pubkey: "A", endpoint: "[fdaa:0:7e::2]:49152"},
		dumpPeer{pubkey: "B"}, // no endpoint yet → "(none)"
	)}
	n := New(r.run, "/k")
	states, err := n.currentPeerStates()
	if err != nil {
		t.Fatalf("currentPeerStates: %v", err)
	}
	var got []string
	for _, st := range states {
		got = append(got, st.pubkey)
	}
	if !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("parsed peers: %v", got)
	}
	if !reflect.DeepEqual(r.calls, []string{"wg show wg0 dump"}) {
		t.Fatalf("calls: %v", r.calls)
	}
}

func TestCurrentPeerStatesOnPeerlessInterface(t *testing.T) {
	// A dump with only the interface line means zero peers, NOT one peer named
	// after the private key.
	r := &recorder{outputs: dumpOutput()}
	n := New(r.run, "/k")
	states, err := n.currentPeerStates()
	if err != nil {
		t.Fatalf("currentPeerStates: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("currentPeerStates on a peerless interface: %v", states)
	}
}

func TestReconcileSetsAndRemoves(t *testing.T) {
	r := &recorder{outputs: dumpOutput(
		dumpPeer{pubkey: "OLDPEER", endpoint: "5.6.7.8:49151"},
		dumpPeer{pubkey: "KEEP", endpoint: "5.6.7.8:49152"}, // endpoint unchanged
	)}
	n := New(r.run, "/k")
	desired := []api.Peer{
		{LaptopWgPubkey: "KEEP", LaptopWgIP: "fd5e:de7b::aa", RelayEndpoint: "5.6.7.8", RelayPort: 49152},
		{LaptopWgPubkey: "NEW", LaptopWgIP: "fd5e:de7b::bb", RelayEndpoint: "5.6.7.8", RelayPort: 49153},
	}
	if err := n.Reconcile(desired); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg show wg0 dump",
		"wg set wg0 peer KEEP allowed-ips fd5e:de7b::aa/128 persistent-keepalive 25 endpoint 5.6.7.8:49152",
		"wg set wg0 peer NEW allowed-ips fd5e:de7b::bb/128 persistent-keepalive 25 endpoint 5.6.7.8:49153",
		"wg set wg0 peer OLDPEER remove",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
}

func TestReconcileEmptyDesiredRemovesEverything(t *testing.T) {
	// The deny-all case: every peer revoked → wg0 must end peerless (a stale
	// peer would keep a revoked laptop authorized).
	r := &recorder{outputs: dumpOutput(
		dumpPeer{pubkey: "A", endpoint: "5.6.7.8:49152"},
		dumpPeer{pubkey: "B"},
	)}
	n := New(r.run, "/k")
	if err := n.Reconcile(nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg show wg0 dump",
		"wg set wg0 peer A remove",
		"wg set wg0 peer B remove",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
}

func TestReconcileFreshPeerIsOneSetWithNoRemove(t *testing.T) {
	// A peer that does not exist yet is already the fast path: `set_peer` sees
	// persistent_keepalive go 0 → 25 on a brand-new peer and emits a keepalive
	// in ~4ms. Removing first would be pointless work on the connect path.
	r := &recorder{outputs: dumpOutput()}
	n := New(r.run, "/k")
	desired := []api.Peer{{LaptopWgPubkey: "NEW", LaptopWgIP: "fd5e:de7b::bb", RelayEndpoint: "fdaa:0:7e::2", RelayPort: 49153}}
	if err := n.Reconcile(desired); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg show wg0 dump",
		"wg set wg0 peer NEW allowed-ips fd5e:de7b::bb/128 persistent-keepalive 25 endpoint [fdaa:0:7e::2]:49153",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
	if r.hasCall("remove") {
		t.Fatalf("a fresh peer must not be removed first; calls:\n%v", r.calls)
	}
}

func TestReconcileUnchangedEndpointDoesNotRemove(t *testing.T) {
	// THE steady-state path: this runs on every successful config pull. A remove
	// here tears down the live session and forces a fresh handshake every time
	// the pull loop goes round — the worst outcome this package can produce.
	r := &recorder{outputs: dumpOutput(dumpPeer{pubkey: "KEEP", endpoint: "5.6.7.8:49152"})}
	n := New(r.run, "/k")
	desired := []api.Peer{{LaptopWgPubkey: "KEEP", LaptopWgIP: "fd5e:de7b::aa", RelayEndpoint: "5.6.7.8", RelayPort: 49152}}
	if err := n.Reconcile(desired); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg show wg0 dump",
		"wg set wg0 peer KEEP allowed-ips fd5e:de7b::aa/128 persistent-keepalive 25 endpoint 5.6.7.8:49152",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
	if r.hasCall("remove") {
		t.Fatalf("an unchanged endpoint must never be removed; calls:\n%v", r.calls)
	}
}

func TestReconcileChangedEndpointRemovesThenSets(t *testing.T) {
	// A `wg set` over an EXISTING peer is silent — the box then stays quiet
	// until WireGuard's own retransmit ladder fires (measured +2.79s for a port
	// change). Removing first makes the peer fresh, so the set emits in ~6.7ms.
	// The ORDER is the assertion: remove, then set.
	r := &recorder{outputs: dumpOutput(dumpPeer{pubkey: "MOVED", endpoint: "5.6.7.8:49152"})}
	n := New(r.run, "/k")
	desired := []api.Peer{{LaptopWgPubkey: "MOVED", LaptopWgIP: "fd5e:de7b::aa", RelayEndpoint: "5.6.7.8", RelayPort: 49153}}
	if err := n.Reconcile(desired); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg show wg0 dump",
		"wg set wg0 peer MOVED remove",
		"wg set wg0 peer MOVED allowed-ips fd5e:de7b::aa/128 persistent-keepalive 25 endpoint 5.6.7.8:49153",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
}

func TestReconcileChangedRelayHostRemovesThenSets(t *testing.T) {
	// Relay reassignment: same port, different relay. Same remove-then-set.
	r := &recorder{outputs: dumpOutput(dumpPeer{pubkey: "MOVED", endpoint: "[fdaa:0:7e::2]:49152"})}
	n := New(r.run, "/k")
	desired := []api.Peer{{LaptopWgPubkey: "MOVED", LaptopWgIP: "fd5e:de7b::aa", RelayEndpoint: "fdaa:0:7e::3", RelayPort: 49152}}
	if err := n.Reconcile(desired); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg show wg0 dump",
		"wg set wg0 peer MOVED remove",
		"wg set wg0 peer MOVED allowed-ips fd5e:de7b::aa/128 persistent-keepalive 25 endpoint [fdaa:0:7e::3]:49152",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
}

func TestReconcileIPv6EndpointSpellingIsNotAChange(t *testing.T) {
	// The hazard that makes this comparison structural rather than textual: the
	// kernel prints an IPv6 address in ITS canonical form, which differs from
	// what net.JoinHostPort produced from the control plane's string — case,
	// leading zeros, and :: compression all vary. String-comparing these reads
	// "changed" for a peer that did not change, so every steady-state pull would
	// tear down the live session. Each case below is the SAME endpoint spelled
	// two ways and must produce a plain set with no remove.
	cases := []struct {
		name         string
		dumpEndpoint string // as the kernel prints it
		relay        string // as the control plane sends it
		port         int
	}{
		{"uppercase", "[FDAA:0:7E:A9D3:A7B:849:5245:AC5B]:49153", "fdaa:0:7e:a9d3:a7b:849:5245:ac5b", 49153},
		{"leading zeros", "[fdaa:0:7e:a9d3:a7b:849:5245:2]:49153", "fdaa:0000:007e:a9d3:0a7b:0849:5245:0002", 49153},
		{"zero compression", "[fdaa::2]:49153", "fdaa:0:0:0:0:0:0:2", 49153},
		{"mapped v4 vs v4", "1.2.3.4:49153", "::ffff:1.2.3.4", 49153},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{outputs: dumpOutput(dumpPeer{pubkey: "KEEP", endpoint: tc.dumpEndpoint})}
			n := New(r.run, "/k")
			desired := []api.Peer{{LaptopWgPubkey: "KEEP", LaptopWgIP: "fd5e:de7b::aa", RelayEndpoint: tc.relay, RelayPort: tc.port}}
			if err := n.Reconcile(desired); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if r.hasCall("remove") {
				t.Fatalf("%s: same endpoint spelled differently must not churn the peer; calls:\n%v", tc.name, r.calls)
			}
			if len(r.calls) != 2 {
				t.Fatalf("%s: expected dump + set, got:\n%v", tc.name, r.calls)
			}
		})
	}
}

func TestReconcileExistingPeerWithNoEndpointIsFreshened(t *testing.T) {
	// "(none)" → an endpoint is the +4.05s case: `wg set` on an existing peer is
	// silent, so the box waits on the ladder. Treat it as a change.
	r := &recorder{outputs: dumpOutput(dumpPeer{pubkey: "SILENT"})}
	n := New(r.run, "/k")
	desired := []api.Peer{{LaptopWgPubkey: "SILENT", LaptopWgIP: "fd5e:de7b::aa", RelayEndpoint: "5.6.7.8", RelayPort: 49152}}
	if err := n.Reconcile(desired); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg show wg0 dump",
		"wg set wg0 peer SILENT remove",
		"wg set wg0 peer SILENT allowed-ips fd5e:de7b::aa/128 persistent-keepalive 25 endpoint 5.6.7.8:49152",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
}

func TestReconcileUnparseableCurrentEndpointIsTreatedAsChanged(t *testing.T) {
	// Conservative direction: if we cannot tell where the kernel is currently
	// pointing this peer, re-add it rather than leave it pointing somewhere
	// unknown and silent.
	r := &recorder{outputs: dumpOutput(dumpPeer{pubkey: "ODD", endpoint: "not-an-endpoint"})}
	n := New(r.run, "/k")
	desired := []api.Peer{{LaptopWgPubkey: "ODD", LaptopWgIP: "fd5e:de7b::aa", RelayEndpoint: "5.6.7.8", RelayPort: 49152}}
	if err := n.Reconcile(desired); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg show wg0 dump",
		"wg set wg0 peer ODD remove",
		"wg set wg0 peer ODD allowed-ips fd5e:de7b::aa/128 persistent-keepalive 25 endpoint 5.6.7.8:49152",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
}

func TestReconcileDesiredPeerWithoutEndpointNeverChurns(t *testing.T) {
	// A peer whose config carries no endpoint (or a zero port) keeps the
	// long-standing behaviour: `wg set` WITHOUT an endpoint argument, and — since
	// there is no argument that clears an endpoint — never a remove. Removing
	// here would drop the session AND discard the endpoint the kernel learned
	// from the peer's own traffic, on every pull, with no way to converge.
	for _, tc := range []struct {
		name string
		peer api.Peer
	}{
		{"no relay endpoint", api.Peer{LaptopWgPubkey: "P", LaptopWgIP: "fd5e:de7b::aa", RelayPort: 49152}},
		{"zero relay port", api.Peer{LaptopWgPubkey: "P", LaptopWgIP: "fd5e:de7b::aa", RelayEndpoint: "5.6.7.8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The kernel has an endpoint (learned or previously set) — the case
			// where a naive "current != desired" would churn hardest.
			r := &recorder{outputs: dumpOutput(dumpPeer{pubkey: "P", endpoint: "5.6.7.8:49152"})}
			n := New(r.run, "/k")
			if err := n.Reconcile([]api.Peer{tc.peer}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			want := []string{
				"wg show wg0 dump",
				"wg set wg0 peer P allowed-ips fd5e:de7b::aa/128 persistent-keepalive 25",
			}
			if !reflect.DeepEqual(r.calls, want) {
				t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
			}
		})
	}
}

func TestEndpointDiffers(t *testing.T) {
	cases := []struct {
		name             string
		current, desired string
		want             bool
	}{
		{"identical", "5.6.7.8:49152", "5.6.7.8:49152", false},
		{"port changed", "5.6.7.8:49152", "5.6.7.8:49153", true},
		{"host changed", "5.6.7.8:49152", "5.6.7.9:49152", true},
		{"same v6, different spelling", "[FDAA::2]:49152", "[fdaa:0:0:0:0:0:0:2]:49152", false},
		{"no current endpoint", "", "5.6.7.8:49152", true},
		{"unparseable current", "garbage", "5.6.7.8:49152", true},
		{"no desired endpoint", "5.6.7.8:49152", "", false},
		{"neither", "", "", false},
		// A non-literal desired endpoint (a hostname) is un-comparable: `wg`
		// resolves it and the dump prints the resolved literal, so any textual
		// check would report a change forever. Not emitted by the control plane;
		// pinned here so the choice is deliberate rather than incidental.
		{"unparseable desired", "5.6.7.8:49152", "relay.internal:49152", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := endpointDiffers(tc.current, tc.desired); got != tc.want {
				t.Fatalf("endpointDiffers(%q, %q) = %v, want %v", tc.current, tc.desired, got, tc.want)
			}
		})
	}
}

// A single transient re-add failure must NOT leave the peer deleted.
//
// remove-then-re-add introduced a state Reconcile could never produce before: the
// remove succeeds, the re-add fails, and wg0 is left with no peer at all — the
// laptop cannot reach the box. A fork/exec or netlink hiccup on a memory-pressured
// 1-vCPU box is the likeliest cause and is by nature momentary, so the re-add is
// retried once inline rather than waiting for the caller's next poll.
func TestReconcileRetriesAFailedReAddInline(t *testing.T) {
	moved := api.Peer{
		LaptopWgPubkey: "MOVED", LaptopWgIP: "fd5e:de7b::a",
		RelayEndpoint: "fdaa:0:7e::2", RelayPort: 49153,
	}
	r := &recorder{
		outputs:  dumpOutput(dumpPeer{pubkey: "MOVED", endpoint: "[fdaa:0:7e::2]:49152"}),
		failOnce: map[string]string{"wg set wg0 peer MOVED allowed-ips": "netlink: out of memory"},
	}
	n := New(r.run, "/k")

	if err := n.Reconcile([]api.Peer{moved}); err != nil {
		t.Fatalf("a single transient re-add failure should have been retried away, got: %v", err)
	}
	sets := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, "wg set wg0 peer MOVED allowed-ips") {
			sets++
		}
	}
	if sets != 2 {
		t.Fatalf("re-add attempted %d time(s), want 2 (the failure plus one inline retry). calls: %v",
			sets, r.calls)
	}
}

// When the re-add fails PERSISTENTLY the peer really is absent, and the error must
// say so distinctly — "a wg set failed" is the benign case (the peer keeps its old
// config); this one means wg0 is missing an authorized peer. The two want different
// operator responses, and the caller only sees what we return.
func TestReconcileNamesTheMissingPeerWhenTheReAddKeepsFailing(t *testing.T) {
	moved := api.Peer{
		LaptopWgPubkey: "MOVED", LaptopWgIP: "fd5e:de7b::a",
		RelayEndpoint: "fdaa:0:7e::2", RelayPort: 49153,
	}
	r := &recorder{
		outputs: dumpOutput(
			dumpPeer{pubkey: "MOVED", endpoint: "[fdaa:0:7e::2]:49152"},
			dumpPeer{pubkey: "REVOKED", endpoint: "[fdaa:0:7e::2]:49999"},
		),
		fails: map[string]string{"wg set wg0 peer MOVED allowed-ips": "netlink: out of memory"},
	}
	n := New(r.run, "/k")

	err := n.Reconcile([]api.Peer{moved})
	if err == nil {
		t.Fatal("Reconcile reported success although the peer is absent from wg0")
	}
	if !strings.Contains(err.Error(), "MISSING") || !strings.Contains(err.Error(), "MOVED") {
		t.Fatalf("the error does not distinguish 'peer is absent' from an ordinary set "+
			"failure, and does not name the peer: %v", err)
	}
	// The security-critical removals pass must still have run.
	if !r.hasCall("wg set wg0 peer REVOKED remove") {
		t.Fatalf("the removals pass was skipped, leaving a revoked peer authorized. calls: %v", r.calls)
	}
}

// A peer whose desired config cannot produce a valid `wg set` must NOT take the
// destructive path.
//
// An empty LaptopWgIP yields a bare "/128" for allowed-ips, so the re-add fails
// every time, not transiently — the inline retry cannot rescue it. Removing first
// would delete a working peer and never restore it, which is strictly worse than
// the pre-change behaviour (peer left alone with its old endpoint). Malformed
// input must degrade to the old behaviour, not to an outage.
func TestReconcileDoesNotRemoveAPeerItCannotReAdd(t *testing.T) {
	malformed := api.Peer{
		LaptopWgPubkey: "NOIP", LaptopWgIP: "", // cannot produce valid allowed-ips
		RelayEndpoint: "fdaa:0:7e::2", RelayPort: 49153,
	}
	r := &recorder{
		outputs: dumpOutput(dumpPeer{pubkey: "NOIP", endpoint: "[fdaa:0:7e::2]:49152"}), // endpoint DIFFERS
		fails:   map[string]string{"wg set wg0 peer NOIP allowed-ips": "Invalid allowed-ips"},
	}
	n := New(r.run, "/k")

	_ = n.Reconcile([]api.Peer{malformed})

	if r.hasCall("wg set wg0 peer NOIP remove") {
		t.Fatalf("a peer that can never be re-added was removed anyway; it is now absent from "+
			"wg0 permanently, which is worse than leaving its stale endpoint alone. calls: %v",
			r.calls)
	}
}

// The "wg0 is MISSING a peer" error must outrank an earlier benign failure.
//
// Guard 1 rests on the caller seeing that text — it is what distinguishes an
// outage (a peer is absent) from the benign case (a set failed, peer keeps its
// old config). If an ordinary failure on an EARLIER peer wins the firstErr slot,
// the distinction is silently dropped and the operator is told the wrong thing.
func TestTheMissingPeerErrorOutranksAnEarlierBenignFailure(t *testing.T) {
	benign := api.Peer{ // sorted first by the fixture order below; its set just fails
		LaptopWgPubkey: "AAA", LaptopWgIP: "fd5e:de7b::a",
		RelayEndpoint: "fdaa:0:7e::2", RelayPort: 49153,
	}
	moved := api.Peer{ // endpoint differs ⇒ remove+re-add; the re-add fails persistently
		LaptopWgPubkey: "BBB", LaptopWgIP: "fd5e:de7b::b",
		RelayEndpoint: "fdaa:0:7e::2", RelayPort: 49153,
	}
	r := &recorder{
		outputs: dumpOutput(
			dumpPeer{pubkey: "AAA", endpoint: "[fdaa:0:7e::2]:49153"}, // unchanged
			dumpPeer{pubkey: "BBB", endpoint: "[fdaa:0:7e::2]:49152"}, // changed
		),
		fails: map[string]string{
			"wg set wg0 peer AAA allowed-ips": "transient",
			"wg set wg0 peer BBB allowed-ips": "netlink: out of memory",
		},
	}
	n := New(r.run, "/k")

	err := n.Reconcile([]api.Peer{benign, moved})
	if err == nil {
		t.Fatal("Reconcile reported success although a peer is absent from wg0")
	}
	if !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("an earlier benign set failure masked the peer-absent error, so the operator "+
			"is told the wrong thing: %v", err)
	}
}
