package sessions

import (
	"bytes"
	"strings"
	"testing"
)

// TestRingReplayFromLastClear: a full-screen app emits a clear (ESC[2J); replay
// starts at the most-recent clear, not the whole history.
func TestRingReplayFromLastClear(t *testing.T) {
	r := newRing(ringSize)
	r.append([]byte("OLD output that should be elided\r\n"))
	r.append([]byte("\x1b[2Jfresh frame line 1\r\n"))
	r.append([]byte("fresh frame line 2\r\n"))

	got := string(r.replay())
	if strings.Contains(got, "OLD output") {
		t.Fatalf("replay should start at the last clear, but contains old output:\n%q", got)
	}
	if !bytes.HasPrefix(r.replay(), []byte("\x1b[2J")) {
		t.Fatalf("replay should begin AT the clear sequence (never mid-escape):\n%q", got)
	}
	if !strings.Contains(got, "fresh frame line 1") || !strings.Contains(got, "fresh frame line 2") {
		t.Fatalf("replay missing post-clear content:\n%q", got)
	}
}

// TestRingReplayNoClearWholeRing: a plain log (no clear) replays the whole ring.
func TestRingReplayNoClearWholeRing(t *testing.T) {
	r := newRing(ringSize)
	r.append([]byte("log line A\n"))
	r.append([]byte("log line B\n"))
	r.append([]byte("log line C\n"))
	got := string(r.replay())
	for _, want := range []string{"log line A", "log line B", "log line C"} {
		if !strings.Contains(got, want) {
			t.Fatalf("no-clear replay missing %q:\n%q", want, got)
		}
	}
}

// TestRingClearSplitAcrossAppends: a clear sequence straddling two PTY reads is
// still detected (the carry window).
func TestRingClearSplitAcrossAppends(t *testing.T) {
	r := newRing(ringSize)
	r.append([]byte("before\r\n\x1b["))   // first half of ESC[2J
	r.append([]byte("2Jafter the clear")) // second half + content
	got := string(r.replay())
	if strings.Contains(got, "before") {
		t.Fatalf("split clear not detected; replay still has pre-clear text:\n%q", got)
	}
	if !strings.Contains(got, "after the clear") {
		t.Fatalf("replay missing post-clear content:\n%q", got)
	}
}

// TestRingEvictionDropsStaleClearMarker: once the clear marker scrolls out of
// the bounded window, replay falls back to the whole (read-aligned) ring.
func TestRingEvictionDropsStaleClearMarker(t *testing.T) {
	r := newRing(64) // tiny ring to force eviction
	r.append([]byte("\x1b[2Jframe"))
	// Push enough bytes to evict the clear marker out of the 64-byte window.
	r.append([]byte(strings.Repeat("x", 200)))
	got := r.replay()
	if len(got) > 64 {
		t.Fatalf("replay exceeded ring cap: %d", len(got))
	}
	// The clear marker is gone; replay is just the tail of the ring.
	if bytes.Contains(got, []byte("\x1b[2J")) {
		t.Fatalf("evicted clear marker should not appear in replay:\n%q", got)
	}
}

// TestRingCapEnforced: the ring never exceeds its cap.
func TestRingCapEnforced(t *testing.T) {
	r := newRing(1024)
	for i := 0; i < 100; i++ {
		r.append([]byte(strings.Repeat("a", 100)))
	}
	if got := len(r.replay()); got > 1024 {
		t.Fatalf("ring replay = %d bytes, want ≤ 1024", got)
	}
}
