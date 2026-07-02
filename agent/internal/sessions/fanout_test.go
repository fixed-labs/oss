package sessions

import (
	"testing"
	"time"
)

// TestSlowClientDroppedShellNeverStalls: a client that never drains its queue is
// DETACHED on fan-out overflow; a co-attached fast client keeps receiving and
// the shell (the reader calling fanout) never blocks. Driven directly through
// fanout so the drop is deterministic regardless of PTY read coalescing. Run
// under -race.
func TestSlowClientDroppedShellNeverStalls(t *testing.T) {
	m := testManager(t, &fakeAPI{})
	fast := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(fast)
	if err != nil {
		t.Fatal(err)
	}
	// A slow client that NEVER drains its queue.
	slow := NewClient(80, 24)
	if _, err := m.Attach(s.ID(), slow); err != nil {
		t.Fatal(err)
	}

	// Fan out far more chunks than the slow client's bounded queue (clientQueue)
	// can hold, draining the FAST client in lockstep each iteration so it always
	// keeps up. Each fanout enqueue is non-blocking; the slow client overflows
	// and is dropped, the fast client keeps receiving, and — crucially — fanout
	// never blocks the caller (the reader) even with a wedged client.
	const chunks = clientQueue * 4
	fastCount := 0
	for i := 0; i < chunks; i++ {
		s.fanout([]byte("x"))
		// Drain whatever the fast client has buffered so it never overflows.
		for drained := true; drained; {
			select {
			case <-fast.Output():
				fastCount++
			default:
				drained = false
			}
		}
	}

	// The slow client must have been detached (its output channel closed).
	closed := make(chan struct{})
	go func() {
		for range slow.Output() {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("slow client was not detached (its output channel never closed)")
	}

	// Only the fast client remains attached — the shell (fanout caller) never
	// stalled on the wedged slow client.
	if got := s.attachedCount(); got != 1 {
		t.Fatalf("attachedCount = %d, want 1 (only slow client dropped)", got)
	}
	if fastCount == 0 {
		t.Fatal("fast client received nothing — fan-out should have reached it")
	}
	s.Detach(fast)
}

// TestEndClosesAllClientChannels: when the shell exits, every attached client's
// output channel closes (so each connection's writer goroutine unblocks).
func TestEndClosesAllClientChannels(t *testing.T) {
	m := testManager(t, &fakeAPI{})
	c1 := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(c1)
	if err != nil {
		t.Fatal(err)
	}
	c2 := NewClient(80, 24)
	if _, err := m.Attach(s.ID(), c2); err != nil {
		t.Fatal(err)
	}
	drainClient(c1)
	drainClient(c2)

	if _, err := s.Write([]byte("exit 0\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("no end")
	}
	// Both channels closed.
	for i, c := range []*Client{c1, c2} {
		done := make(chan struct{})
		go func(c *Client) {
			for range c.Output() {
			}
			close(done)
		}(c)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("client %d output channel not closed on session end", i)
		}
	}
}
