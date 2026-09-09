package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// --- The thaw-discriminator mock (shared harness) -----------------------------
//
// A thaw is no longer identifiable by a pull it makes, because it makes none:
// cancelling the in-flight poll IS the kick, and the fresh peer set is fetched
// and reconciled by pullLoop alone (see thaw). What marks a thaw uniquely is
// CloseIdleConnections — thaw is its only caller in the agent, and the
// steady-state loops must never make it (that would pay a fresh TCP+TLS
// handshake on every beat forever, the cost DisableKeepAlives was rejected for).
// TestSteadyStateLoopNeverDropsIdlePool pins that other half, so counting
// "close-idle" entries in the ordered call log IS the thaw count.
//
// The mock still hands back a NON-EMPTY, ADVANCING cursor ("h:1", "h:2", …) on
// every poll, and the loop threads it. That keeps the OLD discriminator alive as
// a negative: after index 0 (the loop's own first poll) a PullConfig("") can
// only be a full-resync somebody performed out of band, and there must never be
// one again — see emptyCursorPullsAfterIndex0.
type thawMockAPI struct {
	mu sync.Mutex

	hbCalls int         // total Heartbeat invocations
	hbFails int         // fail the first N heartbeats (0 ⇒ never) — exercises beat retry
	hbAt    []time.Time // when each Heartbeat attempt landed (retry-spacing tests)
	pulls   []string    // cursors received, in order

	// calls is every API call IN ORDER ("close-idle" | "beat" | "pull"). thaw's
	// pool drop is correct only if it lands BEFORE the forced beat, and ordering
	// is not observable from the per-call counters.
	calls []string

	steadyCounter int // advances the steady-state cursor "h:1","h:2",…
}

func (m *thawMockAPI) Heartbeat(_ context.Context, _ bool, _ int, _ api.Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hbCalls++
	m.hbAt = append(m.hbAt, time.Now())
	m.calls = append(m.calls, "beat")
	if m.hbCalls <= m.hbFails {
		return fmt.Errorf("api down")
	}
	return nil
}

func (m *thawMockAPI) CloseIdleConnections() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "close-idle")
}

func (m *thawMockAPI) PullConfig(_ context.Context, cursor string) (*api.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pulls = append(m.pulls, cursor)
	m.calls = append(m.calls, "pull")
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

func (m *thawMockAPI) callsSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func (m *thawMockAPI) heartbeatTimes() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Time(nil), m.hbAt...)
}

// thaws counts the thaws the supervisor performed. thaw is the agent's ONLY
// caller of CloseIdleConnections (TestSteadyStateLoopNeverDropsIdlePool pins the
// converse), so the count of "close-idle" entries is the count of thaws.
func (m *thawMockAPI) thaws() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c == "close-idle" {
			n++
		}
	}
	return n
}

// emptyCursorPullsAfterIndex0 counts PullConfig("") calls after the loop's very
// first poll (index 0). The mock threads a non-empty advancing cursor, so the
// loop itself can never produce one; anything counted here is a full-resync
// performed out of band. Since FIX-354 that count must always be ZERO — thaw's
// own PullConfig("") + Reconcile is exactly what was deleted (see thaw for the
// unsynchronised-full-wipe race it caused).
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

// --- The fake resumeWatch -----------------------------------------------------
//
// Detection now hangs off a CLOCK_REALTIME step, and stepping that clock needs
// CAP_SYS_TIME — which no test runner can assume. So the loop-level tests drive
// the seam instead: fakeWatch scripts the exact wake sequence heartbeatLoop will
// observe. The real timerfd is covered separately in resumewatch_linux_test.go.
type fakeWatch struct {
	mu sync.Mutex

	wakes  []wake // scripted, consumed in order; the last one repeats forever
	idx    int
	arms   []time.Time // every Arm deadline, in order
	armErr error       // non-nil ⇒ every Arm fails (drives the not-armed path)
	events []string    // interleaved "arm"/"wait"/"beat" trace — proves ORDERING

	// armSteps schedules Arm's `stepped` report by arm index: armSteps[n] == true
	// ⇒ the (n+1)-th Arm reports a step. That is the channel a resume takes when
	// it lands while the loop is BUSY rather than blocked in Wait.
	armSteps map[int]bool
	// alwaysStepOnArm makes every Arm report a step — a clock being stepped
	// continuously, the worst case for thaw rate.
	alwaysStepOnArm bool

	waitGap time.Duration // a small real pause per Wait, so a deadline script
	// cannot spin faster than the test can observe
}

func (f *fakeWatch) Arm(deadline time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.arms)
	f.arms = append(f.arms, deadline)
	f.events = append(f.events, "arm")
	if f.armErr != nil {
		return false, f.armErr
	}
	return f.alwaysStepOnArm || f.armSteps[n], nil
}

func (f *fakeWatch) Wait(ctx context.Context) wake {
	f.mu.Lock()
	f.events = append(f.events, "wait")
	var w wake
	switch {
	case f.idx < len(f.wakes):
		w = f.wakes[f.idx]
		f.idx++
	case len(f.wakes) > 0:
		w = f.wakes[len(f.wakes)-1]
	default:
		w = wakeDeadline
	}
	gap := f.waitGap
	f.mu.Unlock()

	if gap > 0 {
		select {
		case <-ctx.Done():
			return wakeCancelled
		case <-time.After(gap):
		}
	}
	if ctx.Err() != nil {
		return wakeCancelled
	}
	return w
}

func (f *fakeWatch) Close() error { return nil }

func (f *fakeWatch) trace() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeWatch) armCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.arms)
}

// steppedClock fakes the wall-vs-monotonic divergence a Fly RAM-snapshot resume
// leaves behind. `add` is what a suspend of that length looks like to the loop.
//
// It exists for the same reason fakeWatch does: real divergence needs CAP_SYS_TIME
// to produce, and Go cannot synthesise a time.Time whose monotonic reading moves
// independently of its wall reading. Since FIX-292 that figure GATES the thaw, so
// a test that cannot move it cannot reach either side of the gate.
type steppedClock struct {
	mu    sync.Mutex
	total time.Duration
}

func (c *steppedClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += d
}

func (c *steppedClock) divergence(time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func thawSupervisor(m API, rec Reconciler, w resumeWatch) *Supervisor {
	s := &Supervisor{API: m, Reconcile: rec}
	s.Log = discardLogger()
	s.Identity = api.Identity{WgPubkey: "WGPUB"}
	s.SSHSessions = func() int { return 0 }
	s.HeartbeatInterval = 10 * time.Second
	s.PollFloor = time.Millisecond
	s.BackoffMin = time.Millisecond
	s.BackoffMax = 4 * time.Millisecond
	s.newWatch = func() resumeWatch { return w }
	// These tests predate FIX-292's magnitude gate and are about what the loop DOES
	// with a step, not about telling a resume from a bare clock set. Hand them a
	// clock that looks like a fresh suspend at every reading, so every scripted step
	// still qualifies as a resume. The gate itself is covered by the tests that
	// install their own steppedClock.
	var auto atomic.Int64
	s.divergence = func(time.Time) time.Duration {
		return time.Duration(auto.Add(int64(time.Minute)))
	}
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

// T5 — a wakeClockStep drives the full thaw: exactly one pool drop, an
// out-of-cadence heartbeat, and a SyncSessions.
//
// Updated for FIX-354. The old form asserted a thaw-forced empty-cursor resync
// that reached Reconcile with a distinct peer set; that behaviour is gone on
// purpose (thaw's own pull + reconcile raced pullLoop's — see thaw), so the
// discriminator moved to the pool drop and the resync is now asserted ABSENT.
// The kick that replaces it is proved in pullcancel_test.go, which needs a mock
// whose poll actually blocks.
func TestHeartbeatLoopThawsOnClockStepWake(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	// One step, then ordinary cadence forever.
	w := &fakeWatch{wakes: []wake{wakeClockStep, wakeDeadline}, waitGap: 5 * time.Millisecond}
	s := thawSupervisor(m, rec, w)

	var syncs int
	var syncMu sync.Mutex
	s.SyncSessions = func() { syncMu.Lock(); syncs++; syncMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)

	ok := waitUntil(func() bool { return m.thaws() >= 1 }, 2*time.Second)
	// The thaw's session sync is fired asynchronously, so observe it before
	// shutting the loop down (waiting on it is also what keeps the goroutine from
	// outliving the test).
	syncSeen := waitUntil(func() bool {
		syncMu.Lock()
		defer syncMu.Unlock()
		return syncs > 0
	}, time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	if !ok {
		t.Fatalf("thaw never fired; calls=%v", m.callsSnapshot())
	}

	hb, pulls := m.snapshot()

	// Exactly ONE thaw — more would mean a repeated/mis-detected step.
	if got := m.thaws(); got != 1 {
		t.Fatalf("thaws = %d, want exactly 1; calls=%v", got, m.callsSnapshot())
	}

	// And the thaw did NOT resync on its own: no out-of-band empty-cursor pull.
	if got := emptyCursorPullsAfterIndex0(pulls); got != 0 {
		t.Fatalf("thaw performed %d full-resync pull(s) of its own; pulls=%v — that "+
			"reconcile races pullLoop's with nothing synchronising them, and the loser "+
			"wipes the laptop's peer", got, pulls)
	}

	// An out-of-cadence beat: with a 10s HeartbeatInterval, cadence alone
	// explains exactly one beat in this window. thaw()'s forced beat makes >= 2.
	if hb < 2 {
		t.Fatalf("expected an out-of-cadence heartbeat (hbCalls >= 2), got %d", hb)
	}
	if !syncSeen {
		t.Fatal("SyncSessions never fired")
	}
}

// T6 — no thaw without a step. Only wakeDeadline is ever returned, so the loop
// must beat on cadence and never thaw. The negative signal is ZERO pool drops —
// NOT SyncSessions absence (it rides the normal beat too). Non-vacuous because
// the same harness trips in T5.
func TestHeartbeatLoopNoThawOnDeadlineWake(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	w := &fakeWatch{wakes: []wake{wakeDeadline}, waitGap: 2 * time.Millisecond}
	s := thawSupervisor(m, rec, w)

	var syncs int
	var syncMu sync.Mutex
	s.SyncSessions = func() { syncMu.Lock(); syncs++; syncMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)

	// Let many cadence ticks go by so a false positive would have ample chance.
	waitUntil(func() bool { return w.armCount() >= 20 }, 2*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	hb, _ := m.snapshot()

	if got := m.thaws(); got != 0 {
		t.Fatalf("false positive: %d thaw(s) with no clock step; calls=%v", got, m.callsSnapshot())
	}
	if hb == 0 {
		t.Fatal("loop never beat at all")
	}
	syncMu.Lock()
	gotSyncs := syncs
	syncMu.Unlock()
	if gotSyncs == 0 {
		t.Fatal("SyncSessions never fired on the normal cadence (harness wired wrong)")
	}
}

// T7 — the watch is PRIMED before the very first beat. Until the first Arm the
// kernel does not mark the fd on a clock step at all, so a resume landing during
// that first beat would be invisible to BOTH detection channels.
func TestHeartbeatLoopPrimesWatchBeforeFirstBeat(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	w := &fakeWatch{wakes: []wake{wakeDeadline}, waitGap: 5 * time.Millisecond}
	s := thawSupervisor(m, rec, w)

	// SSHSessions is read inside sendHeartbeat, so it samples the arm count at
	// exactly the moment the first beat is being built.
	armsBeforeFirstBeat := -1
	var once sync.Once
	s.SSHSessions = func() int {
		once.Do(func() { armsBeforeFirstBeat = w.armCount() })
		return 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	waitUntil(func() bool { return w.armCount() >= 3 }, 2*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	if armsBeforeFirstBeat < 1 {
		t.Fatalf("the watch was not primed before the first beat (arms seen at beat "+
			"time = %d) — a resume landing during that beat would be missed by both "+
			"detection channels", armsBeforeFirstBeat)
	}
	if tr := w.trace(); len(tr) == 0 || tr[0] != "arm" {
		t.Fatalf("first watch event = %v, want an arm first; trace=%v", tr, tr)
	}
}

// T7b — the SECOND detection channel. A resume that lands while the loop is BUSY
// (mid-beat, mid-thaw) is never seen by Wait; the kernel latches it and reports it
// through the next timerfd_settime, which returns ECANCELED *and rearms anyway*.
// The loop must treat that as a resume.
//
// This is the case that matters most in production: the Fly suspend a resume
// undoes is triggered BY a heartbeat, so the step lands in the busy window by
// construction. An earlier draft of this design logged Arm's ECANCELED as an
// error and dropped it — which would have swallowed the step, fired the ERROR
// line reserved for a broken detector, and degraded a healthy watch.
//
// The loop must also NOT wait out an interval first: with a 10s HeartbeatInterval
// against a 2s budget here, a thaw deferred to the next wake would never show up.
func TestHeartbeatLoopThawsOnArmReportedStep(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	// arms[0] is the prime; arms[1] is the one after the first beat — report the
	// step there. Wait never reports a step, so a thaw can ONLY come from Arm.
	w := &fakeWatch{
		wakes:    []wake{wakeDeadline},
		armSteps: map[int]bool{1: true},
		waitGap:  5 * time.Millisecond,
	}
	s := thawSupervisor(m, rec, w)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	ok := waitUntil(func() bool { return m.thaws() >= 1 }, 2*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	if !ok {
		t.Fatalf("a step reported by Arm did not drive a thaw; calls=%v — a resume "+
			"landing during a beat would be lost", m.callsSnapshot())
	}
}

// T13 — the heartbeat interval is anchored at WORK-END, not work-start, so a beat
// that overruns is still followed by a full idle interval.
//
// Anchoring before the beat would shorten the effective period to
// max(interval, beatDuration): sendHeartbeat may spend a whole interval retrying,
// and one request is bounded only by the API client's 35s timeout — longer than
// the 30s interval — so a slow control plane would get back-to-back beats with
// zero idle, nearly doubling the request rate on exactly the recovering-API path
// the retry budget exists to protect.
//
// The probe: make one beat far slower than the interval. The arm that follows it
// must carry a deadline LATER than the moment that beat finished; an arm placed
// before the beat could not.
func TestHeartbeatLoopAnchorsIntervalAfterTheBeat(t *testing.T) {
	const beatDuration = 100 * time.Millisecond
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	w := &fakeWatch{wakes: []wake{wakeDeadline}, waitGap: 5 * time.Millisecond}
	s := thawSupervisor(m, rec, w)
	s.HeartbeatInterval = 20 * time.Millisecond // deliberately << beatDuration

	var beatEnd time.Time
	var beatMu sync.Mutex
	var once sync.Once
	s.SSHSessions = func() int {
		once.Do(func() {
			time.Sleep(beatDuration)
			beatMu.Lock()
			beatEnd = time.Now()
			beatMu.Unlock()
		})
		return 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	waitUntil(func() bool { return w.armCount() >= 2 }, 3*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	beatMu.Lock()
	end := beatEnd
	beatMu.Unlock()
	w.mu.Lock()
	arms := append([]time.Time(nil), w.arms...)
	w.mu.Unlock()

	if len(arms) < 2 || end.IsZero() {
		t.Fatalf("harness did not run (arms=%d, beatEnd recorded=%v)", len(arms), !end.IsZero())
	}
	// arms[0] is the prime (before the first beat); arms[1] follows that beat.
	if !arms[1].After(end) {
		t.Fatalf("the arm after the slow beat carries deadline %v, not after that beat's "+
			"end %v — the interval is anchored at work-START, so an overrunning beat is "+
			"followed immediately by another", arms[1], end)
	}
}

// T8 — the thaw-forced beat inherits the retry (warm-resume path). Because the
// retry lives INSIDE sendHeartbeat (thawBeat calls it directly), thaw()'s forced
// heartbeat retries a transient failure exactly as the steady loop's beat does.
// This guards against a future refactor hoisting the retry up into
// heartbeatLoop, which would silently drop retry on resume.
//
// The first retry waits thawFirstRetry rather than BackoffMin, so this test
// spends ~200ms in that first sleep; the ladder after it is the harness's
// ms-scale one. TestThawForcedBeatRetriesFastBeforeTheLadder is what pins the
// spacing itself.
//
// Injection: s.now is frozen, so sendHeartbeat's deadline (now + interval) is
// never approached and the K retries run to the mock's success.
func TestThawForcedBeatInheritsRetry(t *testing.T) {
	const K = 3
	m := &thawMockAPI{hbFails: K}
	rec := &recordingReconciler{}
	s := thawSupervisor(m, rec, &fakeWatch{})
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return frozen }

	// Drive the thaw hook directly: thaw() → thawBeat() → sendHeartbeat(), which
	// must retry the transient failures.
	s.thaw(context.Background())

	hb, _ := m.snapshot()
	if hb != K+1 {
		t.Fatalf("thaw-forced beat Heartbeat calls = %d, want %d (K=%d fails then success) — retry not inherited by thaw", hb, K+1, K)
	}
}

// T10a — INV-4, the arm arm: a watch whose Arm always fails must not stop the
// beat. The loop degrades to the plain timer and keeps its cadence. An earlier
// draft of this design left the loop blocked on an unarmed fd here — no further
// beats ever, which is strictly worse than the bug being fixed, because the
// heartbeat is the sole writer of the cluster's `running` flip.
func TestHeartbeatLoopKeepsBeatingWhenArmFails(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	w := &fakeWatch{armErr: errors.New("settime: bad fd"), wakes: []wake{wakeDeadline}}
	s := thawSupervisor(m, rec, w)
	// Short interval so the degraded plain timer fires quickly.
	s.HeartbeatInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	beat2 := waitUntil(func() bool { hb, _ := m.snapshot(); return hb >= 2 }, 2*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	if !beat2 {
		hb, _ := m.snapshot()
		t.Fatalf("loop stopped beating after an Arm failure (hbCalls=%d, want >= 2) — "+
			"a broken watch must degrade the agent, never wedge it", hb)
	}
}

// T10b — INV-4, the read arm: a watch whose Wait reports wakeBroken is swapped
// for the plain timer and the loop keeps beating. The danger this guards is a
// watch that fails INSTANTLY and forever: folding that into wakeDeadline would
// spin the loop into a tight beat-fail-beat loop against the API. After the swap
// the beats must be paced by HeartbeatInterval again.
func TestHeartbeatLoopDegradesOnBrokenWatch(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	w := &fakeWatch{wakes: []wake{wakeBroken}} // no gap: returns instantly, forever
	s := thawSupervisor(m, rec, w)
	s.HeartbeatInterval = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	ok := waitUntil(func() bool { hb, _ := m.snapshot(); return hb >= 2 }, 2*time.Second)
	// ~300ms at a 30ms cadence is ~10 beats; an un-degraded spin would be
	// thousands.
	waitUntil(func() bool { return false }, 300*time.Millisecond)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	hb, _ := m.snapshot()
	if !ok {
		t.Fatalf("loop stopped beating after a broken watch (hbCalls=%d)", hb)
	}
	if hb > 60 {
		t.Fatalf("hbCalls=%d — the loop is spinning, not degrading: a broken watch "+
			"must be replaced by the plain timer so beats stay paced by HeartbeatInterval", hb)
	}
}

// T14 — a clock stepped over and over must NOT spin the loop.
//
// CANCEL_ON_SET latches on any clock set anywhere in the box, and a devbox runs
// arbitrary user workloads (a suite that fakes time, an `ntpdate` loop). Each
// step drives a thaw, and a thaw is four requests. The pre-FIX-280 detector was
// rate-limited by accident — its 3s poll and 10s magnitude threshold both capped
// thaw frequency — and deleting those means the floor has to be explicit.
//
// Here EVERY Arm reports a step, i.e. the worst case. Thaws must be paced by
// thawSpacing rather than by network RTT.
func TestHeartbeatLoopDoesNotSpinOnRepeatedSteps(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	w := &fakeWatch{wakes: []wake{wakeDeadline}, alwaysStepOnArm: true}
	s := thawSupervisor(m, rec, w)
	s.thawSpacing = 40 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	// ~400ms at a 40ms floor allows ~10 thaws. Unpaced, this would run at RTT
	// against an in-process mock — thousands.
	waitUntil(func() bool { return false }, 400*time.Millisecond)
	cancel()
	waitUntil(func() bool { return false }, 30*time.Millisecond)

	thaws := m.thaws()
	if thaws == 0 {
		t.Fatal("no thaw at all — the harness never drove a step")
	}
	if thaws > 25 {
		t.Fatalf("thaws=%d in ~400ms at a %v floor — the loop is spinning; a box "+
			"whose clock is stepped repeatedly would hammer the API and the "+
			"heartbeat depot", thaws, s.thawSpacing)
	}
}

// T15 — the FIRST step after a quiet period is never delayed by the floor. This
// is what keeps T14's rate limit from becoming a detection threshold: a genuine
// warm resume must still thaw immediately.
func TestHeartbeatLoopFirstStepThawsWithoutDelay(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	w := &fakeWatch{wakes: []wake{wakeClockStep, wakeDeadline}, waitGap: 2 * time.Millisecond}
	s := thawSupervisor(m, rec, w)
	// A floor far longer than the test's budget: if the first step were subject
	// to it, the thaw could not land inside the deadline below.
	s.thawSpacing = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	ok := waitUntil(func() bool { return m.thaws() >= 1 }, 2*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	if !ok {
		t.Fatalf("the first step did not thaw promptly — the thaw floor (%v) is "+
			"being applied to it, which turns a rate limit into a detection delay",
			s.thawSpacing)
	}
}

// T16 — a clock SET that moves no wall time is NOT a resume (FIX-292).
//
// This is the prod regression. Fly sets the guest CLOCK_REALTIME continuously —
// measured 2026-07-29 on a machine that had never been suspended, once every few
// seconds from 4s after boot. CANCEL_ON_SET latches on every one of them, so
// before the magnitude gate each drove a full thaw: four control-plane requests
// per box per 5s, forever, floored only by thawSpacing.
//
// Every Arm reports a step here, exactly as prod behaved, and the divergence never
// moves — which is what a clock set is. Zero thaws is the whole assertion. T14 is
// the same harness with divergence advancing, so this is not vacuous.
func TestHeartbeatLoopAbsorbsClockSetsThatMoveNoWallTime(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	w := &fakeWatch{wakes: []wake{wakeClockStep}, alwaysStepOnArm: true}
	s := thawSupervisor(m, rec, w)
	s.thawSpacing = 40 * time.Millisecond
	clk := &steppedClock{} // never advances: a set, not a suspend
	s.divergence = clk.divergence

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	waitUntil(func() bool { return false }, 400*time.Millisecond)
	cancel()
	waitUntil(func() bool { return false }, 30*time.Millisecond)

	if thaws := m.thaws(); thaws != 0 {
		t.Fatalf("thaws=%d — a clock set carrying no wall discontinuity was treated "+
			"as a resume; on Fly that is every few seconds on every box", thaws)
	}
}

// T17 — a SUB-SECOND suspend still thaws. The gate must not become the magnitude
// threshold FIX-280 deleted: the bimodal prod attach it fixed was caused by parks
// shorter than a 10s threshold going undetected entirely. 250ms is 40x below that
// threshold and must sail through.
func TestHeartbeatLoopThawsOnSubSecondSuspend(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	w := &fakeWatch{wakes: []wake{wakeClockStep, wakeDeadline}, waitGap: 2 * time.Millisecond}
	s := thawSupervisor(m, rec, w)
	clk := &steppedClock{}
	clk.add(250 * time.Millisecond)
	s.divergence = clk.divergence

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	ok := waitUntil(func() bool { return m.thaws() >= 1 }, 2*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	if !ok {
		t.Fatal("a 250ms suspend did not thaw — the magnitude gate has become a " +
			"detection threshold, which is the FIX-280 defect returning")
	}
}

// T18 — one suspend thaws ONCE, however many channels report it.
//
// Divergence is permanent: once a park has happened, every later reading taken
// against a pre-park reference still contains it. Measuring against the live arm
// therefore re-reported an already-thawed resume — seen on prod, where the arm
// channel repeated the same 465254ms magnitude the wait channel had just thawed
// on. The accumulator consumes what it reports, so the second channel sees zero.
func TestHeartbeatLoopThawsOncePerSuspend(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	// One suspend, then a clock that never moves again — but a step reported at
	// every opportunity, both channels, forever.
	w := &fakeWatch{wakes: []wake{wakeClockStep}, alwaysStepOnArm: true}
	s := thawSupervisor(m, rec, w)
	s.thawSpacing = 20 * time.Millisecond
	clk := &steppedClock{}
	clk.add(90 * time.Second)
	s.divergence = clk.divergence

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	waitUntil(func() bool { return false }, 400*time.Millisecond)
	cancel()
	waitUntil(func() bool { return false }, 30*time.Millisecond)

	thaws := m.thaws()
	if thaws == 0 {
		t.Fatal("the suspend never thawed at all")
	}
	if thaws != 1 {
		t.Fatalf("thaws=%d for ONE suspend — the magnitude is being re-reported "+
			"rather than consumed, so each channel thaws on the same park", thaws)
	}
}

// T19 — thaw drops the pooled connections BEFORE its forced beat.
//
// This is the staging outage of 2026-08-04. Every socket the agent had pooled
// was torn down by the funnel while the box sat suspended, but a frozen box
// never sees the FIN, so on resume the pool still looked healthy. thaw's forced
// beat went out on a dead connection, no response header ever came back, and it
// died at the client's 35s timeout — as did every call after it, for the 15
// minutes until the cluster reaped the box as `vm-lost`. Restarting the agent
// (a fresh transport, nothing else) recovered it in 420ms.
//
// So ORDER is the assertion, not merely presence: a drop placed after the beat
// leaves the one request that matters — the beat that flips the row to running —
// still going down a dead socket.
func TestThawDropsIdlePoolBeforeTheForcedBeat(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	s := thawSupervisor(m, rec, &fakeWatch{})
	// Driving thaw() directly skips Run's default wiring, so freeze the clock the
	// same way T8 does — sendHeartbeat reads s.now to build its retry deadline.
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return frozen }

	s.thaw(context.Background())

	calls := m.callsSnapshot()
	if len(calls) == 0 {
		t.Fatal("thaw made no API calls at all — harness wired wrong")
	}
	if calls[0] != "close-idle" {
		t.Fatalf("thaw's first API call = %q, want %q; full order = %v — a beat sent "+
			"before the pool is dropped goes down a connection that died during the "+
			"suspend, and stalls for the client timeout", calls[0], "close-idle", calls)
	}
}

// T19b — the pool drop is warm-resume-ONLY: the steady-state loop must never do
// it. Dropping idle connections on the ordinary cadence would throw away a
// perfectly good keep-alive and pay a fresh TCP+TLS handshake on every beat, on
// every box, forever — which is the cost DisableKeepAlives was rejected for.
//
// Non-vacuous: T19 trips on the same mock when a thaw does happen.
func TestSteadyStateLoopNeverDropsIdlePool(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	// Only ever wakeDeadline, so no thaw can fire (the T6 harness).
	w := &fakeWatch{wakes: []wake{wakeDeadline}, waitGap: 2 * time.Millisecond}
	s := thawSupervisor(m, rec, w)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	waitUntil(func() bool { return w.armCount() >= 20 }, 2*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	for _, c := range m.callsSnapshot() {
		if c == "close-idle" {
			t.Fatalf("the steady-state loop dropped the idle pool; that costs a fresh "+
				"TCP+TLS handshake on every beat. calls=%v", m.callsSnapshot())
		}
	}
	if hb, _ := m.snapshot(); hb == 0 {
		t.Fatal("loop never beat at all — harness wired wrong")
	}
}

func reconcileSets(rec *recordingReconciler) [][]api.Peer {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([][]api.Peer(nil), rec.sets...)
}

// directThawSupervisor wires a Supervisor for the tests that call thaw() by hand
// rather than through Run. Run's defaults() never happens on that path, and
// sendHeartbeat reads s.now to build its retry deadline, so the clock is frozen
// here exactly as T8 and T19 do it — frozen means the deadline is never
// approached, so a retry ladder runs to completion instead of being truncated.
func directThawSupervisor(m API, rec Reconciler) *Supervisor {
	s := thawSupervisor(m, rec, &fakeWatch{})
	s.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	return s
}

// T20 — thaw performs NO pull and NO reconcile of its own.
//
// This is the FIX-354 deletion, asserted directly. thaw ran on the heartbeat
// goroutine and pullLoop on the main one with nothing synchronising them, and
// both sides of the reconcile are full REPLACEMENTS — wgnet removes every
// current peer absent from the desired set, the SSH table Replaces wholesale —
// so whichever landed second won outright. A stale result carrying fewer peers
// deleted the laptop's peer and deauthorised it, durably: pullLoop's cursor
// already hashed the good content, so the server 304s forever afterwards.
// Observed landing 291-749ms apart in 3/3 staging runs.
//
// (The ticket's own argument for this race — that thaw's pull was "structurally
// guaranteed" to read a pre-attach snapshot — was disproven; thaw's pull
// returned count=2, post-attach. The missing synchronisation is the reason, not
// the ordering of the snapshot.)
//
// No pull loop runs here, so every pull and every reconcile the mocks see would
// have to be thaw's own.
func TestThawPerformsNoPullOrReconcileOfItsOwn(t *testing.T) {
	m := &thawMockAPI{}
	rec := &recordingReconciler{}
	s := directThawSupervisor(m, rec)

	s.thaw(context.Background())

	if _, pulls := m.snapshot(); len(pulls) != 0 {
		t.Fatalf("thaw pulled config itself (%d time(s), cursors=%v) — that pull's "+
			"reconcile races pullLoop's, and the loser's peer set wins", len(pulls), pulls)
	}
	if sets := reconcileSets(rec); len(sets) != 0 {
		t.Fatalf("thaw reconciled peers itself (%d time(s): %v) — pullLoop is the sole "+
			"reconciler by design", len(sets), sets)
	}
}

// T21 — exactly ONE session sync per resume.
//
// beat already fires SyncSessions, and thaw used to fire it a second time on top
// (thaw.go's own s.SyncSessions() call), so every resume shipped the whole
// session snapshot twice. The duplicate is a defect the ticket never named.
//
// The join is on the test's own SyncSessions stub, which runs INSIDE the spawned
// goroutine — so receiving from it proves the sync happened, deterministically
// and without a sleep.
func TestThawFiresExactlyOneSessionSync(t *testing.T) {
	m := &thawMockAPI{}
	s := directThawSupervisor(m, &recordingReconciler{})

	var syncs atomic.Int32
	fired := make(chan struct{}, 4)
	s.SyncSessions = func() { syncs.Add(1); fired <- struct{}{} }

	s.thaw(context.Background())
	// Join on the stub itself rather than on a flag: the stub runs INSIDE the
	// spawned sync, so receiving here proves the sync ran to completion.
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("thaw's session sync never fired")
	}

	if got := syncs.Load(); got != 1 {
		t.Fatalf("SyncSessions fired %d times for one resume, want exactly 1 — the "+
			"beat's piggyback and thaw's own call are duplicates of each other", got)
	}
}

// T22 — the session sync is OFF the resume critical path.
//
// It contributes nothing to readiness and nothing to the peer set, but in
// production it is sessions.Manager.SyncNow — a POST on its own 15s budget — and
// it used to sit inline between the beat and the pull. That put up to 15s of a
// measured 85s thaw in front of work that actually mattered.
//
// The probe: a sync that takes far longer than the thaw's whole budget. thaw
// must return without it, and the sync must still happen exactly once.
func TestThawSessionSyncIsOffTheResumeCriticalPath(t *testing.T) {
	const syncDuration = 400 * time.Millisecond
	m := &thawMockAPI{}
	s := directThawSupervisor(m, &recordingReconciler{})

	var syncs atomic.Int32
	done := make(chan struct{})
	s.SyncSessions = func() {
		time.Sleep(syncDuration)
		syncs.Add(1)
		close(done)
	}

	start := time.Now()
	s.thaw(context.Background())
	elapsed := time.Since(start)

	if elapsed >= syncDuration {
		t.Fatalf("thaw took %v with a %v session sync — the sync is still inline on "+
			"the resume path, where it delays nothing but the user", elapsed, syncDuration)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the async session sync never completed")
	}
	if got := syncs.Load(); got != 1 {
		t.Fatalf("async SyncSessions fired %d times, want exactly 1 — off the critical "+
			"path must not mean dropped", got)
	}
}

// T23 — the thaw-forced beat retries FAST first (~thawFirstRetry), then hands
// back to the ordinary ladder.
//
// A resume's first heartbeat failure is a stale socket, not an overloaded
// server: the connection it went down was killed while the box was frozen, and
// the retry is a fresh dial. The 2s ladder exists to be kind to a recovering
// API, and there is no recovering API here — meanwhile the beat is the single
// write that flips the row to `running`, so every millisecond it is late is a
// millisecond attach 409s.
//
// BackoffMin is set an order of magnitude above thawFirstRetry so the two are
// separable even with the 50% jitter: a fast first retry lands in
// [200ms, 300ms), a BackoffMin one in [600ms, 900ms).
func TestThawForcedBeatRetriesFastBeforeTheLadder(t *testing.T) {
	m := &thawMockAPI{hbFails: 1}
	s := directThawSupervisor(m, &recordingReconciler{})
	s.BackoffMin = 600 * time.Millisecond
	s.BackoffMax = 2 * time.Second
	s.HeartbeatInterval = time.Hour // the retry deadline must not truncate the ladder

	s.thaw(context.Background())

	at := m.heartbeatTimes()
	if len(at) != 2 {
		t.Fatalf("heartbeat attempts = %d, want 2 (one failure then a retry)", len(at))
	}
	gap := at[1].Sub(at[0])
	if gap >= s.BackoffMin {
		t.Fatalf("the thaw beat's first retry waited %v (>= BackoffMin %v) — a resume's "+
			"first failure is a dead socket, not an overloaded server, and the box is "+
			"unattachable for the whole wait", gap, s.BackoffMin)
	}
	if gap < thawFirstRetry/2 {
		t.Fatalf("the thaw beat's first retry waited only %v, well under thawFirstRetry "+
			"%v — it is not retrying, it is hammering", gap, thawFirstRetry)
	}
}

// T23b — the CADENCE beat's retry spacing is unchanged: its first retry still
// waits BackoffMin. thawFirstRetry is a resume-path concession, and letting it
// leak into the 30s loop would make every agent in the fleet retry a struggling
// control plane five times faster than the ladder intends.
//
// Non-vacuous against T23: same mock, same BackoffMin, opposite expectation.
func TestCadenceBeatFirstRetryStillUsesBackoffMin(t *testing.T) {
	m := &thawMockAPI{hbFails: 1}
	s := directThawSupervisor(m, &recordingReconciler{})
	s.BackoffMin = 600 * time.Millisecond
	s.BackoffMax = 2 * time.Second
	s.HeartbeatInterval = time.Hour

	s.beat(context.Background())

	at := m.heartbeatTimes()
	if len(at) != 2 {
		t.Fatalf("heartbeat attempts = %d, want 2 (one failure then a retry)", len(at))
	}
	if gap := at[1].Sub(at[0]); gap < s.BackoffMin {
		t.Fatalf("the cadence beat's first retry waited %v, want >= BackoffMin %v — "+
			"the warm-resume fast retry has leaked into the steady-state loop", gap, s.BackoffMin)
	}
}
