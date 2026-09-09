package supervisor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// --- The long-poll-holding mock (resume-kick harness) -------------------------
//
// thawMockAPI answers every poll instantly, which is fine for "did a thaw
// happen" and useless for the mechanism this PR is about: cancelling a poll that
// was never in flight proves nothing. holdPullAPI's PullConfig BLOCKS on the
// request context the way the real ≤25s long-poll does, so the cancel, the
// immediate re-poll it triggers, and — the part that is easy to get wrong — its
// ordering against CloseIdleConnections are all directly assertable.
type holdPullAPI struct {
	mu sync.Mutex

	// script is consumed one entry per poll; once exhausted every further poll
	// blocks.
	script []pullAction

	pullStarts []time.Time
	// inFlight is the request context of the most recent poll. Reading its Err()
	// from inside CloseIdleConnections is the CAUSAL form of "the cancel came
	// first": it does not depend on which goroutine the scheduler picks.
	inFlight context.Context

	// closeIdleHold makes the pool drop take measurable time, so "did the pull
	// loop sneak a request in while the pool was being dropped" is a question
	// with a real window in which to answer wrong.
	closeIdleHold      time.Duration
	pullCtxDoneAtClose []bool
	// pullsAtCloseStart/End bracket the drop with the number of polls issued, so
	// a re-poll landing INSIDE the drop shows up as a difference.
	pullsAtCloseStart []int
	pullsAtCloseEnd   []int

	calls []string
}

type pullAction int

const (
	// pullBlock holds the request open until its context is cancelled — what the
	// real long-poll does, and the only shape in which "the in-flight poll was
	// cancelled" is a statement about anything.
	pullBlock pullAction = iota
	// pullFail returns an ordinary transient error at once (the backoff path).
	pullFail
)

func (m *holdPullAPI) PullConfig(ctx context.Context, _ string) (*api.Config, error) {
	m.mu.Lock()
	m.pullStarts = append(m.pullStarts, time.Now())
	m.inFlight = ctx
	m.calls = append(m.calls, "pull")
	action := pullBlock
	if len(m.script) > 0 {
		action, m.script = m.script[0], m.script[1:]
	}
	m.mu.Unlock()

	if action == pullFail {
		return nil, errors.New("pull boom")
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *holdPullAPI) Heartbeat(_ context.Context, _ bool, _ int, _ api.Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "beat")
	return nil
}

func (m *holdPullAPI) CloseIdleConnections() {
	m.mu.Lock()
	m.calls = append(m.calls, "close-idle")
	done := false
	if m.inFlight != nil {
		done = m.inFlight.Err() != nil
	}
	m.pullCtxDoneAtClose = append(m.pullCtxDoneAtClose, done)
	m.pullsAtCloseStart = append(m.pullsAtCloseStart, len(m.pullStarts))
	hold := m.closeIdleHold
	m.mu.Unlock()

	if hold > 0 {
		time.Sleep(hold)
	}

	m.mu.Lock()
	m.pullsAtCloseEnd = append(m.pullsAtCloseEnd, len(m.pullStarts))
	m.mu.Unlock()
}

func (m *holdPullAPI) pullCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pullStarts)
}

func (m *holdPullAPI) pullStartAt(i int) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pullStarts[i]
}

func (m *holdPullAPI) closeReports() (ctxDone []bool, pullsBefore, pullsAfter []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]bool(nil), m.pullCtxDoneAtClose...),
		append([]int(nil), m.pullsAtCloseStart...),
		append([]int(nil), m.pullsAtCloseEnd...)
}

func (m *holdPullAPI) callsSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// holdSupervisor wires a Supervisor for the resume-kick tests. The clock is
// frozen for the same reason the direct-thaw tests freeze it (sendHeartbeat
// builds its retry deadline from s.now, and these tests call thaw() by hand,
// which skips Run's defaults()).
func holdSupervisor(m *holdPullAPI) *Supervisor {
	s := &Supervisor{API: m, Reconcile: &recordingReconciler{}}
	s.Log = discardLogger()
	s.Identity = api.Identity{WgPubkey: "WGPUB"}
	s.SSHSessions = func() int { return 0 }
	s.HeartbeatInterval = time.Hour
	s.PollFloor = time.Millisecond
	s.BackoffMin = time.Millisecond
	s.BackoffMax = 4 * time.Millisecond
	s.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	return s
}

// runPullLoop starts pullLoop and registers a cleanup that stops it and WAITS
// for it, so no test leaves a poll goroutine running past its own end.
func runPullLoop(t *testing.T, s *Supervisor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.pullLoop(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("pullLoop did not stop when its run context was cancelled")
		}
	})
}

// C1 — ORDERING ONLY.
//
// This asserts the cancel precedes the drop. It deliberately does NOT claim the
// drop is therefore effective: a cancelled request's stream is not unregistered
// until cleanupWriteRequest runs on another goroutine, so ordering alone proves
// nothing about whether the connection was actually evicted. That property is
// only observable server-side, and is covered by
// TestThawForcedBeatLandsOnAFreshConnection.
//
// Original note: C1 — thaw cancels the in-flight poll BEFORE it drops the pool.
//
// Order is the whole mechanism, not a nicety. api.Client owns one transport and
// the control plane speaks h2, so every call multiplexes onto ONE
// http2ClientConn; Transport.CloseIdleConnections reaches that conn's
// closeIfIdle, which returns early while len(cc.streams) > 0, and the config
// long-poll holds a stream essentially always. A drop performed while the poll
// is still in flight is therefore a NO-OP — the dead conn stays pooled and every
// later request is handed the same corpse and dies at the 35s client timeout,
// permanently, which is the observed staging failure.
//
// The assertion is causal rather than a race on the log order: the mock reads
// the in-flight poll's context inside CloseIdleConnections. If the cancel really
// came first, that context is already Done at that instant; if the drop came
// first, it is still live.
func TestThawCancelsTheInFlightPollBeforeDroppingThePool(t *testing.T) {
	m := &holdPullAPI{}
	s := holdSupervisor(m)
	runPullLoop(t, s)

	if !waitUntil(func() bool { return m.pullCount() >= 1 }, 2*time.Second) {
		t.Fatal("the pull loop never issued its first poll — harness wired wrong")
	}

	s.thaw(context.Background())

	ctxDone, _, _ := m.closeReports()
	if len(ctxDone) != 1 {
		t.Fatalf("CloseIdleConnections called %d times in one thaw, want exactly 1; calls=%v",
			len(ctxDone), m.callsSnapshot())
	}
	if !ctxDone[0] {
		t.Fatal("the in-flight poll's context was NOT yet cancelled when CloseIdleConnections " +
			"ran — the cancel must come first, or the request is still live across the drop")
	}
}

// C2 — the pull loop is kept OUT until the pool has actually been dropped.
//
// This is the ordering hazard the cancel creates. cancel() wakes pullLoop on a
// DIFFERENT goroutine (the main one) from the one thaw runs on (the heartbeat
// one), and pullLoop's entire job on waking is to re-poll immediately. If it
// wins that race it opens a fresh stream on the very ClientConn about to be
// dropped, closeIfIdle sees len(cc.streams) > 0 again, and the bug is back —
// self-inflicted this time. So the sequence must be a guarantee (pullMu held
// across cancel + CloseIdleConnections), never a scheduling hope.
//
// The probe: a drop that takes 80ms. Without the interlock the woken loop has
// 80ms of sleeping-in-CloseIdleConnections in which to issue its re-poll, which
// it will. With it, the re-poll blocks in pullOnce until the drop returns.
func TestThawKeepsThePullLoopOutUntilThePoolIsDropped(t *testing.T) {
	m := &holdPullAPI{closeIdleHold: 80 * time.Millisecond}
	s := holdSupervisor(m)
	runPullLoop(t, s)

	if !waitUntil(func() bool { return m.pullCount() >= 1 }, 2*time.Second) {
		t.Fatal("the pull loop never issued its first poll — harness wired wrong")
	}

	s.thaw(context.Background())

	_, before, after := m.closeReports()
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("expected exactly one pool drop, got %d/%d; calls=%v", len(before), len(after), m.callsSnapshot())
	}
	if before[0] != after[0] {
		t.Fatalf("%d poll(s) were issued while the idle pool was being dropped — the "+
			"woken pull loop re-dialled onto the ClientConn thaw was in the middle of "+
			"evicting, which makes closeIfIdle a no-op and restores the bug",
			after[0]-before[0])
	}
	// Non-vacuous: the loop must be kept out only until the drop is done, not
	// forever. The kick still has to land.
	if !waitUntil(func() bool { return m.pullCount() >= 2 }, 2*time.Second) {
		t.Fatal("the cancelled poll never re-polled — the kick is not reaching pullLoop, " +
			"so a resume now fetches no fresh config at all")
	}
}

// C3 — a cancelled poll is re-polled IMMEDIATELY: no backoff sleep, no poll
// floor.
//
// Cancelling the stale poll IS the pull-loop kick — it is the only thing that
// makes a resume fetch fresh config, now that thaw does no pull of its own. A
// kick that then waits out a backoff or the poll floor is not a kick.
//
// Both sleeps are set to 500ms and both would apply on this path if the
// cancellation branch did not skip them (the poll floor measures from the poll's
// START, and this poll has been alive for only a few ms). So a re-poll inside
// 200ms can only mean neither ran.
func TestPullLoopRePollsImmediatelyOnACancelledPoll(t *testing.T) {
	m := &holdPullAPI{}
	s := holdSupervisor(m)
	s.PollFloor = 500 * time.Millisecond
	s.BackoffMin = 500 * time.Millisecond
	s.BackoffMax = time.Second
	runPullLoop(t, s)

	if !waitUntil(func() bool { return m.pullCount() >= 1 }, 2*time.Second) {
		t.Fatal("the pull loop never issued its first poll — harness wired wrong")
	}

	cancelledAt := time.Now()
	s.cancelPollAndDropPool()

	if !waitUntil(func() bool { return m.pullCount() >= 2 }, 2*time.Second) {
		t.Fatal("the cancelled poll never re-polled")
	}
	if gap := m.pullStartAt(1).Sub(cancelledAt); gap >= 200*time.Millisecond {
		t.Fatalf("the re-poll came %v after the cancel, with PollFloor=%v and "+
			"BackoffMin=%v — the cancellation is being treated as a failure and slept "+
			"on, so a resume waits for its fresh config", gap, s.PollFloor, s.BackoffMin)
	}
}

// C4 — a cancelled poll RESETS the backoff.
//
// A resume kick arriving mid-outage must not inherit the outage's backoff: the
// point of the kick is that the next poll happens now. The ladder is climbed
// first (three real failures), so a reset is distinguishable from never having
// grown.
func TestPullLoopResetsBackoffOnACancelledPoll(t *testing.T) {
	m := &holdPullAPI{script: []pullAction{pullFail, pullFail, pullFail, pullBlock, pullFail}}
	log, bl := newBufferLogger()
	s := holdSupervisor(m)
	s.Log = log
	s.PollFloor = time.Millisecond
	s.BackoffMin = 10 * time.Millisecond
	s.BackoffMax = time.Second
	runPullLoop(t, s)

	// Poll 4 is the blocking one: by the time it is in flight the ladder has
	// climbed through three failures.
	if !waitUntil(func() bool { return m.pullCount() >= 4 }, 3*time.Second) {
		t.Fatalf("only %d polls issued; the backoff ladder never ran", m.pullCount())
	}
	s.cancelPollAndDropPool()
	// Poll 5 fails (logging the post-cancel backoff), poll 6 blocks.
	if !waitUntil(func() bool { return m.pullCount() >= 6 }, 3*time.Second) {
		t.Fatalf("only %d polls issued after the cancel", m.pullCount())
	}

	backoffs := loggedBackoffs(bl)
	if len(backoffs) != 4 {
		t.Fatalf("logged %d pull failures, want 4 (3 before the cancel, 1 after): %v",
			len(backoffs), backoffs)
	}
	// The jitter is up to +50%, so BackoffMin*2 separates "at the floor" from
	// "one rung up" cleanly.
	if backoffs[2] < 2*s.BackoffMin {
		t.Fatalf("the backoff never climbed before the cancel (%v) — the reset assertion "+
			"below would be vacuous", backoffs)
	}
	if backoffs[3] >= 2*s.BackoffMin {
		t.Fatalf("the failure after the cancel backed off %v, still up the ladder (%v) — "+
			"a resume kick must reset the backoff, or the box keeps waiting out an "+
			"outage that ended when it was suspended", backoffs[3], backoffs)
	}
}

// C5 — a cancelled RUN context still ends the loop.
//
// This is the discrimination the cancellation branch has to get right. Cancelling
// the run context cancels the per-request child too, so the error PullConfig
// returns at shutdown is byte-identical to the one a resume kick produces:
// context.Canceled. The only thing that tells them apart is the run context
// itself, which is why pullLoop tests ctx.Err() BEFORE it tests the error. Get
// that backwards and shutdown becomes an unbounded re-poll spin instead of a
// return.
func TestPullLoopExitsWhenTheRunContextEnds(t *testing.T) {
	m := &holdPullAPI{}
	s := holdSupervisor(m)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.pullLoop(ctx)
		close(done)
	}()

	if !waitUntil(func() bool { return m.pullCount() >= 1 }, 2*time.Second) {
		cancel()
		t.Fatal("the pull loop never issued its first poll — harness wired wrong")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("pullLoop did not return when the run context ended (%d polls issued "+
			"and counting) — a cancelled run context is being read as a resume kick",
			m.pullCount())
	}
	if n := m.pullCount(); n != 1 {
		t.Fatalf("pullLoop issued %d polls, want 1 — it re-polled after the run context "+
			"ended", n)
	}
}

// loggedBackoffs extracts the backoff attr from every "config pull failed"
// record, in order. slog's TextHandler renders a time.Duration unquoted
// (e.g. backoff=12.5ms), so the token parses straight back.
func loggedBackoffs(bl *bufferLogger) []time.Duration {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	var out []time.Duration
	for _, line := range strings.Split(bl.buf.String(), "\n") {
		if !strings.Contains(line, `msg="config pull failed"`) {
			continue
		}
		i := strings.Index(line, "backoff=")
		if i < 0 {
			continue
		}
		tok := line[i+len("backoff="):]
		if j := strings.IndexByte(tok, ' '); j >= 0 {
			tok = tok[:j]
		}
		d, err := time.ParseDuration(strings.Trim(tok, `"`))
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	return out
}

// stuckPullAPI's PullConfig ignores cancellation entirely: it returns only when
// released. That is the shape pollReturnWait exists for — a poll that will not
// come back — which no other harness in this package can produce, because they
// all return promptly on ctx.Done().
type stuckPullAPI struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func (m *stuckPullAPI) Heartbeat(context.Context, bool, int, api.Identity) error { return nil }
func (m *stuckPullAPI) CloseIdleConnections()                                    {}
func (m *stuckPullAPI) PullConfig(context.Context, string) (*api.Config, error) {
	m.once.Do(func() { close(m.started) })
	<-m.release // NOT ctx — deliberately unresponsive to cancellation
	return &api.Config{Cursor: "h:1"}, nil
}

// A poll that never returns must not hold the resume path open indefinitely.
//
// cancelPollAndDropPool waits for the cancelled poll to unwind before dropping
// the pool, because a cancelled request's stream is not gone when cancel()
// returns. But that wait has to be BOUNDED: the beat behind it is the single
// write that makes the box attachable, and it must not queue behind a request
// that is never coming back. Overrunning the bound costs a no-op drop — the
// pre-fix behaviour — not a hang.
func TestTheDropDoesNotWaitForeverOnAPollThatNeverReturns(t *testing.T) {
	m := &stuckPullAPI{release: make(chan struct{}), started: make(chan struct{})}

	s := &Supervisor{API: m, Reconcile: &recordingReconciler{}}
	s.Log = discardLogger()
	s.PollFloor = time.Millisecond
	s.defaults()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.pullLoop(ctx) }()
	// Release the stuck poll BEFORE joining the loop. It ignores ctx by
	// construction, so cancelling alone would never let it return and the join
	// would hang — which is the whole reason this harness exists.
	defer func() { cancel(); close(m.release); <-done }()

	select {
	case <-m.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the pull loop never issued a poll")
	}

	// Run the drop off the test goroutine so an UNBOUNDED wait fails here with a
	// legible message, rather than wedging the whole binary until the package
	// timeout — which would report a hang, not the property that broke.
	took := make(chan time.Duration, 1)
	go func() {
		st := time.Now()
		s.cancelPollAndDropPool()
		took <- time.Since(st)
	}()

	ceiling := pollReturnWait + streamCleanupSettle + time.Second
	select {
	case elapsed := <-took:
		if elapsed < pollReturnWait {
			t.Fatalf("the drop returned in %v, sooner than pollReturnWait (%v) — it cannot "+
				"have waited for the poll at all", elapsed, pollReturnWait)
		}
	case <-time.After(ceiling):
		t.Fatalf("the pool drop had not returned after %v against a poll that never comes "+
			"back — the wait for the poll to unwind is unbounded, so the beat that makes the "+
			"box attachable queues behind it", ceiling)
	}
}
