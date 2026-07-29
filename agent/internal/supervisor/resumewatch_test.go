package supervisor

import (
	"testing"
	"time"
)

// Portable tests for resumewatch.go's clock arithmetic. They live here rather
// than in resumewatch_linux_test.go so the `Round(0)` and wall-vs-monotonic
// traps are still guarded on a developer Mac, where §3.3's whole reason for
// keeping timerWatch is that `go test ./...` works.

// Smoke: with both clocks advancing together, unaccountedWall reports ~0.
// Necessary but NOT sufficient — the naive formula passes this too, which is
// why TestUnaccountedWallReportsAClockStep (Linux, CAP_SYS_TIME) exists.
func TestUnaccountedWallIsZeroWithoutAClockStep(t *testing.T) {
	since := time.Now()
	time.Sleep(30 * time.Millisecond)
	if got := unaccountedWall(since); got > 5*time.Millisecond || got < -5*time.Millisecond {
		t.Fatalf("unaccountedWall = %v across an ordinary 30ms sleep, want ~0", got)
	}
}

func TestThawDelayNeverDelaysTheFirstStep(t *testing.T) {
	// The zero Time is the "never thawed" sentinel. It must not be mistaken for
	// "thawed at the epoch, so wait": the first resume after boot has to thaw at
	// once or the floor becomes a detection threshold.
	if got := thawDelay(time.Time{}, time.Hour); got != 0 {
		t.Fatalf("thawDelay(zero) = %v, want 0 — the first step must never be paced", got)
	}
}

func TestThawDelayPacesAndExpires(t *testing.T) {
	// Just thawed ⇒ close to a full spacing left.
	if got := thawDelay(time.Now(), time.Minute); got < 50*time.Second {
		t.Fatalf("thawDelay(just now, 1m) = %v, want ~1m — the floor is not being applied", got)
	}
	// Long past ⇒ no wait.
	if got := thawDelay(time.Now().Add(-2*time.Minute), time.Minute); got != 0 {
		t.Fatalf("thawDelay(2m ago, 1m) = %v, want 0 — the floor is not expiring", got)
	}
}

// Why the floor takes the LARGER of the two legs. Each case below is one that a
// single-leg implementation gets wrong, and neither is reachable through
// thawDelay itself: Go offers no way to build a time.Time whose monotonic
// reading diverges independently from its wall reading, which is precisely the
// state a Fly resume produces. Hence the split at thawDelayFrom.
func TestThawDelayFromNeedsBothLegs(t *testing.T) {
	const spacing = 5 * time.Second

	// A park. CLOCK_MONOTONIC stops across a Fly RAM snapshot, so an hour-long
	// park advances the monotonic leg by only the time the agent was awake.
	// A monotonic-only floor scores this as "quiet for 1s" and delays the
	// resume's thaw by 4s — turning the rate limit into exactly the detection
	// threshold INV-3 says it is not, on the one path FIX-280 exists to speed up.
	if got := thawDelayFrom(time.Hour, time.Second, spacing); got != 0 {
		t.Fatalf("thawDelayFrom(wall=1h, mono=1s) = %v, want 0 — the floor is "+
			"ignoring the wall leg, so every post-park resume would be paced", got)
	}

	// A user workload stepping the clock BACKWARD 24h. A wall-only floor would
	// wait 24h — wedging the heartbeat, which INV-4 forbids outright.
	if got := thawDelayFrom(-24*time.Hour, time.Second, spacing); got > spacing {
		t.Fatalf("thawDelayFrom(wall=-24h, mono=1s) = %v, want <= %v — the floor is "+
			"ignoring the monotonic leg, so a backward clock step wedges the loop",
			got, spacing)
	}

	// Steady state: both legs agree and the floor simply expires.
	if got := thawDelayFrom(time.Second, time.Second, spacing); got != 4*time.Second {
		t.Fatalf("thawDelayFrom(1s, 1s, 5s) = %v, want 4s", got)
	}
	if got := thawDelayFrom(9*time.Second, 9*time.Second, spacing); got != 0 {
		t.Fatalf("thawDelayFrom(9s, 9s, 5s) = %v, want 0", got)
	}
}
