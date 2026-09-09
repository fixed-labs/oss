package supervisor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// failingReconciler fails its first `failures` calls, then succeeds, recording
// when each call landed so a test can measure the spacing between retries.
type failingReconciler struct {
	mu       sync.Mutex
	failures int
	calls    atomic.Int64
	at       []time.Time // capped: a spinning implementation must not OOM the test
}

func (r *failingReconciler) Reconcile([]api.Peer) error {
	n := r.calls.Add(1)
	r.mu.Lock()
	if len(r.at) < 16 {
		r.at = append(r.at, time.Now())
	}
	r.mu.Unlock()
	if int(n) <= r.failures {
		return errors.New("wg set: exit status 1")
	}
	return nil
}

func (r *failingReconciler) callCount() int { return int(r.calls.Load()) }

func (r *failingReconciler) callTimes() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.at...)
}

func reconcileFailSupervisor(m *thawMockAPI, rec Reconciler) *Supervisor {
	s := &Supervisor{API: m, Reconcile: rec}
	s.Log = discardLogger()
	s.PollFloor = time.Millisecond
	s.BackoffMin = 20 * time.Millisecond
	s.BackoffMax = 80 * time.Millisecond
	s.defaults()
	return s
}

// A FAILED reconcile must not advance the cursor.
//
// The cursor is the server's content hash, so advancing it after a reconcile that
// did not happen asserts "I am holding this state" when the box is not: the server
// then answers 304 to every later poll and the failure is never retried. Since a
// failed pass can leave a peer wholly absent from wg0 (wgnet.Reconcile's
// remove-then-re-add), that is a durable strand — the class this change set exists
// to remove — reachable from a transient fork/exec failure.
func TestAFailedReconcileDoesNotAdvanceTheCursor(t *testing.T) {
	m := &thawMockAPI{}
	rec := &failingReconciler{failures: 2}
	s := reconcileFailSupervisor(m, rec)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.pullLoop(ctx) }()
	// DEFERRED, not called inline after the assertions: waitForReconciles and the
	// assertions below can t.Fatal, and an un-cancelled pullLoop would then be
	// left running for the rest of the binary's life — spinning, if the pacing
	// this test exists to check is ever broken.
	defer func() {
		cancel()
		// BOUNDED join. The retry path must remain cancellable — if it ever stops
		// observing ctx (which is what an unpaced busy-retry looks like), an
		// unbounded wait here would hang the whole test binary instead of
		// reporting the defect.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("pullLoop did not exit within 2s of cancellation — the reconcile-failure " +
				"retry path is not observing the run context")
		}
	}()
	waitForReconciles(t, rec, 4)

	_, pulls := m.snapshot()
	if len(pulls) < 4 {
		t.Fatalf("only %d polls issued: %q", len(pulls), pulls)
	}
	// The mock advances its cursor on every poll, so an advancing cursor in the
	// REQUEST stream means the loop accepted a config it never applied. The first
	// two reconciles fail, so polls 1..3 must all carry "".
	for i := 0; i < 3; i++ {
		if pulls[i] != "" {
			t.Fatalf("poll %d carried cursor %q; a failed reconcile advanced the cursor, so "+
				"the server will 304 and the failure is never retried (pulls: %q)", i+1, pulls[i], pulls)
		}
	}
	if pulls[3] == "" {
		t.Fatalf("poll 4 still carried an empty cursor after a SUCCESSFUL reconcile; the "+
			"loop never advances at all (pulls: %q)", pulls)
	}
}

// …and the retry it enables must be PACED.
//
// Holding the cursor means the server stops long-polling: livepoll returns 200 the
// instant the supplied cursor differs from the rendered content, which it always
// does while we hold a stale one. So an unpaced retry is not "one redundant poll" —
// it is pull→reconcile→PollFloor→repeat at the floor's rate, forever, re-running
// the whole `wg` fork/exec sequence each pass, against a control plane serving the
// entire fleet, in exactly the memory-pressured conditions that caused the failure.
// A durable reconcile failure is reachable from ordinary data (an empty
// LaptopWgIP yields `allowed-ips /128`), so this is not a transient-only concern.
func TestAFailedReconcileIsRetriedOnTheBackoffLadderNotThePollFloor(t *testing.T) {
	m := &thawMockAPI{}
	// Never succeeds — the persistent case, which is where an unpaced loop spins.
	rec := &failingReconciler{failures: 1 << 30}
	s := reconcileFailSupervisor(m, rec)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.pullLoop(ctx) }()
	defer func() {
		cancel()
		// BOUNDED join. The retry path must remain cancellable — if it ever stops
		// observing ctx (which is what an unpaced busy-retry looks like), an
		// unbounded wait here would hang the whole test binary instead of
		// reporting the defect.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("pullLoop did not exit within 2s of cancellation — the reconcile-failure " +
				"retry path is not observing the run context")
		}
	}()

	// Sample the RATE over a fixed window. That is the property in question: with
	// the cursor held the server stops long-polling and answers instantly, so the
	// only thing standing between a failing box and a hot loop is this pacing.
	const window = 400 * time.Millisecond
	time.Sleep(window)
	got := rec.callCount()

	// Paced (BackoffMin 20ms, doubling to 80ms) ⇒ ~5 in 200ms. Unpaced ⇒ thousands.
	// The bound is deliberately loose: it is not asserting an exact ladder, it is
	// asserting that a persistent failure cannot become a busy loop.
	const maxInWindow = 30
	if got > maxInWindow {
		t.Fatalf("a persistently failing reconcile was retried %d times in %v (limit %d): "+
			"the failure path is not paced, so every box in this state would hammer "+
			"/agent-config at the poll floor's rate, re-running the whole wg fork/exec "+
			"sequence each pass", got, window, maxInWindow)
	}
	if got < 2 {
		t.Fatalf("only %d reconcile(s) in %v — the retry is not happening at all, so a "+
			"transient failure would never be re-applied", got, window)
	}

	// And the ladder must climb, or a persistent failure never reaches BackoffMax.
	at := rec.callTimes()
	if len(at) < 4 {
		t.Fatalf("only %d retries in %v — too few to read the ladder off; the window must be "+
			"wide enough that this assertion is unconditional", len(at), window)
	}
	{
		if first, last := at[1].Sub(at[0]), at[3].Sub(at[2]); last <= first {
			t.Fatalf("backoff is not climbing across retries (%v then %v); a persistent "+
				"failure would keep polling at the initial rate", first, last)
		}
	}
}

func waitForReconciles(t *testing.T, rec *failingReconciler, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.callTimes()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the pull loop reached only %d reconciles, want %d", len(rec.callTimes()), n)
		}
		time.Sleep(time.Millisecond)
	}
}
