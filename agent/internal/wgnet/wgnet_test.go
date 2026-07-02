package wgnet

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

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
	n.laptopIPPath = filepath.Join(t.TempDir(), "laptop-wg-ips")
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
		"wg show wg0 dump", // Reconcile re-publishes the live broker file
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
	n.laptopIPPath = filepath.Join(t.TempDir(), "laptop-wg-ips")
	if err := n.Reconcile(nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{
		"wg show wg0 peers",
		"wg set wg0 peer A remove",
		"wg set wg0 peer B remove",
		"wg show wg0 dump", // Reconcile re-publishes the live broker file
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%v\nwant:\n%v", r.calls, want)
	}
}

// dumpLine builds one `wg show wg0 dump` peer line (pubkey, psk, endpoint,
// allowed-ips, latest-handshake, rx, tx, keepalive).
func dumpLine(pubkey, allowedIP string, handshake int64) string {
	return strings.Join([]string{
		pubkey, "(none)", "1.2.3.4:49152", allowedIP,
		strconv.FormatInt(handshake, 10), "0", "0", "25",
	}, "\t")
}

const wgDumpIfaceLine = "PRIV\tPUB\t51820\toff"

func TestParseWgDump(t *testing.T) {
	dump := wgDumpIfaceLine + "\n" +
		"A\t(none)\t1.2.3.4:1\tfd5e::aa/128\t1700000000\t1\t2\t25\n" +
		"\n" + // blank line tolerated
		"B\t(none)\t(none)\t(none)\t0\t0\t0\toff\n"
	got := parseWgDump(dump)
	want := []wgPeer{
		{pubkey: "A", allowedIP: "fd5e::aa", lastHandshake: 1700000000},
		{pubkey: "B", allowedIP: "", lastHandshake: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseWgDump:\n%#v\nwant\n%#v", got, want)
	}
}

// TestPublishLiveLaptopIPs locks in that the broker file carries only LIVE
// connections (recent — or not-yet-completed — wg handshake), so a stale strand is
// excluded while a freshly-connected peer (handshake 0) is published immediately.
func TestPublishLiveLaptopIPs(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name string
		dump string
		want string
	}{
		{
			name: "recent peer published",
			dump: wgDumpIfaceLine + "\n" + dumpLine("A", "fd5e:de7b::aa/128", now.Unix()-10) + "\n",
			want: "fd5e:de7b::aa\n",
		},
		{
			name: "stale strand pruned, live kept",
			dump: wgDumpIfaceLine + "\n" +
				dumpLine("LIVE", "fd5e:de7b::aa/128", now.Unix()-10) + "\n" +
				dumpLine("STRAND", "fd5e:de7b::bb/128", now.Unix()-1000) + "\n",
			want: "fd5e:de7b::aa\n",
		},
		{
			name: "never-handshaked peer kept (grace — no new-connect gap)",
			dump: wgDumpIfaceLine + "\n" + dumpLine("NEW", "fd5e:de7b::cc/128", 0) + "\n",
			want: "fd5e:de7b::cc\n",
		},
		{
			name: "all stale → empty file",
			dump: wgDumpIfaceLine + "\n" +
				dumpLine("S1", "fd5e:de7b::aa/128", now.Unix()-1000) + "\n" +
				dumpLine("S2", "fd5e:de7b::bb/128", now.Unix()-2000) + "\n",
			want: "",
		},
		{
			name: "(none) allowed-ips skipped",
			dump: wgDumpIfaceLine + "\n" + dumpLine("X", "(none)", now.Unix()-10) + "\n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{outputs: map[string]string{"wg show wg0 dump": tc.dump}}
			n := New(r.run, "/k")
			n.now = func() time.Time { return now }
			path := filepath.Join(t.TempDir(), "laptop-wg-ips")
			n.laptopIPPath = path
			if err := n.PublishLiveLaptopIPs(); err != nil {
				t.Fatalf("PublishLiveLaptopIPs: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("published %q, want %q", got, tc.want)
			}
			// The file must be world-readable for the unprivileged `devbox run` user.
			if fi, _ := os.Stat(path); fi != nil && fi.Mode().Perm() != 0o644 {
				t.Fatalf("file mode = %v, want 0644", fi.Mode().Perm())
			}
		})
	}
}
