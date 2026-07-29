//go:build linux

package supervisor

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func newTestTimerfdWatch(t *testing.T) resumeWatch {
	t.Helper()
	w, err := newTimerfdWatch(discardLogger())
	if err != nil {
		t.Fatalf("newTimerfdWatch: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// stepRealtimeClock sets CLOCK_REALTIME to (approximately) its current value.
// A set is a set: the kernel calls clock_was_set() regardless of magnitude, so
// this trips TFD_TIMER_CANCEL_ON_SET while moving the wall clock by only the
// microseconds between the get and the set.
//
// It needs CAP_SYS_TIME, which an ordinary test runner does not have — so the
// tests using it SKIP rather than fail. That is a deliberate trade: the loop's
// behaviour on a step is covered unconditionally through the fakeWatch
// (thaw_test.go), and these two tests upgrade that to the real kernel path
// wherever the capability happens to exist.
func stepRealtimeClock(t *testing.T) {
	t.Helper()
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &ts); err != nil {
		t.Fatalf("clock_gettime: %v", err)
	}
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		t.Skipf("cannot step CLOCK_REALTIME (%v) — needs CAP_SYS_TIME", err)
	}
}

// T1 — the armed absolute deadline wakes the watch. This is the heartbeat's
// ordinary cadence path, and it also proves the fd is registered with the Go
// runtime poller (an unpollable fd would block past the deadline).
func TestTimerfdWatchDeadlineWake(t *testing.T) {
	w := newTestTimerfdWatch(t)
	if _, err := w.Arm(time.Now().Add(120 * time.Millisecond)); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	start := time.Now()
	if got := w.Wait(context.Background()); got != wakeDeadline {
		t.Fatalf("Wait = %v, want wakeDeadline", got)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("woke after %v, want ~120ms — the deadline was not honoured", elapsed)
	}
}

// T2 — ctx cancellation unblocks a Wait armed far in the future, AND the watch
// is reusable afterwards. The reuse half is the real point: Wait unblocks via a
// read deadline, so a straggler from the cancelled call could otherwise poison
// the next one. This is why Wait clears the deadline on ENTRY rather than exit.
func TestTimerfdWatchCancelledByCtxAndReusable(t *testing.T) {
	w := newTestTimerfdWatch(t)

	if _, err := w.Arm(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := w.Wait(ctx); got != wakeCancelled {
		t.Fatalf("Wait = %v, want wakeCancelled", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ctx cancellation took %v to unblock the read", elapsed)
	}

	// Reuse on a fresh ctx must still honour a deadline.
	if _, err := w.Arm(time.Now().Add(120 * time.Millisecond)); err != nil {
		t.Fatalf("re-Arm: %v", err)
	}
	if got := w.Wait(context.Background()); got != wakeDeadline {
		t.Fatalf("Wait after a cancelled Wait = %v, want wakeDeadline — the read "+
			"deadline from the cancellation leaked into the next call", got)
	}
}

// T3 — the mechanism this whole design rests on: a CLOCK_REALTIME step wakes the
// watch immediately, with the timer armed an hour out and with a step magnitude
// of essentially zero. Magnitude-independence is the property that removes
// FIX-280's dominant defect (a suspend shorter than the old 10s ThawThreshold
// was never detected at all).
func TestTimerfdWatchClockStepWake(t *testing.T) {
	w := newTestTimerfdWatch(t)
	stepRealtimeClock(t) // skips here if CAP_SYS_TIME is absent

	if _, err := w.Arm(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	stepRealtimeClock(t)

	start := time.Now()
	if got := w.Wait(context.Background()); got != wakeClockStep {
		t.Fatalf("Wait = %v, want wakeClockStep", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("step detection took %v, want ~instant", elapsed)
	}
}

// T4 — a step that lands before the read (rather than during it) is still
// reported by Wait: the cancellation is LATCHED on the fd, so Wait does not have
// to be blocked at the instant of the step.
func TestTimerfdWatchStepBeforeWaitIsNotLost(t *testing.T) {
	w := newTestTimerfdWatch(t)
	stepRealtimeClock(t) // skips here if CAP_SYS_TIME is absent

	if _, err := w.Arm(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	stepRealtimeClock(t)
	time.Sleep(50 * time.Millisecond)

	if got := w.Wait(context.Background()); got != wakeClockStep {
		t.Fatalf("Wait = %v, want wakeClockStep — a latched step was lost", got)
	}
}

// T4b — THE regression guard for the second detection channel, and the one that
// actually protects the production ordering.
//
// The latch T4 relies on is destroyed by the next timerfd_settime. So the case
// that matters is Arm → step → **Arm**: the loop re-arms after every iteration's
// work, and a resume landing during that work would be silently consumed by that
// re-arm. It is not lost only because timerfd_settime REPORTS the cancellation
// (ECANCELED) while still rearming the timer — see Arm's contract.
//
// This is exactly the production ordering: the Fly suspend a resume undoes is
// triggered BY a heartbeat, so the step lands while the loop is busy, never while
// it is blocked in Wait. Get this wrong and the common case is undetectable while
// every test that only exercises Wait still passes.
func TestTimerfdWatchArmReportsAStepItConsumes(t *testing.T) {
	w := newTestTimerfdWatch(t)
	stepRealtimeClock(t) // skips here if CAP_SYS_TIME is absent

	if _, err := w.Arm(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	stepRealtimeClock(t)
	time.Sleep(50 * time.Millisecond) // stands in for beat()/thaw() running

	stepped, err := w.Arm(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("re-Arm reported an error rather than a step: %v — ECANCELED from "+
			"timerfd_settime means 'a step was latched AND the timer is rearmed', "+
			"not a failure", err)
	}
	if !stepped {
		t.Fatal("re-Arm did not report the step it consumed — a resume landing while " +
			"the loop is busy (the production case) would be silently swallowed")
	}

	// And the timer really is rearmed: the latch is gone, so a fresh Wait blocks
	// rather than re-reporting the same step forever.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got := w.Wait(ctx); got != wakeCancelled {
		t.Fatalf("Wait after a step-reporting Arm = %v, want wakeCancelled — the "+
			"cancellation was not consumed, so the loop would thaw in a tight loop", got)
	}
}

// T11 — newResumeWatch returns the real timerfd detector on Linux. If this ever
// starts returning the plain timer, every box silently loses warm-resume
// detection and regresses to the 30s stall.
func TestNewResumeWatchUsesTimerfdOnLinux(t *testing.T) {
	w := newResumeWatch(discardLogger())
	t.Cleanup(func() { _ = w.Close() })
	if _, ok := w.(*timerfdWatch); !ok {
		t.Fatalf("newResumeWatch returned %T, want *timerfdWatch — warm resumes "+
			"would not be detected", w)
	}
}

// T12b — the REAL guard for unaccountedWall, and the only test that can catch
// the mistake the formula exists to avoid.
//
// `Time.Sub` uses the monotonic readings ALONE when both operands carry them, so
// the naive `time.Now().Sub(since) - monotonicElapsed` cancels to ~0 for every
// resume no matter how long the suspend was. That failure is silent and looks
// like data: a field named `unaccounted_wall_ms` reading 0 reads as "no
// divergence", not "broken formula". Only `Round(0)` on both operands — which
// strips the monotonic reading and forces a true WALL subtraction — reports the
// step, and only a real clock step can tell the two apart.
//
// Steps CLOCK_REALTIME forward by a known delta (the same shape as a Fly resume:
// wall jumps, monotonic does not), asserts the delta is reported, and restores
// the clock. Gated on CAP_SYS_TIME like T3/T4/T4b.
func TestUnaccountedWallReportsAClockStep(t *testing.T) {
	const jump = 2 * time.Second

	var before unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &before); err != nil {
		t.Fatalf("clock_gettime: %v", err)
	}
	// Probe writability first, so an unprivileged run skips before touching
	// anything rather than half-way through.
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &before); err != nil {
		t.Skipf("cannot step CLOCK_REALTIME (%v) — needs CAP_SYS_TIME", err)
	}

	since := time.Now()
	stepped := unix.NsecToTimespec(unix.TimespecToNsec(before) + int64(jump))
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &stepped); err != nil {
		t.Fatalf("stepping the clock forward: %v", err)
	}
	got := unaccountedWall(since)

	// Restore before asserting, so a failure cannot leave the clock advanced.
	var now unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_REALTIME, &now)
	restored := unix.NsecToTimespec(unix.TimespecToNsec(now) - int64(jump))
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &restored); err != nil {
		t.Fatalf("FAILED TO RESTORE THE CLOCK (it is %v fast): %v", jump, err)
	}

	if got < jump-200*time.Millisecond || got > jump+200*time.Millisecond {
		t.Fatalf("unaccountedWall = %v across a %v clock step, want ~%v — the wall "+
			"leg is not stripping its monotonic reading, so every resume would log "+
			"~0 regardless of suspend duration", got, jump, jump)
	}
}
