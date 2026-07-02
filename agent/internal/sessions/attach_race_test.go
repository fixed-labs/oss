package sessions

import (
	"sync"
	"testing"
	"time"
)

// TestAttachReplayNeverPanicsAgainstConcurrentFanoutOverflow is the regression
// test for the "panic: send on closed channel" crash: when a
// client attaches to a BUSY session, the attach-path scrollback replay raced the
// fan-out reader. Under the old ordering attach registered the client in
// s.clients, RELEASED the lock, then did BLOCKING sends of the replay onto c.out;
// once visible, a fan-out overflow could close c.out (detachOnce) while an
// in-flight blocking send was still running — panicking and taking down the whole
// agent.
//
// This test models the exact production ordering: a freshly-created session whose
// reader is already firing fanout overflows (clientQueue+ chunks, no drain) while
// a second client attaches and replays scrollback concurrently. The attach's
// non-blocking replay sends must never panic, even when the very same client is
// the one fanout drops and closes.
//
// MUST pass under -race. Against the pre-fix ordering (register-then-blocking-
// replay) this reliably panics; with the fix (buffer replay under the lock, then
// register) it cannot.
func TestAttachReplayNeverPanicsAgainstConcurrentFanoutOverflow(t *testing.T) {
	// Seed the ring with a non-trivial replay so attach buffers a real chunk
	// (not just the two clear markers) — maximizes the chance a concurrent close
	// would land on an in-flight send under the old ordering.
	const iters = 200
	for i := 0; i < iters; i++ {
		m := testManager(t, &fakeAPI{})
		first := NewClient(80, 24)
		s, err := m.CreateOrAttachDefault(first)
		if err != nil {
			t.Fatal(err)
		}
		// Drain the first (creator) client so the shell's own output never wedges
		// the session and our overflow targets only the racing client.
		drainClient(first)

		// Fill the ring so replay is a sizeable chunk.
		s.ring.append(make([]byte, ringSize/2))

		racer := NewClient(80, 24) // never drained → fanout will overflow + close it

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine A: attach the racer, which buffers the scrollback replay onto
		// racer.out. Under the buggy ordering its blocking replay send raced the
		// close below.
		go func() {
			defer wg.Done()
			s.attach(racer)
		}()

		// Goroutine B: hammer fanout with far more than clientQueue chunks so the
		// undrained racer overflows and fanout closes racer.out via detachOnce —
		// concurrently with the attach replay.
		go func() {
			defer wg.Done()
			for j := 0; j < clientQueue*4; j++ {
				s.fanout([]byte("x"))
			}
		}()

		wg.Wait()

		// Drain whatever the racer ended up with (channel may already be closed by
		// the overflow); ranging over a closed channel is safe and must not block.
		done := make(chan struct{})
		go func() {
			for range racer.Output() {
			}
			close(done)
		}()

		// The racer may still be attached (if it never overflowed) — detaching it
		// closes the channel so the drain goroutine terminates. detachOnce makes a
		// double close (overflow already closed it) safe.
		s.Detach(racer)

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("racer output channel never closed")
		}
		s.Detach(first)
	}
}
