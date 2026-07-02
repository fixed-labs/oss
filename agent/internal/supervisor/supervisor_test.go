package supervisor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// mockAPI scripts PullConfig results and records everything.
type mockAPI struct {
	mu         sync.Mutex
	hbCalls    int // total Heartbeat invocations (incl. failed)
	hbFails    int // fail the first N heartbeats
	heartbeats []heartbeat
	pulls      []string // cursors received
	script     []pullResult
}

type heartbeat struct {
	interactive bool
	ssh         int
	id          api.Identity
}

type pullResult struct {
	cfg *api.Config
	err error
}

func (m *mockAPI) Heartbeat(_ context.Context, live bool, sshSessions int, id api.Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hbCalls++
	if m.hbCalls <= m.hbFails {
		return fmt.Errorf("api down")
	}
	m.heartbeats = append(m.heartbeats, heartbeat{interactive: live, ssh: sshSessions, id: id})
	return nil
}

func (m *mockAPI) PullConfig(_ context.Context, cursor string) (*api.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pulls = append(m.pulls, cursor)
	if len(m.script) == 0 {
		return nil, api.ErrNotModified
	}
	r := m.script[0]
	m.script = m.script[1:]
	return r.cfg, r.err
}

type recordingReconciler struct {
	mu   sync.Mutex
	sets [][]api.Peer
}

func (r *recordingReconciler) Reconcile(peers []api.Peer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets = append(r.sets, peers)
	return nil
}

func fastSupervisor(m *mockAPI, rec Reconciler) *Supervisor {
	s := &Supervisor{API: m, Reconcile: rec}
	s.Identity = api.Identity{
		SSHHost:       "fd5e:de7b::1",
		WgPubkey:      "WGPUB",
		SSHHostPubkey: "ssh-ed25519 HOST",
	}
	s.SSHSessions = func() int { return 1 } // raw open ssh conns (rides ssh_sessions)
	// A held+attached session ⇒ interactive_live (the new session-derived axis;
	// raw ssh liveness is separate and folded server-side).
	s.AttachedClients = func() int { return 1 }
	s.HeldLivePTYs = func() int { return 1 }
	s.HeartbeatInterval = 10 * time.Millisecond
	s.PollFloor = time.Millisecond
	s.BackoffMin = 2 * time.Millisecond
	s.BackoffMax = 8 * time.Millisecond
	return s
}

func TestFullLoop(t *testing.T) {
	m := &mockAPI{script: []pullResult{
		{cfg: &api.Config{Cursor: "h:1", Peers: []api.Peer{{LaptopWgPubkey: "A", LaptopWgIP: "fd::a"}}}},
		{err: api.ErrNotModified},
		{cfg: &api.Config{Cursor: "h:2", Peers: nil}}, // revoke-all
	}}
	rec := &recordingReconciler{}
	s := fastSupervisor(m, rec)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.heartbeats) == 0 || !m.heartbeats[0].interactive {
		t.Fatalf("heartbeats: %v", m.heartbeats)
	}
	// every heartbeat re-asserts the identity (it IS the readiness signal).
	if m.heartbeats[0].id.WgPubkey != "WGPUB" {
		t.Fatalf("heartbeat missing identity: %+v", m.heartbeats[0].id)
	}
	// cursor threading: "" → h:1 → h:1 (after 304) → h:2 → …
	if len(m.pulls) < 4 || m.pulls[0] != "" || m.pulls[1] != "h:1" || m.pulls[2] != "h:1" || m.pulls[3] != "h:2" {
		t.Fatalf("cursor threading: %v", m.pulls)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sets) < 2 || len(rec.sets[0]) != 1 || len(rec.sets[1]) != 0 {
		t.Fatalf("reconciles: %v", rec.sets)
	}
}

func TestHeartbeatRefreshesLivePeers(t *testing.T) {
	// The broker-discovery file must be re-pruned on the heartbeat cadence (the
	// pull loop only rewrites on a peer-set change), so RefreshLivePeers — when set
	// — rides every heartbeat alongside SyncSessions.
	m := &mockAPI{}
	s := fastSupervisor(m, &recordingReconciler{})
	var refreshes atomic.Int32
	s.RefreshLivePeers = func() { refreshes.Add(1) }

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	if refreshes.Load() == 0 {
		t.Fatal("RefreshLivePeers never invoked on the heartbeat tick")
	}
}

func TestHeartbeatReportsIdentity(t *testing.T) {
	m := &mockAPI{}
	s := fastSupervisor(m, &recordingReconciler{})
	s.Identity.ResolvedCommit = "deadbeefcafe"

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.heartbeats) == 0 {
		t.Fatal("no heartbeats")
	}
	hb := m.heartbeats[0].id
	if hb.ResolvedCommit != "deadbeefcafe" || hb.WgPubkey != "WGPUB" || hb.SSHHostPubkey != "ssh-ed25519 HOST" {
		t.Fatalf("identity not asserted on heartbeat: %+v", hb)
	}
}

func TestHeartbeatReportsSSHSessions(t *testing.T) {
	// The raw open-ssh count rides the heartbeat as ssh_sessions (the api re-folds
	// it into liveness defensively). It is a SEPARATE axis from interactive_live,
	// which is now session-derived (attached clients / held-PTY keep-warm).
	m := &mockAPI{}
	s := fastSupervisor(m, &recordingReconciler{})
	s.SSHSessions = func() int { return 2 }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.heartbeats) == 0 {
		t.Fatal("no heartbeats")
	}
	if m.heartbeats[0].ssh != 2 {
		t.Fatalf("ssh_sessions not reported: %+v", m.heartbeats[0])
	}
}

func TestHeartbeatRawSSHDoesNotSetInteractiveLive(t *testing.T) {
	// New contract: an open ssh conn alone does NOT set interactive_live — that
	// flag is session-derived. With no sessions held, interactive_live is false
	// even with ssh_sessions > 0; the api folds ssh_sessions in defensively.
	m := &mockAPI{}
	s := fastSupervisor(m, &recordingReconciler{})
	s.SSHSessions = func() int { return 3 }
	s.AttachedClients = func() int { return 0 }
	s.HeldLivePTYs = func() int { return 0 }

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.heartbeats) == 0 {
		t.Fatal("no heartbeats")
	}
	if m.heartbeats[0].interactive {
		t.Fatalf("raw ssh must not set interactive_live: %+v", m.heartbeats[0])
	}
	if m.heartbeats[0].ssh != 3 {
		t.Fatalf("ssh_sessions = %d, want 3", m.heartbeats[0].ssh)
	}
}

func TestHeartbeatDefaultsSSHSessionsToZero(t *testing.T) {
	// No embedded SSH server wired (overlay-less boot) → reports 0, not nil-panic.
	m := &mockAPI{}
	s := fastSupervisor(m, &recordingReconciler{})
	s.SSHSessions = nil

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.heartbeats) == 0 || m.heartbeats[0].ssh != 0 {
		t.Fatalf("heartbeats: %+v", m.heartbeats)
	}
}

func TestHeartbeatFailureNeverBlocksPull(t *testing.T) {
	// The lesson: a failing heartbeat (now the readiness signal too) must not
	// gate config pull — they run on independent loops.
	m := &mockAPI{hbFails: 1000}
	rec := &recordingReconciler{}
	s := fastSupervisor(m, rec)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pulls) == 0 {
		t.Fatal("pull loop never ran while heartbeat was failing")
	}
}

func TestInteractiveLiveAttachedClients(t *testing.T) {
	s := &Supervisor{}
	s.defaults()
	s.AttachedClients = func() int { return 1 }
	s.HeldLivePTYs = func() int { return 1 }
	if !s.interactiveLive() {
		t.Fatal("attached client must be interactive-live")
	}
}

func TestInteractiveLiveHeldPTYWithinKeepWarm(t *testing.T) {
	s := &Supervisor{DetachedKeepWarm: time.Hour}
	s.defaults()
	s.AttachedClients = func() int { return 0 }
	s.HeldLivePTYs = func() int { return 1 }
	s.LastDetachAt = func() time.Time { return time.Now().Add(-1 * time.Minute) } // recent detach
	if !s.interactiveLive() {
		t.Fatal("held PTY within keep-warm window must be interactive-live")
	}
}

func TestInteractiveLiveHeldPTYPastKeepWarm(t *testing.T) {
	s := &Supervisor{DetachedKeepWarm: 30 * time.Minute}
	s.defaults()
	s.AttachedClients = func() int { return 0 }
	s.HeldLivePTYs = func() int { return 1 }
	s.LastDetachAt = func() time.Time { return time.Now().Add(-2 * time.Hour) } // long past
	if s.interactiveLive() {
		t.Fatal("held PTY past keep-warm window must NOT be interactive-live")
	}
}

func TestInteractiveLiveNoSessions(t *testing.T) {
	s := &Supervisor{}
	s.defaults()
	s.AttachedClients = func() int { return 0 }
	s.HeldLivePTYs = func() int { return 0 }
	if s.interactiveLive() {
		t.Fatal("no held PTYs must NOT be interactive-live")
	}
}

func TestInteractiveLiveNilAccessors(t *testing.T) {
	// A box with no session module wired contributes no session liveness.
	s := &Supervisor{}
	s.defaults()
	if s.interactiveLive() {
		t.Fatal("nil session accessors must yield not-live (raw-conn liveness rides ssh_sessions separately)")
	}
}

func TestSyncSessionsFiresOnHeartbeatCadence(t *testing.T) {
	m := &mockAPI{}
	s := fastSupervisor(m, &recordingReconciler{})
	var syncs int
	var mu sync.Mutex
	s.SyncSessions = func() { mu.Lock(); syncs++; mu.Unlock() }

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if syncs == 0 {
		t.Fatal("SyncSessions never fired on the heartbeat cadence")
	}
}

func TestPullErrorBacksOffThenRecovers(t *testing.T) {
	m := &mockAPI{script: []pullResult{
		{err: fmt.Errorf("boom")},
		{err: fmt.Errorf("boom")},
		{cfg: &api.Config{Cursor: "h:9", Peers: nil}},
	}}
	rec := &recordingReconciler{}
	s := fastSupervisor(m, rec)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sets) == 0 {
		t.Fatal("never recovered to a successful reconcile after errors")
	}
}
