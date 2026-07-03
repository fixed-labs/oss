package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
