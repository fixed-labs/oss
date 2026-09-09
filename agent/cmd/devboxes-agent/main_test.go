package main

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// fakeReconciler stands in for the multiReconciler fan-out. It records the
// desired sets that actually reached it, and — the point of the exercise —
// notices if two calls are ever inside it at once.
type fakeReconciler struct {
	// onApply runs INSIDE the wrapper's critical section, so a test can read
	// the wrapper's own state (the generation it just committed) without
	// racing it.
	onApply func(peers []api.Peer)
	err     error

	// sets is appended without a lock of its own, deliberately: the wrapper is
	// supposed to be the lock. If it ever stops serializing, `go test -race`
	// reports this append as a data race — which is the failure we want, loudly.
	sets [][]api.Peer

	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (f *fakeReconciler) Reconcile(peers []api.Peer) error {
	n := f.inFlight.Add(1)
	for {
		seen := f.maxInFlight.Load()
		if n <= seen || f.maxInFlight.CompareAndSwap(seen, n) {
			break
		}
	}
	f.sets = append(f.sets, peers)
	if f.onApply != nil {
		f.onApply(peers)
	}
	// Hold the critical section open long enough that a NON-serializing wrapper
	// would actually overlap here. Without this the test could pass vacuously:
	// each call might finish before the next goroutine is even scheduled.
	runtime.Gosched()
	time.Sleep(200 * time.Microsecond)
	f.inFlight.Add(-1)
	return f.err
}

func peer(pubkey string) []api.Peer {
	return []api.Peer{{LaptopWgPubkey: pubkey, LaptopWgIP: "fd5e:de7b::aa"}}
}

func TestSerialReconcilerDropsAStaleGeneration(t *testing.T) {
	// The scenario the generation counter exists for: a reconcile that was
	// issued EARLIER reaches the front of the queue LATER — the mutex gives no
	// ordering guarantee to its waiters — and carries an older desired set.
	//
	// Here that older set is empty, which is the case with teeth: an empty
	// reconcile is a full wipe (wgnet removes every peer, the SSH table
	// deauthorizes every laptop) and the strand is durable, because the pull
	// loop's cursor already hashes the peer set it last saw, so the server
	// answers 304 forever and nothing re-drives the reconcile.
	fake := &fakeReconciler{}
	s := &serialReconciler{inner: fake}

	if err := s.reconcileGen(2, peer("LAPTOP")); err != nil {
		t.Fatalf("newest generation: %v", err)
	}
	if err := s.reconcileGen(1, nil); err != nil {
		t.Fatalf("a dropped stale reconcile must report success, not an error: %v", err)
	}
	// Re-arrival of an already-applied generation is equally stale.
	if err := s.reconcileGen(2, nil); err != nil {
		t.Fatalf("replayed generation: %v", err)
	}

	if len(fake.sets) != 1 {
		t.Fatalf("stale generations reached the reconciler: applied %d sets, want 1 (%v)", len(fake.sets), fake.sets)
	}
	if len(fake.sets[0]) != 1 || fake.sets[0][0].LaptopWgPubkey != "LAPTOP" {
		t.Fatalf("wrong desired set survived: %v", fake.sets[0])
	}
	if s.seen != 2 {
		t.Fatalf("applied generation = %d, want 2", s.seen)
	}
}

func TestSerialReconcilerAppliesNewerGenerations(t *testing.T) {
	// The other half of the guard: newer is not merely "not dropped", it wins.
	fake := &fakeReconciler{}
	s := &serialReconciler{inner: fake}
	for _, tc := range []struct {
		gen uint64
		pk  string
	}{{1, "FIRST"}, {2, "SECOND"}, {3, "THIRD"}} {
		if err := s.reconcileGen(tc.gen, peer(tc.pk)); err != nil {
			t.Fatalf("gen %d: %v", tc.gen, err)
		}
	}
	if len(fake.sets) != 3 {
		t.Fatalf("applied %d sets, want 3", len(fake.sets))
	}
	if fake.sets[2][0].LaptopWgPubkey != "THIRD" {
		t.Fatalf("last applied set = %v, want THIRD", fake.sets[2])
	}
}

func TestSerialReconcilerStampsGenerationsAtEntry(t *testing.T) {
	// Reconcile must take its generation on the way IN (before the lock), or
	// the number records mutex-acquisition order instead of arrival order and
	// can never identify a stale caller.
	fake := &fakeReconciler{}
	s := &serialReconciler{inner: fake}
	if err := s.Reconcile(peer("A")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := s.Reconcile(peer("B")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := s.issued.Load(); got != 2 {
		t.Fatalf("issued = %d, want 2", got)
	}
	if s.seen != 2 {
		t.Fatalf("applied = %d, want 2", s.seen)
	}
	if len(fake.sets) != 2 {
		t.Fatalf("applied %d sets, want 2", len(fake.sets))
	}
}

func TestSerialReconcilerSerializesConcurrentCallers(t *testing.T) {
	// The property the wrapper is here to enforce, exercised against the real
	// scheduler (and, under -race, against the race detector): no two
	// reconciles overlap, and the generations that DO get applied only ever
	// move forward.
	//
	// wgnet.Reconcile is a read-then-mutate sequence of `wg` shell-outs against
	// kernel state; two of those interleaved corrupt each other in a place no
	// mutex can reach afterwards.
	const callers = 50

	var s *serialReconciler
	// Recorded inside the critical section. Note this observes the generation in
	// force AS THIS RECONCILE RUNS — the commit happens after the inner call
	// succeeds, so `applied` still holds the previous winner here. Monotonicity
	// is the property being read off this sequence; the final winner is asserted
	// from s.seen after everything has settled.
	var appliedGens []uint64
	fake := &fakeReconciler{onApply: func([]api.Peer) {
		appliedGens = append(appliedGens, s.seen)
	}}
	s = &serialReconciler{inner: fake}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, to maximise contention
			errs <- s.Reconcile(peer("LAPTOP"))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	if got := fake.maxInFlight.Load(); got != 1 {
		t.Fatalf("%d reconciles ran concurrently; the wrapper must admit exactly one at a time", got)
	}
	if got := s.issued.Load(); got != callers {
		t.Fatalf("issued = %d, want %d (one generation per call, stamped at entry)", got, callers)
	}
	if len(appliedGens) == 0 || len(appliedGens) > callers {
		t.Fatalf("applied %d generations, want 1..%d", len(appliedGens), callers)
	}
	for i := 1; i < len(appliedGens); i++ {
		if appliedGens[i] <= appliedGens[i-1] {
			t.Fatalf("generations went backwards: %v", appliedGens)
		}
	}
	// The newest generation can never be dropped (nothing newer exists to have
	// beaten it), so it is always the one left standing — which is exactly "a
	// stale result cannot overwrite a newer one".
	if s.seen != callers {
		t.Fatalf("applied = %d, want the newest (%d): %v", s.seen, callers, appliedGens)
	}
}

// A failed reconcile surfaces its error AND does not wedge the wrapper.
//
// The caller must see the failure (under guard 2, supervisor.pullLoop holds its
// cursor on the strength of it and retries), and the retry that follows must
// still be admitted rather than dropped as stale.
func TestAFailedReconcileSurfacesItsErrorAndDoesNotWedgeTheWrapper(t *testing.T) {
	boom := errors.New("wg set: exit status 1")
	fake := &fakeReconciler{err: boom}
	s := &serialReconciler{inner: fake}

	if err := s.Reconcile(peer("LAPTOP")); !errors.Is(err, boom) {
		t.Fatalf("Reconcile: %v, want the inner error", err)
	}
	// A subsequent caller must still be admitted, not dropped as stale.
	fake.err = nil
	if err := s.Reconcile(peer("LAPTOP")); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if s.seen != 2 {
		t.Fatalf("seen = %d, want 2 — the retry after a failure was dropped as stale", s.seen)
	}
}

// A newer generation that FAILED must still fence an older one.
//
// This is the hazard the type exists for, and it is only stopped by fencing on
// ADMISSION rather than on application: if the guard read `applied`, a failed
// gen 2 would leave it at 0, and gen 1 — older, and in production the one that
// might carry an empty wipe — would then pass the guard and land on top.
func TestSerialReconcilerFencesAnOlderGenerationBehindAFailedNewerOne(t *testing.T) {
	boom := errors.New("wg set: exit status 1")
	fake := &fakeReconciler{}
	s := &serialReconciler{inner: fake}

	// Stamp two generations by hand so the ORDER is stated, not raced for:
	// gen 2 (newer) runs first and fails; gen 1 (older) arrives afterwards.
	fake.err = boom
	if err := s.reconcileGen(2, peer("NEW")); !errors.Is(err, boom) {
		t.Fatalf("gen 2: %v, want the inner error", err)
	}
	fake.err = nil
	before := len(fake.sets)
	if err := s.reconcileGen(1, peer("OLD")); err != nil {
		t.Fatalf("gen 1: %v", err)
	}
	if len(fake.sets) != before {
		t.Fatalf("the OLDER generation was applied after a newer one had already been "+
			"admitted (%d → %d inner calls); a stale desired set can land on top of a "+
			"newer one whenever the newer one fails", before, len(fake.sets))
	}
}
