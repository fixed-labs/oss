package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
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
	// calls records every API call IN ORDER ("close-idle" | "beat" | "pull").
	// thaw's whole correctness is an ordering property — the pool drop has to
	// land before the first request — so the order is what tests assert on.
	calls []string
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
	m.calls = append(m.calls, "beat")
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
	m.calls = append(m.calls, "pull")
	if len(m.script) == 0 {
		return nil, api.ErrNotModified
	}
	r := m.script[0]
	m.script = m.script[1:]
	return r.cfg, r.err
}

func (m *mockAPI) CloseIdleConnections() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "close-idle")
}

// callsSnapshot copies the ordered call log under the lock — the supervisor's
// loops run on their own goroutines, so reading m.calls bare would race.
func (m *mockAPI) callsSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
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

// --- Lever 1 (§4.1) — sendHeartbeat retry helpers -----------------------------
//
// The retry loop in sendHeartbeat bounds itself against a deadline captured ONCE
// at entry (deadline := s.now().Add(s.HeartbeatInterval)) and re-reads s.now()
// each iteration to decide whether the next backoff would cross it. The tests
// below inject s.now directly (an unexported field, set in-package as thaw_test.go
// does) to make the deadline deterministic:
//
//   - a FROZEN now (retry-then-succeed, ctx-cancel): now never advances, so the
//     deadline never trips early — the loop is bounded only by the mock finally
//     succeeding, or by ctx cancellation. HeartbeatInterval is sized so K+1
//     attempts' worth of backoff never crosses it.
//   - a STEPPING now (exhaustion): now advances a fixed step on every read, so the
//     s.now()-relative deadline is eventually crossed — a frozen clock here would
//     loop forever because the deadline is never reached.

// frozenClock returns the same wall time on every read.
func frozenClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// steppingClock advances by step on every read (thread-safe). The FIRST read
// (sendHeartbeat's deadline capture) returns base; each subsequent read is
// base + n*step, so the s.now()-relative deadline is eventually crossed.
type steppingClock struct {
	mu   sync.Mutex
	cur  time.Time
	step time.Duration
}

func newSteppingClock(base time.Time, step time.Duration) *steppingClock {
	return &steppingClock{cur: base, step: step}
}

func (c *steppingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.cur
	c.cur = c.cur.Add(c.step)
	return t
}

// bufferLogger returns a slog.Logger writing to a buffer plus the buffer, so a
// test can assert on the exact message keys emitted (the §4.4 observability
// contract keys recurrence alerting on "heartbeat retries exhausted"). The mutex
// guards concurrent Handle calls; countMsg reads under the same lock.
type bufferLogger struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newBufferLogger() (*slog.Logger, *bufferLogger) {
	bl := &bufferLogger{}
	h := slog.NewTextHandler(&syncWriter{bl: bl}, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), bl
}

type syncWriter struct{ bl *bufferLogger }

func (w *syncWriter) Write(p []byte) (int, error) {
	w.bl.mu.Lock()
	defer w.bl.mu.Unlock()
	return w.bl.buf.Write(p)
}

// countMsg counts how many log records carry msg=<want>. slog's TextHandler
// renders the message as msg="...", so we match that exact token to avoid
// substring collisions between the two distinct lines.
func (bl *bufferLogger) countMsg(want string) int {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	token := `msg="` + want + `"`
	n := 0
	for _, line := range strings.Split(bl.buf.String(), "\n") {
		if strings.Contains(line, token) {
			n++
		}
	}
	return n
}

// discardLogger is a no-op logger for tests that don't assert on log output.
// beat()/sendHeartbeat call s.Log.Warn unconditionally, and calling beat()
// directly (not via Run) skips defaults(), which would otherwise install
// slog.Default() — so a nil s.Log would panic.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// retrySupervisor builds a Supervisor wired for the sendHeartbeat-retry tests:
// the given mock, ms-scale backoff, and a caller-supplied now clock. No session
// accessors beyond fastSupervisor's, so beat()'s piggybacks are observable. A nil
// log gets a discarding logger (beat() is called directly, bypassing defaults()).
func retrySupervisor(m *mockAPI, now func() time.Time, log *slog.Logger) *Supervisor {
	s := fastSupervisor(m, &recordingReconciler{})
	s.now = now
	if log != nil {
		s.Log = log
	} else {
		s.Log = discardLogger()
	}
	return s
}

// T4 — sendHeartbeat retries a transient failure and lands within the interval.
// The mock fails K times then succeeds; a FROZEN now keeps the entry-captured
// deadline out of reach (HeartbeatInterval ≫ the ms-scale backoff budget), so the
// loop is bounded only by the mock finally succeeding. Asserts Heartbeat was
// called exactly K+1 times (retried, not once-and-give-up) AND that the beat's
// piggyback (SyncSessions) fired EXACTLY once — the retry loop must not multiply
// it (it lives in beat(), after sendHeartbeat returns).
func TestSendHeartbeatRetriesThenSucceeds(t *testing.T) {
	const K = 3
	m := &mockAPI{hbFails: K}
	// Frozen clock: deadline = now + HeartbeatInterval is fixed and, with a 1s
	// interval vs a ~1+2+4ms backoff budget, never crossed — so exhaustion can't
	// pre-empt the K retries.
	s := retrySupervisor(m, frozenClock(time.Now()), nil)
	s.HeartbeatInterval = time.Second

	var syncs atomic.Int32
	s.SyncSessions = func() { syncs.Add(1) }

	// Call beat() directly (not Run) so we observe exactly ONE beat.
	s.beat(context.Background())

	m.mu.Lock()
	got := m.hbCalls
	landed := len(m.heartbeats)
	m.mu.Unlock()
	if got != K+1 {
		t.Fatalf("Heartbeat calls = %d, want %d (K=%d fails then success)", got, K+1, K)
	}
	if landed != 1 {
		t.Fatalf("landed heartbeats = %d, want 1 (only the successful attempt records)", landed)
	}
	if s := syncs.Load(); s != 1 {
		t.Fatalf("SyncSessions fired %d times, want exactly 1 (retry must not multiply piggybacks)", s)
	}
}

// T5(a) — sendHeartbeat bounds retries at the deadline and logs exhaustion
// distinctly. The mock ALWAYS errors; a STEPPING now advances every read so the
// s.now()-relative deadline is eventually crossed (a frozen clock would loop
// forever). Asserts: exactly one "heartbeat retries exhausted" record; "heartbeat
// failed" appears once per attempt and is a DISTINCT message key from the
// exhaustion line; the call returns (no hang); attempt count is bounded by the
// deadline budget.
func TestSendHeartbeatExhaustionLogsDistinctly(t *testing.T) {
	m := &mockAPI{hbFails: 1 << 30} // always fails
	log, bl := newBufferLogger()
	// Step the clock by BackoffMin each read so the deadline is reached after a
	// bounded number of iterations. HeartbeatInterval sized to admit several
	// attempts (each iteration reads now() at least once → advances the step).
	step := 2 * time.Millisecond
	s := retrySupervisor(m, newSteppingClock(time.Now(), step).now, log)
	s.HeartbeatInterval = 40 * time.Millisecond // ~ a handful of stepped reads

	done := make(chan struct{})
	go func() {
		s.sendHeartbeat(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sendHeartbeat hung (deadline never tripped) — exhaustion not bounded")
	}

	exhausted := bl.countMsg("heartbeat retries exhausted")
	if exhausted != 1 {
		t.Fatalf(`"heartbeat retries exhausted" logged %d times, want exactly 1`, exhausted)
	}
	failed := bl.countMsg("heartbeat failed")
	m.mu.Lock()
	attempts := m.hbCalls
	m.mu.Unlock()
	// Every attempt logs one "heartbeat failed" (the mock always errors), so the
	// count matches the attempt count — proving the two lines are DISTINCT keys
	// (not collapsed into one) and that "failed" fires per attempt.
	if failed != attempts {
		t.Fatalf(`"heartbeat failed" logged %d times, want %d (once per attempt)`, failed, attempts)
	}
	if failed == exhausted {
		t.Fatalf("the per-attempt and exhaustion lines must be distinct keys (failed=%d exhausted=%d)", failed, exhausted)
	}
	// Bounded: the deadline budget caps attempts well under a runaway loop.
	if attempts < 1 || attempts > 100 {
		t.Fatalf("attempts = %d, expected a small deadline-bounded count", attempts)
	}
}

// T5(b) — a ctx-cancelled beat returns promptly and emits ZERO "heartbeat retries
// exhausted" records (the exhaustion line is a genuine recurrence signal, never
// shutdown noise — §4.4). The mock always errors; a FROZEN now held BELOW the
// deadline keeps the deadline-cross check (which sendHeartbeat evaluates BEFORE
// the select on ctx.Done()) from firing, so the cancel — not exhaustion — is what
// ends the loop.
func TestSendHeartbeatCtxCancelNoExhaustionLog(t *testing.T) {
	m := &mockAPI{hbFails: 1 << 30} // always fails
	log, bl := newBufferLogger()
	// Frozen now + a large interval ⇒ the deadline is never approached, so the
	// only way out of the loop is ctx.Done() during the backoff sleep.
	s := retrySupervisor(m, frozenClock(time.Now()), log)
	s.HeartbeatInterval = time.Hour
	// Larger min backoff so the first attempt reliably parks in the select on
	// ctx.Done() (rather than racing a sub-ms cancel).
	s.BackoffMin = 20 * time.Millisecond
	s.BackoffMax = 40 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.sendHeartbeat(ctx)
		close(done)
	}()
	// Let the first attempt fail and enter the backoff sleep, then cancel.
	time.Sleep(5 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sendHeartbeat did not return promptly on ctx cancel")
	}

	if got := bl.countMsg("heartbeat retries exhausted"); got != 0 {
		t.Fatalf(`ctx-cancelled beat emitted %d "heartbeat retries exhausted" records, want 0 (no shutdown noise)`, got)
	}
}
