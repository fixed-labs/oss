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
	fails   map[string]string // command prefix → error output
}

func (r *recorder) run(name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, cmd)
	for prefix, out := range r.fails {
		if strings.HasPrefix(cmd, prefix) {
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

func TestReconcileSetsAndRemoves(t *testing.T) {
	r := &recorder{outputs: map[string]string{
		"wg show wg0 peers": "OLDPEER\nKEEP\n",
	}}
	n := New(r.run, "/k")
	desired := []api.Peer{
		{LaptopWgPubkey: "KEEP", LaptopWgIP: "fd5e:de7b::aa", RelayEndpoint: "5.6.7.8", RelayPort: 49152},
		{LaptopWgPubkey: "NEW", LaptopWgIP: "fd5e:de7b::bb", RelayEndpoint: "5.6.7.8", RelayPort: 49153},
	}
	if err := n.Reconcile(desired); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg set wg0 peer KEEP allowed-ips fd5e:de7b::aa/128 persistent-keepalive 25 endpoint 5.6.7.8:49152",
		"wg set wg0 peer NEW allowed-ips fd5e:de7b::bb/128 persistent-keepalive 25 endpoint 5.6.7.8:49153",
		"wg show wg0 peers",
		"wg set wg0 peer OLDPEER remove",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
}

func TestReconcileEmptyDesiredRemovesEverything(t *testing.T) {
	// The deny-all case: every peer revoked → wg0 must end peerless (a stale
	// peer would keep a revoked laptop authorized).
	r := &recorder{outputs: map[string]string{"wg show wg0 peers": "A\nB\n"}}
	n := New(r.run, "/k")
	if err := n.Reconcile(nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg show wg0 peers",
		"wg set wg0 peer A remove",
		"wg set wg0 peer B remove",
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
}
