package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// --- The cursor-discriminator mock (shared harness) ---------------------------
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
	hbFails int      // fail the first N heartbeats (0 ⇒ never) — exercises beat retry
	pulls   []string // cursors received, in order

	steadyCounter int // advances the steady-state cursor "h:1","h:2",…

	// thawPeers is the peer set returned to the empty-cursor (thaw resync) pull.
	thawPeers []api.Peer
}

func (m *thawMockAPI) Heartbeat(_ context.Context, _ bool, _ int, _ api.Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hbCalls++
	if m.hbCalls <= m.hbFails {
		return fmt.Errorf("api down")
	}
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

// T5 — a wakeClockStep drives the full thaw: an out-of-cadence heartbeat, exactly
// one empty-cursor full-resync (which reaches Reconcile with the THAW peer set),
// and a SyncSessions.
func TestHeartbeatLoopThawsOnClockStepWake(t *testing.T) {
	thawPeers := []api.Peer{
		{LaptopWgPubkey: "THAW-A", LaptopWgIP: "fd::a"},
		{LaptopWgPubkey: "THAW-B", LaptopWgIP: "fd::b"},
	}
	m := &thawMockAPI{thawPeers: thawPeers}
	rec := &recordingReconciler{}
	// One step, then ordinary cadence forever.
	w := &fakeWatch{wakes: []wake{wakeClockStep, wakeDeadline}, waitGap: 5 * time.Millisecond}
	s := thawSupervisor(m, rec, w)

	var syncs int
	var syncMu sync.Mutex
	s.SyncSessions = func() { syncMu.Lock(); syncs++; syncMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)

	ok := waitUntil(func() bool {
		_, pulls := m.snapshot()
		return emptyCursorPullsAfterIndex0(pulls) >= 1
	}, 2*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	if !ok {
		_, pulls := m.snapshot()
		t.Fatalf("thaw resync (empty-cursor pull after index 0) never fired; pulls=%v", pulls)
	}

	hb, pulls := m.snapshot()

	// Exactly ONE forced resync — more would mean a repeated/mis-detected thaw.
	if got := emptyCursorPullsAfterIndex0(pulls); got != 1 {
		t.Fatalf("empty-cursor pulls after index 0 = %d, want exactly 1; pulls=%v", got, pulls)
	}

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

	// An out-of-cadence beat: with a 10s HeartbeatInterval, cadence alone
	// explains exactly one beat in this window. thaw()'s forced beat makes >= 2.
	if hb < 2 {
		t.Fatalf("expected an out-of-cadence heartbeat (hbCalls >= 2), got %d", hb)
	}

	syncMu.Lock()
	gotSyncs := syncs
	syncMu.Unlock()
	if gotSyncs == 0 {
		t.Fatal("SyncSessions never fired")
	}
}

// T6 — no thaw without a step. Only wakeDeadline is ever returned, so the loop
// must beat on cadence and never force a resync. The negative signal is ZERO
// empty-cursor pulls after index 0 — NOT SyncSessions absence (it rides the
// normal beat too). Non-vacuous because the same harness trips in T5.
func TestHeartbeatLoopNoThawOnDeadlineWake(t *testing.T) {
	m := &thawMockAPI{thawPeers: []api.Peer{{LaptopWgPubkey: "THAW", LaptopWgIP: "fd::z"}}}
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

	hb, pulls := m.snapshot()

	if got := emptyCursorPullsAfterIndex0(pulls); got != 0 {
		t.Fatalf("false positive: %d empty-cursor pulls after index 0; pulls=%v", got, pulls)
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
	thawPeers := []api.Peer{{LaptopWgPubkey: "THAW-ARM", LaptopWgIP: "fd::c"}}
	m := &thawMockAPI{thawPeers: thawPeers}
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
	ok := waitUntil(func() bool {
		_, pulls := m.snapshot()
		return emptyCursorPullsAfterIndex0(pulls) >= 1
	}, 2*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	if !ok {
		_, pulls := m.snapshot()
		t.Fatalf("a step reported by Arm did not drive a thaw; pulls=%v — a resume "+
			"landing during a beat would be lost", pulls)
	}

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
		t.Fatalf("Arm-reported step thawed but no Reconcile got the thaw peer set %v; got %v",
			thawPeers, reconcileSets(rec))
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
// retry lives INSIDE beat() (thaw() calls s.beat directly), thaw()'s forced
// heartbeat retries a transient failure exactly as the steady loop's beat does.
// This guards against a future refactor hoisting the retry up into
// heartbeatLoop, which would silently drop retry on resume.
//
// Injection: s.now is frozen, so sendHeartbeat's deadline (now + interval) is
// never approached and the K retries run to the mock's success.
func TestThawForcedBeatInheritsRetry(t *testing.T) {
	const K = 3
	m := &thawMockAPI{
		hbFails:   K,
		thawPeers: []api.Peer{{LaptopWgPubkey: "THAW", LaptopWgIP: "fd::z"}},
	}
	rec := &recordingReconciler{}
	s := thawSupervisor(m, rec, &fakeWatch{})
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return frozen }

	// Drive the thaw hook directly: thaw() → s.beat() → sendHeartbeat(), which
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
	m := &thawMockAPI{thawPeers: []api.Peer{{LaptopWgPubkey: "SPIN", LaptopWgIP: "fd::9"}}}
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

	_, pulls := m.snapshot()
	thaws := emptyCursorPullsAfterIndex0(pulls)
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
	m := &thawMockAPI{thawPeers: []api.Peer{{LaptopWgPubkey: "FIRST", LaptopWgIP: "fd::8"}}}
	rec := &recordingReconciler{}
	w := &fakeWatch{wakes: []wake{wakeClockStep, wakeDeadline}, waitGap: 2 * time.Millisecond}
	s := thawSupervisor(m, rec, w)
	// A floor far longer than the test's budget: if the first step were subject
	// to it, the thaw could not land inside the deadline below.
	s.thawSpacing = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	ok := waitUntil(func() bool {
		_, pulls := m.snapshot()
		return emptyCursorPullsAfterIndex0(pulls) >= 1
	}, 2*time.Second)
	cancel()
	waitUntil(func() bool { return false }, 20*time.Millisecond)

	if !ok {
		t.Fatalf("the first step did not thaw promptly — the thaw floor (%v) is "+
			"being applied to it, which turns a rate limit into a detection delay",
			s.thawSpacing)
	}
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
