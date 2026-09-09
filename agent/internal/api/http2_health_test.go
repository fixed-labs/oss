package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- HTTP/2 health-check pings ------------------------------------------------
//
// The warm-resume wedge these guard against, proven rather than asserted in a
// comment.
//
// The control plane speaks h2 and the transport keeps ForceAttemptHTTP2, so
// every call this client makes shares ONE *http2ClientConn. A Fly suspend takes
// the peer's end of that connection away without the box — frozen — ever
// observing a FIN. CloseIdleConnections cannot clean it up: it reaches
// http2ClientConn.closeIfIdle, which returns early while len(cc.streams) > 0,
// and the config long-poll holds a stream essentially always. Nothing else
// evicts it either, so every LATER request, freshly issued, is handed the same
// corpse and dies at the 35s Client.Timeout — permanently.
//
// Two tests, because the proof has two halves that cannot be done at once:
// TestNewCarriesHTTP2HealthCheckPings pins what is actually SHIPPED, and
// TestHTTP2PingsEvictAConnectionWhosePeerVanishedWithoutAFIN proves that config
// does what is claimed, at shortened timeouts so the proof costs seconds.

const (
	// Stand-ins for the shipped 5s/5s: detection lands at ~2s instead of ~10s.
	probePingIdle = 1 * time.Second
	probePingWait = 1 * time.Second

	// Detection is bounded by probePingIdle+probePingWait ≈ 2s; 5s is headroom for
	// a loaded machine. It is also far inside the 35s Client.Timeout — the only
	// other bound in play — so a pass here cannot be the client timeout wearing a
	// disguise.
	failFastBudget = 5 * time.Second

	// Without the pings NOTHING detects anything: no FIN is ever sent, the
	// transport has no other liveness signal, and TCP's own retransmit ladder runs
	// for minutes. So 3s of "still in flight" is conclusive for the control case,
	// and keeps it fast — the claim is that nothing happens, not that something
	// happens slowly.
	stillHungProbe = 3 * time.Second
)

// TestNewCarriesHTTP2HealthCheckPings pins the shipped configuration, including
// the two properties the mechanism below silently depends on: that h2 is
// attempted at all (no h2, no single-ClientConn failure mode and no ping-based
// cure), and that a lost connection is detected strictly BEFORE the request
// that is riding it gives up — pings slower than Client.Timeout would change
// nothing.
func TestNewCarriesHTTP2HealthCheckPings(t *testing.T) {
	c := New("https://control.invalid", "ws-1", "tok")
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want an owned *http.Transport", c.HTTP.Transport)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 is off — with HTTP/1.1 the pool is per-connection " +
			"and CloseIdleConnections alone suffices, so the ping config below is " +
			"dead weight and this whole story needs revisiting")
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout == 0 || tr.HTTP2.PingTimeout == 0 {
		t.Fatalf("HTTP2 = %+v — a zero SendPingTimeout disables the health check "+
			"entirely, which is exactly the pre-fix wedge", tr.HTTP2)
	}
	if worst := tr.HTTP2.SendPingTimeout + tr.HTTP2.PingTimeout; worst >= c.HTTP.Timeout {
		t.Fatalf("worst-case detection %v >= Client.Timeout %v: the request riding the "+
			"dead connection would time out before the transport noticed, so the "+
			"connection would stay pooled and the next request would wedge too",
			worst, c.HTTP.Timeout)
	}
}

// TestHTTP2PingsEvictAConnectionWhosePeerVanishedWithoutAFIN reproduces the
// resume in miniature: an h2 server behind a proxy that stops forwarding both
// directions without closing anything, a long-poll holding a stream, then
// thaw's CloseIdleConnections and thaw's forced heartbeat.
//
// The control case is what makes the fix case mean something. It shows both
// halves of the bug on today's transport: CloseIdleConnections does not help
// (the heartbeat issued immediately AFTER it still rides the corpse), and the
// stale long-poll is not cleaned up either.
func TestHTTP2PingsEvictAConnectionWhosePeerVanishedWithoutAFIN(t *testing.T) {
	for _, tc := range []struct {
		name     string
		h2       *http.HTTP2Config
		wantFast bool
	}{
		{
			name:     "with health-check pings",
			h2:       &http.HTTP2Config{SendPingTimeout: probePingIdle, PingTimeout: probePingWait},
			wantFast: true,
		},
		{
			name: "without health-check pings",
			h2:   nil, // today's transport, i.e. the wedge
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The control plane's two relevant endpoints. cursor "" is the absolute
			// read (answers immediately); any cursor is the ≤25s long-poll hold.
			shutdown := make(chan struct{})
			polling := make(chan struct{})
			var pollOnce sync.Once
			var warmProto atomic.Value

			mux := http.NewServeMux()
			mux.HandleFunc("/api/rift/v1/ws-1/agent-config", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("cursor") == "" {
					warmProto.Store(r.Proto)
					_ = json.NewEncoder(w).Encode(map[string]any{"cursor": "h:1", "peers": []any{}})
					return
				}
				pollOnce.Do(func() { close(polling) })
				select {
				case <-time.After(25 * time.Second):
					w.WriteHeader(http.StatusNotModified)
				case <-r.Context().Done():
				case <-shutdown:
				}
			})
			mux.HandleFunc("/api/rift/v1/ws-1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			})

			srv := httptest.NewUnstartedServer(mux)
			srv.EnableHTTP2 = true
			srv.StartTLS()
			t.Cleanup(srv.Close)
			// Cleanups run LIFO, so this one runs BEFORE srv.Close: it releases the
			// held long-poll, which Close would otherwise sit and wait out.
			t.Cleanup(func() { close(shutdown) })

			bh := newBlackhole(t, srv.Listener.Addr().String())

			// The SHIPPED client, with exactly two things changed: it trusts the test
			// server's CA, and the ping timeouts are the shortened ones above (or nil
			// for the control). Everything else — the 35s timeout, the cloned default
			// transport, ForceAttemptHTTP2 — is production's.
			c := New("https://"+bh.addr(), "ws-1", "tok")
			tr := c.HTTP.Transport.(*http.Transport)
			tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			tr.HTTP2 = tc.h2

			// Warm the connection. From here on every call multiplexes onto this one
			// ClientConn, which is the precondition for the whole failure mode.
			if _, err := c.PullConfig(context.Background(), ""); err != nil {
				t.Fatalf("warm-up pull: %v", err)
			}
			if got, _ := warmProto.Load().(string); got != "HTTP/2.0" {
				t.Fatalf("server saw %q, want HTTP/2.0 — over HTTP/1.1 this test passes "+
					"vacuously, because there is no shared ClientConn to strand", got)
			}

			// The config long-poll goes in flight and holds a stream open. Waiting for
			// the server to confirm it, rather than sleeping, is what keeps the freeze
			// below deterministic.
			pollErr := make(chan error, 1)
			go func() {
				_, err := c.PullConfig(context.Background(), "h:1")
				// Never touch t from here: in the control case this outlives the test.
				pollErr <- err
			}()
			select {
			case <-polling:
			case <-time.After(failFastBudget):
				t.Fatal("the long-poll never reached the server")
			}

			// The VM freezes and the peer goes away. Nothing is closed — bytes simply
			// stop moving in both directions, so no FIN is ever observed.
			bh.drop()
			frozen := time.Now()

			// thaw's first act today. It reaches http2ClientConn.closeIfIdle, which
			// returns early because the long-poll is holding a stream, so the dead
			// ClientConn stays pooled — and the heartbeat issued right after it is
			// handed that same connection rather than a fresh dial.
			c.CloseIdleConnections()
			beatErr := make(chan error, 1)
			go func() {
				beatErr <- c.Heartbeat(context.Background(), false, 0, Identity{WgPubkey: "WGPUB"})
			}()

			inFlight := []struct {
				what string
				ch   <-chan error
			}{
				{"the in-flight long-poll", pollErr},
				{"the freshly-issued heartbeat", beatErr},
			}

			if tc.wantFast {
				for _, req := range inFlight {
					select {
					case err := <-req.ch:
						if err == nil {
							t.Fatalf("%s SUCCEEDED across a blackholed connection", req.what)
						}
						t.Logf("%s failed %v after the freeze: %v",
							req.what, time.Since(frozen).Round(time.Millisecond), err)
					case <-time.After(failFastBudget):
						t.Fatalf("%s was still in flight %v after the peer vanished — the "+
							"health-check pings did not tear the ClientConn down, so a "+
							"resumed box is wedged until the process restarts",
							req.what, failFastBudget)
					}
				}
				return
			}

			// Control. Bounded deliberately: the claim is that nothing at all happens,
			// so a short window is enough to demonstrate it, and the test does not pay
			// for the 35s Client.Timeout that is the only thing which would eventually
			// fire (and which leaves the ClientConn pooled anyway, wedging the next
			// request too).
			deadline := time.After(stillHungProbe)
			for range inFlight {
				select {
				case err := <-pollErr:
					t.Fatalf("without the ping config the long-poll returned after %v (%v) — "+
						"something else now detects a peer that vanished without a FIN, so "+
						"re-derive whether the pings are still load-bearing",
						time.Since(frozen).Round(time.Millisecond), err)
				case err := <-beatErr:
					t.Fatalf("without the ping config the post-CloseIdleConnections heartbeat "+
						"returned after %v (%v) — CloseIdleConnections appears to work on an "+
						"h2 conn holding a stream, which contradicts the premise of the fix",
						time.Since(frozen).Round(time.Millisecond), err)
				case <-deadline:
					return
				}
			}
		})
	}
}

// blackhole is a TCP proxy that can be told to silently stop forwarding in BOTH
// directions without closing either socket. That is the frozen-VM case exactly:
// the peer never observes a FIN, so the kernel still believes the connection is
// established and the HTTP/2 layer still believes the ClientConn is healthy.
// Closing the sockets instead would prove nothing — a FIN is handled correctly
// today.
type blackhole struct {
	ln       net.Listener
	backend  string
	dropping atomic.Bool

	mu    sync.Mutex
	conns []net.Conn
}

func newBlackhole(t *testing.T, backend string) *blackhole {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &blackhole{ln: ln, backend: backend}
	t.Cleanup(b.close)
	go b.serve()
	return b
}

func (b *blackhole) addr() string { return b.ln.Addr().String() }

// drop engages the blackhole. It is one-way on purpose: a resume never gets the
// old connection back.
func (b *blackhole) drop() { b.dropping.Store(true) }

func (b *blackhole) serve() {
	for {
		client, err := b.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", b.backend)
		if err != nil {
			_ = client.Close()
			return
		}
		b.mu.Lock()
		b.conns = append(b.conns, client, server)
		b.mu.Unlock()
		go b.pipe(client, server)
		go b.pipe(server, client)
	}
}

// pipe copies src→dst, except that once dropping it reads and DISCARDS. It
// never closes either side; the bytes simply stop arriving.
func (b *blackhole) pipe(src, dst net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 && !b.dropping.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (b *blackhole) close() {
	_ = b.ln.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		_ = c.Close()
	}
}
