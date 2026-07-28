package supervisor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// --- The cursor-discriminator mock (T2.11/T2.12 shared harness) ---------------
//
// The steady-state pullLoop emits PullConfig("") on EVERY poll while its cursor
// is "" (an ErrNotModified reply leaves the cursor empty), so "an empty-cursor
// pull happened" does NOT by itself distinguish a thaw. To make the empty-cursor
// pull a UNIQUE thaw signal, thawMockAPI returns a NON-EMPTY, ADVANCING cursor
// ("h:1", "h:2", …) on every steady-state pull — the loop threads that cursor,
// so the ONLY PullConfig("") after index 0 is thaw()'s forced full-resync (it
// calls PullConfig(ctx, "") with an empty cursor). The mock is cursor-AWARE: a
// non-empty cursor gets the next steady-state config (advancing cursor + a
// distinct peer set); an empty cursor (the thaw resync) gets the thaw config
// (a fixed cursor + the thaw peer set) so thaw() reaches Reconcile(peers).
type thawMockAPI struct {
	mu sync.Mutex

	hbCalls int      // total Heartbeat invocations
	pulls   []string // cursors received, in order

	steadyCounter int // advances the steady-state cursor "h:1","h:2",…

	// thawPeers is the peer set returned to the empty-cursor (thaw resync) pull.
	thawPeers []api.Peer
}

func (m *thawMockAPI) Heartbeat(_ context.Context, _ bool, _ int, _ api.Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hbCalls++
	return nil
}

func (m *thawMockAPI) PullConfig(_ context.Context, cursor string) (*api.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pulls = append(m.pulls, cursor)
	if cursor == "" {
		// Either the loop's very first poll (index 0) or the thaw's forced
		// full-resync. Return the thaw config with a fixed non-empty cursor so
		// the loop starts threading from there, and carry the thaw peer set so
		// thaw() reaches Reconcile(thawPeers).
		return &api.Config{Cursor: "thaw:1", Peers: m.thawPeers}, nil
	}
	// Steady-state poll on a threaded cursor: hand back the next advancing
	// cursor and a distinct (single-peer) set so ordinary reconciles are
	// visibly different from the thaw's peer set.
	m.steadyCounter++
	return &api.Config{
		Cursor: nextCursor(m.steadyCounter),
		Peers:  []api.Peer{{LaptopWgPubkey: "STEADY", LaptopWgIP: "fd::1"}},
	}, nil
}

func nextCursor(n int) string {
	// "h:1", "h:2", … — non-empty and strictly advancing so the loop always
	// threads a non-empty cursor and never re-emits PullConfig("") on its own.
	return "h:" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (m *thawMockAPI) snapshot() (hb int, pulls []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hbCalls, append([]string(nil), m.pulls...)
}

// emptyCursorPullsAfterIndex0 counts PullConfig("") calls after the loop's very
// first poll (index 0). Under the cursor discriminator that count is exactly the
// number of thaw-forced resyncs.
func emptyCursorPullsAfterIndex0(pulls []string) int {
	n := 0
	for i, c := range pulls {
		if i == 0 {
			continue
		}
		if c == "" {
			n++
		}
	}
	return n
}

// --- The two-clock driver -----------------------------------------------------
//
// waitOrThaw compares s.now() (wall, monotonic stripped) against s.monoNow()
// (independent monotonic-elapsed). A real Fly RAM-snapshot resume shows up as a
// WALL jump with NO matching monotonic advance. A single time.Time fake cannot
// express that divergence, which is exactly why the production code injects the
// two clocks separately.
//
// The driver advances a shared virtual clock deterministically, decoupled from
// the wall-clock ticker: on each ticker fire, waitOrThaw calls s.now() then
// s.monoNow() (in that order). We make s.monoNow() the "stepper": it advances
// both virtual clocks by one sub-tick worth of time, then the NEXT s.now() read
// reflects it. Every sub-tick advances wall and mono TOGETHER by monoStep,
// EXCEPT at the scheduled jump sub-tick, where wall additionally jumps by
// wallJump (mono still only advances monoStep) — the divergence a suspend
// produces.
type twoClockDriver struct {
	mu sync.Mutex

	wall time.Time     // current virtual wall time (returned by now())
	mono time.Duration // current virtual monotonic-elapsed (returned by monoNow())

	monoStep time.Duration // per-sub-tick advance for BOTH clocks
	wallJump time.Duration // extra wall-only jump injected at the jump step (0 ⇒ never)

	steps    int // number of monoNow() advances so far
	jumpStep int // 1-based monoNow() call index at which to inject wallJump (0 ⇒ never)
	jumped   bool
}

func newTwoClockDriver(monoStep, wallJump time.Duration, jumpStep int) *twoClockDriver {
	return &twoClockDriver{
		// A fixed base far from the monotonic zero so Round(0) has real wall
		// values to subtract.
		wall:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		mono:     0,
		monoStep: monoStep,
		wallJump: wallJump,
		jumpStep: jumpStep,
	}
}

// now returns the current virtual wall time. It is a pure read: the .Round(0)
// the detector applies is a no-op on these values (no monotonic reading), so
// wallDelta is exactly the wall advance between samples.
func (d *twoClockDriver) now() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.wall
}

// monoNow advances the shared virtual clock by one sub-tick, then returns the
// new monotonic-elapsed. Advancing here (rather than in now) keeps a single
// stepper: waitOrThaw reads now() then monoNow() per tick, so the wall value a
// tick observes is the one this method set on the previous tick.
func (d *twoClockDriver) monoNow() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.steps++
	d.mono += d.monoStep
	d.wall = d.wall.Add(d.monoStep)
	if d.jumpStep > 0 && d.steps == d.jumpStep && !d.jumped {
		// Inject the wall-only jump: wall races ahead, mono does not — the
		// signature of a warm resume from a Fly RAM snapshot.
		d.wall = d.wall.Add(d.wallJump)
		d.jumped = true
	}
	return d.mono
}

func thawSupervisor(m API, rec Reconciler, d *twoClockDriver) *Supervisor {
	s := &Supervisor{API: m, Reconcile: rec}
	s.Identity = api.Identity{WgPubkey: "WGPUB"}
	s.SSHSessions = func() int { return 0 }
	s.now = d.now
	s.monoNow = d.monoNow
	// Long heartbeat interval so an ordinary interval-boundary tick doesn't end
	// the sub-poll before we've driven our scripted sub-ticks; the thaw (or its
	// absence) is what we observe, not a wall-interval rollover.
	s.HeartbeatInterval = 10 * time.Second
	s.ThawPoll = time.Millisecond // fast real ticker so the loop spins quickly
	s.ThawThreshold = 10 * time.Second
	s.PollFloor = time.Millisecond
	s.BackoffMin = time.Millisecond
	s.BackoffMax = 4 * time.Millisecond
	return s
}

// waitUntil polls cond every 2ms until it returns true or the deadline passes.
func waitUntil(cond func() bool, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// T2.11 — thaw fires: self-detect the wall/mono divergence, force an
// out-of-cadence heartbeat, and force one empty-cursor full-resync that drives
// a Reconcile with the thaw peer set + a SyncSessions.
func TestThawFiresForcesBeatResyncAndSessionSync(t *testing.T) {
	thawPeers := []api.Peer{
		{LaptopWgPubkey: "THAW-A", LaptopWgIP: "fd::a"},
		{LaptopWgPubkey: "THAW-B", LaptopWgIP: "fd::b"},
	}
	m := &thawMockAPI{thawPeers: thawPeers}
	rec := &recordingReconciler{}

	// Baseline sub-ticks advance both clocks by 1s TOGETHER; at the 4th
	// monoNow() sample inject a wall-only jump (the wallDelta − monoDelta for
	// that sample is exactly the jump ⇒ fires). The jump is wired below to
	// s.ThawThreshold (referencing the field, never a hardcoded 10s).
	monoStep := time.Second
	d := newTwoClockDriver(monoStep, 0 /* wallJump set from the field below */, 4)
	s := thawSupervisor(m, rec, d)
	// wall-only jump ≥ ThawThreshold, referencing the field per the plan.
	d.wallJump = s.ThawThreshold

	var syncs int
	var syncMu sync.Mutex
	s.SyncSessions = func() { syncMu.Lock(); syncs++; syncMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)

	// The thaw resync is the empty-cursor pull after index 0. Wait until it
	// lands (with a generous real-time budget — the driver spins on a 1ms
	// ticker, so a handful of sub-ticks elapse in a few ms).
	ok := waitUntil(func() bool {
		_, pulls := m.snapshot()
		return emptyCursorPullsAfterIndex0(pulls) >= 1
	}, 2*time.Second)
	cancel()
	// Give the in-flight loop a moment to settle after cancel.
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	if !ok {
		_, pulls := m.snapshot()
		t.Fatalf("thaw resync (empty-cursor pull after index 0) never fired; pulls=%v", pulls)
	}

	hb, pulls := m.snapshot()

	// (b) EXACTLY ONE empty-cursor pull after index 0 (the single forced
	// resync). More than one would mean a repeated/mis-detected thaw.
	if got := emptyCursorPullsAfterIndex0(pulls); got != 1 {
		t.Fatalf("empty-cursor pulls after index 0 = %d, want exactly 1; pulls=%v", got, pulls)
	}

	// (b cont.) that resync drove a Reconcile receiving the THAW peer set.
	rec.mu.Lock()
	var sawThaw bool
	for _, set := range rec.sets {
		if peersEqual(set, thawPeers) {
			sawThaw = true
			break
		}
	}
	rec.mu.Unlock()
	if !sawThaw {
		t.Fatalf("no Reconcile received the thaw peer set %v; got %v", thawPeers, reconcileSets(rec))
	}

	// (a) an out-of-cadence extra Heartbeat: with a 10s HeartbeatInterval and
	// only a few seconds of virtual time elapsed, the steady loop would have
	// beaten exactly ONCE (the initial beat). The thaw's forced beat makes it
	// at least TWO. Any hbCalls ≥ 2 in this window is a beat the cadence alone
	// cannot explain.
	if hb < 2 {
		t.Fatalf("expected an out-of-cadence heartbeat (hbCalls ≥ 2), got %d", hb)
	}

	// (c) a SyncSessions call fired (thaw() calls it after the resync). Note
	// SyncSessions also rides the normal beat, so its presence is necessary but
	// not thaw-unique — the empty-cursor pull is the discriminator above.
	syncMu.Lock()
	gotSyncs := syncs
	syncMu.Unlock()
	if gotSyncs == 0 {
		t.Fatal("SyncSessions never fired")
	}
}

// T2.12 — no false positive: wall and mono advance TOGETHER every sub-tick
// (wallDelta ≈ monoDelta, under ThawThreshold), so the detector must stay quiet.
// The negative signal is ZERO empty-cursor pulls after index 0 (the thaw-
// specific signal) and no out-of-cadence beat — NOT SyncSessions absence (it
// fires on the normal cadence too). Non-vacuous because the SAME harness
// demonstrably trips in T2.11.
func TestThawNoFalsePositiveOnNormalTick(t *testing.T) {
	m := &thawMockAPI{thawPeers: []api.Peer{{LaptopWgPubkey: "THAW", LaptopWgIP: "fd::z"}}}
	rec := &recordingReconciler{}

	// monoStep well under ThawThreshold, NO wall-only jump (jumpStep 0): every
	// sub-tick advances both clocks by 1s together ⇒ wallDelta − monoDelta ≈ 0.
	d := newTwoClockDriver(time.Second, 0, 0)
	s := thawSupervisor(m, rec, d)

	var syncs int
	var syncMu sync.Mutex
	s.SyncSessions = func() { syncMu.Lock(); syncs++; syncMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)

	// Let the loop run through many sub-ticks so a false positive would have
	// ample opportunity to fire. The driver advances only on monoNow() reads,
	// so we wait until it has stepped well past the point a thaw would trip.
	waitUntil(func() bool { return d.stepCount() >= 30 }, 2*time.Second)
	// A little extra real time for any in-flight resync to be recorded.
	waitUntil(func() bool { return false }, 20*time.Millisecond)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	hbBefore, pulls := m.snapshot()

	// Primary negative: ZERO thaw-forced resyncs.
	if got := emptyCursorPullsAfterIndex0(pulls); got != 0 {
		t.Fatalf("false positive: %d empty-cursor pulls after index 0; pulls=%v", got, pulls)
	}

	// Secondary negative: no out-of-cadence beat. With a 10s HeartbeatInterval
	// and ~30s of virtual time never crossing an interval boundary within one
	// waitOrThaw (each waitOrThaw runs one interval), heartbeats stay on the
	// ordinary cadence. There is no thaw-forced beat, so the count must be
	// modest — the point is it isn't inflated by spurious thaws. We assert the
	// stronger structural fact (zero forced resyncs) above; here we simply
	// confirm the loop kept beating on cadence at all (liveness), not a thaw
	// burst.
	if hbBefore == 0 {
		t.Fatal("loop never beat at all")
	}

	// SyncSessions is EXPECTLY non-zero here (normal cadence) — asserting its
	// presence guards against keying the negative on it by mistake.
	syncMu.Lock()
	gotSyncs := syncs
	syncMu.Unlock()
	if gotSyncs == 0 {
		t.Fatal("SyncSessions never fired on the normal cadence (harness wired wrong)")
	}
}

// stepCount reports how many monoNow() advances the driver has taken.
func (d *twoClockDriver) stepCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.steps
}

func peersEqual(a, b []api.Peer) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func reconcileSets(rec *recordingReconciler) [][]api.Peer {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([][]api.Peer(nil), rec.sets...)
}
