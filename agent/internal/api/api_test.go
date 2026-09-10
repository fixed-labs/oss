package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, "ws-1", "tok")
}

func TestHeartbeatShape(t *testing.T) {
	var got map[string]any
	var path, auth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	id := Identity{SSHHost: "1.2.3.4", ResolvedCommit: "abc123",
		WgPubkey: "WGPUB", SSHHostPubkey: "ssh-ed25519 HOST"}
	if err := c.Heartbeat(context.Background(), true, 3, id); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if path != "/api/rift/v1/ws-1/heartbeat" {
		t.Fatalf("path: %q", path)
	}
	if auth != "Bearer tok" {
		t.Fatalf("auth: %q", auth)
	}
	// liveness + the identity facts that drive provisioned/starting → running.
	want := map[string]any{
		"interactive_live": true, "ssh_sessions": float64(3),
		"ssh_host": "1.2.3.4", "resolved_commit": "abc123",
		"wg_pubkey": "WGPUB", "ssh_host_pubkey": "ssh-ed25519 HOST",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("body[%q] = %v, want %v", k, got[k], v)
		}
	}
}

func TestPullConfigDecodesPeers(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != "h:prev" {
			t.Errorf("cursor: %q", r.URL.Query().Get("cursor"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cursor": "h:abc",
			"peers": []map[string]any{{
				"laptop_wg_pubkey": "LP", "laptop_wg_ip": "fd5e:de7b::aa",
				"developer_id": "u-1", "login_user": "dev",
				"relay_endpoint": "5.6.7.8", "relay_port": 49152,
				"lease_until": 12345,
			}},
		})
	})
	cfg, err := c.PullConfig(context.Background(), "h:prev")
	if err != nil {
		t.Fatalf("PullConfig: %v", err)
	}
	if cfg.Cursor != "h:abc" || len(cfg.Peers) != 1 {
		t.Fatalf("cfg: %+v", cfg)
	}
	p := cfg.Peers[0]
	if p.LaptopWgPubkey != "LP" || p.LaptopWgIP != "fd5e:de7b::aa" ||
		p.DeveloperID != "u-1" || p.LoginUser != "dev" ||
		p.RelayEndpoint != "5.6.7.8" || p.RelayPort != 49152 || p.LeaseUntil != 12345 {
		t.Fatalf("peer: %+v", p)
	}
}

func TestPullConfig304(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	_, err := c.PullConfig(context.Background(), "h:same")
	if !errors.Is(err, ErrNotModified) {
		t.Fatalf("expected ErrNotModified, got %v", err)
	}
}

func TestSessionPostShapes(t *testing.T) {
	type capture struct {
		path string
		body map[string]any
	}
	var got capture
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	t.Run("create", func(t *testing.T) {
		if err := c.CreateSession(context.Background(), "sid-1", "main", 7); err != nil {
			t.Fatal(err)
		}
		if got.path != "/api/rift/v1/ws-1/sessions" {
			t.Fatalf("path: %q", got.path)
		}
		want := map[string]any{"type": "create", "id": "sid-1", "name": "main", "gen_epoch": float64(7)}
		assertBody(t, got.body, want)
		if _, hasTS := got.body["timestamp"]; hasTS {
			t.Fatal("agent must NOT send a timestamp")
		}
	})

	t.Run("end", func(t *testing.T) {
		if err := c.EndSession(context.Background(), "sid-1", "shell exited code 0"); err != nil {
			t.Fatal(err)
		}
		assertBody(t, got.body, map[string]any{"type": "end", "id": "sid-1", "reason": "shell exited code 0"})
	})

	t.Run("sync", func(t *testing.T) {
		snap := map[string]SessionMeta{
			"sid-1": {Name: "main", AttachedCount: 2, ForegroundCmd: "vim", ForegroundCwd: "/home/dev"},
		}
		if err := c.SyncSessions(context.Background(), 7, snap); err != nil {
			t.Fatal(err)
		}
		if got.body["type"] != "sync" || got.body["gen_epoch"] != float64(7) {
			t.Fatalf("sync body: %+v", got.body)
		}
		sessions, ok := got.body["sessions"].(map[string]any)
		if !ok {
			t.Fatalf("sessions not an object: %+v", got.body["sessions"])
		}
		s1, ok := sessions["sid-1"].(map[string]any)
		if !ok {
			t.Fatalf("sid-1 missing: %+v", sessions)
		}
		if s1["name"] != "main" || s1["attached_count"] != float64(2) ||
			s1["foreground_cmd"] != "vim" || s1["foreground_cwd"] != "/home/dev" {
			t.Fatalf("sync meta wire shape: %+v", s1)
		}
	})

	t.Run("tombstone", func(t *testing.T) {
		if err := c.TombstoneStaleSessions(context.Background(), 3); err != nil {
			t.Fatal(err)
		}
		assertBody(t, got.body, map[string]any{"type": "tombstone", "gen_epoch": float64(3)})
	})
}

func assertBody(t *testing.T, got, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("body[%q] = %v (%T), want %v (%T)", k, got[k], got[k], v, v)
		}
	}
}

func TestErrorStatusSurfaces(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Invalid or missing bearer token", http.StatusUnauthorized)
	})
	if err := c.Heartbeat(context.Background(), false, 0, Identity{}); err == nil {
		t.Fatal("expected error on 401")
	}
}

// --- client_system heartbeat key (rift-image-model §3.7) ----------------------
//
// The promote gate reads this key at the WIRE, so the assertions parse the raw
// serialized JSON body captured server-side — never the in-memory map the
// client built. Absent (not empty, not null) is the legacy-compat contract:
// pre-bootstrap agents must never send the field.

// rawBodyClient returns a client whose server records every POST's raw body
// bytes; decoded() parses each captured body fresh.
func rawBodyClient(t *testing.T) (*Client, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var raws [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		raws = append(raws, b)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "ws-1", "tok")
	decoded := func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		out := make([]map[string]any, len(raws))
		for i, rb := range raws {
			var m map[string]any
			if err := json.Unmarshal(rb, &m); err != nil {
				t.Fatalf("body %d not JSON: %v (%q)", i, err, rb)
			}
			out[i] = m
		}
		return out
	}
	return c, decoded
}

// heartbeatIdentity is the shared identity fixture; the resulting six core
// fields are asserted identically in every case (positive anchor for the
// key-absence negatives, and "the other six fields unchanged").
func heartbeatIdentity() Identity {
	return Identity{SSHHost: "1.2.3.4", ResolvedCommit: "abc123",
		WgPubkey: "WGPUB", SSHHostPubkey: "ssh-ed25519 HOST"}
}

func assertHeartbeatCoreFields(t *testing.T, m map[string]any) {
	t.Helper()
	want := map[string]any{
		"interactive_live": true,
		"ssh_sessions":     float64(3),
		"ssh_host":         "1.2.3.4",
		"resolved_commit":  "abc123",
		"wg_pubkey":        "WGPUB",
		"ssh_host_pubkey":  "ssh-ed25519 HOST",
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("body[%q] = %v, want %v", k, m[k], v)
		}
	}
}

// Hook unset, and hook returning "": the serialized object LACKS the key
// entirely. Armed by TestHeartbeatClientSystemKeyPresent, which shares this
// exact call shape with only the hook return varied.
func TestHeartbeatClientSystemKeyAbsentForLegacy(t *testing.T) {
	c, decoded := rawBodyClient(t)
	if err := c.Heartbeat(context.Background(), true, 3, heartbeatIdentity()); err != nil {
		t.Fatal(err)
	}
	c.ClientSystem = func() string { return "" }
	if err := c.Heartbeat(context.Background(), true, 3, heartbeatIdentity()); err != nil {
		t.Fatal(err)
	}
	bodies := decoded()
	if len(bodies) != 2 {
		t.Fatalf("captured %d bodies, want 2", len(bodies))
	}
	for i, m := range bodies {
		assertHeartbeatCoreFields(t, m) // the beat really carried its payload
		if v, ok := m["client_system"]; ok {
			t.Fatalf("body %d carries client_system (%v) — legacy compat is ABSENT, not empty/null", i, v)
		}
	}
}

func TestHeartbeatClientSystemKeyPresent(t *testing.T) {
	c, decoded := rawBodyClient(t)
	c.ClientSystem = func() string { return "healthy" }
	if err := c.Heartbeat(context.Background(), true, 3, heartbeatIdentity()); err != nil {
		t.Fatal(err)
	}
	bodies := decoded()
	if len(bodies) != 1 {
		t.Fatalf("captured %d bodies, want 1", len(bodies))
	}
	if bodies[0]["client_system"] != "healthy" {
		t.Fatalf("client_system = %v, want healthy", bodies[0]["client_system"])
	}
	assertHeartbeatCoreFields(t, bodies[0]) // the other six fields unchanged
}

// Freshness: the hook is evaluated per POST — a cache-once implementation
// would freeze a box at "realizing" and wedge the promote gate while both
// static cases above stay green. Two beats, two different hook values, each
// serialized body carries its own.
func TestHeartbeatClientSystemFreshPerPOST(t *testing.T) {
	c, decoded := rawBodyClient(t)
	vals := []string{"realizing", "healthy"}
	calls := 0
	c.ClientSystem = func() string {
		v := vals[calls]
		if calls < len(vals)-1 {
			calls++
		}
		return v
	}
	for range 2 {
		if err := c.Heartbeat(context.Background(), true, 3, heartbeatIdentity()); err != nil {
			t.Fatal(err)
		}
	}
	bodies := decoded()
	if len(bodies) != 2 {
		t.Fatalf("captured %d bodies, want 2", len(bodies))
	}
	if bodies[0]["client_system"] != "realizing" || bodies[1]["client_system"] != "healthy" {
		t.Fatalf("client_system per beat = %v / %v, want realizing then healthy",
			bodies[0]["client_system"], bodies[1]["client_system"])
	}
}

// --- Warm-resume connection hygiene ------------------------------------------

// The client must POOL connections in steady state and DROP them on demand.
// Both halves matter and they pull in opposite directions, so they are asserted
// against a real server on one connection-counting harness.
//
// Why the drop exists: a Fly suspend freezes the VM, so every pooled socket is
// torn down by the far end without the box ever seeing the FIN. On resume the
// pool looks healthy, the transport hands out a corpse, and the request hangs
// until the 35s client timeout — every call, indefinitely. Transport's own
// IdleConnTimeout cannot help: it runs on the monotonic clock, which does not
// advance across a suspend.
//
// Why the pooling half is asserted too: the cheap "fix" is DisableKeepAlives,
// which would pay a fresh TCP+TLS handshake on every heartbeat of every box
// forever. Reuse-then-drop keeps the steady-state cost and fixes the resume.
func TestCloseIdleConnectionsForcesAFreshDial(t *testing.T) {
	var newConns atomic.Int64
	// UNSTARTED, so ConnState is installed BEFORE any connection can be accepted.
	// Assigning srv.Config.ConnState after httptest.NewServer has already begun
	// serving races net/http.(*conn).setState — a real -race failure, not a
	// theoretical one.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	c := New(srv.URL, "ws-1", "tok")
	ctx := context.Background()

	pull := func() {
		t.Helper()
		if _, err := c.PullConfig(ctx, "cursor"); err != nil {
			t.Fatalf("pull: %v", err)
		}
	}

	// The pull loop is what populates the pool: it decodes the whole body, so its
	// connection goes back as reusable.
	pull()
	pull()
	if got := newConns.Load(); got != 1 {
		t.Fatalf("two sequential pulls opened %d connections, want 1 — keep-alive "+
			"reuse is off, so every poll on every box pays a fresh handshake", got)
	}

	// And THIS is the mechanism of the resume bug: the pool is per-HOST, not
	// per-call-site, so the heartbeat rides whatever socket the pull loop left
	// behind. After a suspend that socket is dead, which is why thaw()'s forced
	// beat — the one request that flips the row to running — was a casualty and
	// not just the pull loop.
	if err := c.Heartbeat(ctx, false, 0, Identity{WgPubkey: "WGPUB"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if got := newConns.Load(); got != 1 {
		t.Fatalf("the heartbeat dialed its own connection (total %d, want 1) — if that "+
			"is now true by design the resume story changes, because the beat no "+
			"longer inherits the pull loop's pooled socket", got)
	}

	c.CloseIdleConnections()

	pull()
	if got := newConns.Load(); got != 2 {
		t.Fatalf("after CloseIdleConnections the next request brought the total to %d "+
			"connections, want 2 — the pooled socket was reused, which after a warm "+
			"resume means reusing one that died during the suspend", got)
	}
}
