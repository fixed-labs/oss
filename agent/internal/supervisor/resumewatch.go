package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// wake is why a resumeWatch.Wait returned.
type wake int

const (
	// wakeDeadline — the armed absolute WALL-clock deadline passed. An ordinary
	// heartbeat tick.
	wakeDeadline wake = iota
	// wakeClockStep — CLOCK_REALTIME was stepped discontinuously. On a Fly
	// machine that is a resume from a RAM snapshot (see resumewatch_linux.go).
	wakeClockStep
	// wakeCancelled — ctx was cancelled; the loop should exit.
	wakeCancelled
	// wakeBroken — the watch malfunctioned and cannot be trusted again. The loop
	// must replace it. Distinct from wakeDeadline so a watch that fails
	// INSTANTLY and REPEATEDLY degrades once instead of spinning the heartbeat
	// loop into a tight beat-fail-beat loop against the API.
	wakeBroken
)

func (w wake) String() string {
	switch w {
	case wakeDeadline:
		return "deadline"
	case wakeClockStep:
		return "clock-step"
	case wakeCancelled:
		return "cancelled"
	case wakeBroken:
		return "broken"
	}
	return "unknown"
}

// errUnsupportedPlatform tells newResumeWatch that the plain timer is the
// EXPECTED choice, not a degradation worth alerting on: timerfd_create +
// TFD_TIMER_CANCEL_ON_SET are Linux-only, and the agent only ever runs on Linux.
var errUnsupportedPlatform = errors.New("timerfd: unsupported platform")

// resumeWatch is the heartbeat loop's wait primitive, and the seam that makes
// warm-resume detection testable without privileges (stepping CLOCK_REALTIME
// needs CAP_SYS_TIME, which no test runner can assume).
//
// A realtime clock step is reported through TWO channels, and between them they
// cover ALL wall time, so a resume can never fall into a gap:
//
//   - Wait, when the loop was blocked in it at the moment of the step;
//   - Arm's `stepped` return, when the loop was BUSY (beating, thawing) instead.
//
// The second channel is not an optimisation. `timerfd_settime` consumes a
// latched cancellation, so an arm that follows the work would otherwise destroy
// the evidence — and the Fly suspend a resume undoes is triggered BY a
// heartbeat, which puts the step squarely in that busy window.
//
// Wait returns no error by design: a malfunction is reported as wakeBroken and
// the loop swaps in a watch that cannot fail (INV-4), so there is no caller
// branch that could wedge or spin the heartbeat.
//
// NOT safe for concurrent use — heartbeatLoop is the only caller.
type resumeWatch interface {
	// Arm re-anchors the wake at an absolute WALL-clock deadline. It reports
	// stepped=true when a realtime clock step was latched since the watch was
	// last armed or waited. The watch is armed regardless of `stepped`; a
	// non-nil error means it is NOT armed and Wait must not be relied on.
	Arm(deadline time.Time) (stepped bool, err error)
	Wait(ctx context.Context) wake
	Close() error
}

// newResumeWatch picks the detector: the Linux timerfd (event-driven, no poll
// period and no magnitude threshold — the production path), else the plain
// timer, which keeps the heartbeat on cadence but detects nothing.
//
// A Linux box that lands on the plain timer has silently lost warm-resume
// detection — i.e. it is back to the 30s stall FIX-280 exists to remove — so
// that case logs at ERROR. On a non-Linux build it is simply where the agent
// does not run, so it logs at DEBUG.
func newResumeWatch(log *slog.Logger) resumeWatch {
	w, err := newTimerfdWatch(log)
	if err == nil {
		// A POSITIVE, unambiguous statement that event-driven detection is live.
		//
		// It exists because the negative signals are all ambiguous. The thaw log
		// line this agent emits on a resume is worded the same as the pre-FIX-280
		// agent's, so "no unexpected thaws" is equally true of a working detector,
		// a degraded one, and an agent so old it has no timerfd code at all — and
		// an agent reaches a devbox only via the consuming repo's locked `rift`
		// flake input, which is easy to leave stale without noticing. Grep this
		// line to tell "shipped and armed" from "looks fine, does nothing".
		log.Info("resume watch: timerfd armed (event-driven warm-resume detection)",
			"detector", "timerfd-cancel-on-set")
		return w
	}
	if errors.Is(err, errUnsupportedPlatform) {
		log.Debug("resume watch: timerfd is Linux-only; using a plain timer (the agent does not run on this platform)")
	} else {
		log.Error("resume watch: timerfd unavailable, using a plain timer — "+
			"warm resumes will NOT be detected", "err", err)
	}
	return newTimerWatch()
}

// timerWatch is the no-detection fallback: it honours the absolute deadline and
// nothing else.
//
// It exists for two cases, neither of them production. (1) A non-Linux build, so
// `go build` / `go test ./...` work on a developer machine — the agent itself
// only ever runs inside a Fly (Linux) VM. (2) Any timerfdWatch malfunction, as
// the single uniform degradation target (INV-4): the beat keeps its cadence, and
// only detection is lost.
//
// There is deliberately no polling wall-vs-monotonic detector here. Such a
// detector could only ever fire on a clock STEP in the first place — frequency
// slew would need hours to accumulate seconds of divergence — and timerfdWatch
// catches every step instantly and without a threshold. So on Linux it could
// never fire on something the timerfd missed, and off Linux there is nothing to
// detect. Its one real effect would be to keep a poll interval and a magnitude
// threshold alive, which is precisely the pair FIX-280 removes.
type timerWatch struct{ t *time.Timer }

func newTimerWatch() resumeWatch {
	// Armed far out; Arm resets it. Safe to Reset without draining: the module's
	// go directive is >= 1.23, where Reset discards an unread stale fire.
	return &timerWatch{t: time.NewTimer(time.Hour)}
}

func (w *timerWatch) Arm(deadline time.Time) (bool, error) {
	w.t.Reset(time.Until(deadline))
	return false, nil // detects nothing, so it never reports a step
}

func (w *timerWatch) Wait(ctx context.Context) wake {
	select {
	case <-w.t.C:
		return wakeDeadline
	case <-ctx.Done():
		return wakeCancelled
	}
}

func (w *timerWatch) Close() error {
	w.t.Stop()
	return nil
}

// minThawSpacing is the floor on how often a detected clock step may drive a
// thaw. It is a rate limit, not a detection threshold: the FIRST step after a
// quiet period always thaws immediately, so a genuine warm resume pays nothing.
//
// It exists because CANCEL_ON_SET latches on ANY clock set anywhere in the box,
// and a devbox runs arbitrary user workloads — a test suite that fakes time, a
// container entrypoint that runs `ntpdate`, a stray `date -s` loop. Without a
// floor, each such call drives another thaw with no wait in between, and a thaw
// is four requests (heartbeat, session sync, full config pull, session sync).
// One step per second would then produce a heartbeat-depot append per second per
// box — precisely the append-rate blowup that rules out simply shortening
// HeartbeatInterval.
//
// The pre-FIX-280 detector was rate-limited by accident: its 3s poll and 10s
// magnitude threshold both bounded thaw frequency. Deleting those (the point of
// FIX-280) means the bound has to be stated explicitly.
//
// 5s is far below any interval over which a resume's latency matters and far
// above the RTT a spin would otherwise run at.
const minThawSpacing = 5 * time.Second

// thawDelay reports how long the loop must wait before honouring another
// detected step, given when it last thawed. Zero ⇒ thaw now.
//
// It measures the quiet period as the LARGER of the wall and monotonic elapsed,
// and needs both legs for opposite reasons:
//
//   - The WALL leg is what counts a park. CLOCK_MONOTONIC stops across a Fly RAM
//     snapshot — the premise this whole design rests on — so a monotonic-only
//     measure would score a box that parked for an hour as having been quiet for
//     zero seconds, and would then delay that resume's thaw by up to the full
//     spacing. That would turn the rate limit into precisely the detection
//     threshold INV-3 says it is not, on the exact path FIX-280 exists to speed
//     up.
//   - The MONOTONIC leg is what makes it safe. A user workload stepping the wall
//     clock BACKWARD would otherwise leave the wall elapsed hugely negative and
//     wedge the loop for as long as the step.
//
// A zero lastThawAt (never thawed) is enormous on both legs, so the first step
// after startup is never delayed.
func thawDelay(lastThawAt time.Time, spacing time.Duration) time.Duration {
	return thawDelayFrom(
		// Wall leg: Round(0) strips the monotonic readings, forcing a true
		// wall-clock subtraction (a plain Sub would use the monotonic ones).
		time.Now().Round(0).Sub(lastThawAt.Round(0)),
		// Monotonic leg.
		time.Since(lastThawAt),
		spacing)
}

// thawDelayFrom is thawDelay's policy, split out from the clock reads so both
// legs can be exercised. Neither can be driven from a test otherwise: Go offers
// no way to build a time.Time whose monotonic reading diverges independently
// from its wall reading, which is exactly the state a Fly resume produces.
func thawDelayFrom(wallElapsed, monoElapsed, spacing time.Duration) time.Duration {
	elapsed := monoElapsed
	if wallElapsed > elapsed {
		elapsed = wallElapsed
	}
	if d := spacing - elapsed; d > 0 {
		return d
	}
	return 0
}

// unaccountedWall is WALL time elapsed since `since` minus MONOTONIC time
// elapsed over the same span. A Fly RAM-snapshot resume pauses the monotonic
// clock while the platform steps the wall clock forward, so on a detected resume
// this figure IS the suspend duration.
//
// One time.Now() carries both readings, so no second clock source is needed:
// Round(0) strips the monotonic reading (leaving a pure wall subtraction) while
// time.Since keeps it. `since` must therefore come from a real time.Now().
//
// This closes the evidence gap in FIX-280's prod measurement, where park-age per
// claimed VM was never instrumented: a resume logged with a value in the
// hundreds of milliseconds is a sub-second suspend detected — the case that used
// to cost a full 30s.
func unaccountedWall(since time.Time) time.Duration {
	return time.Now().Round(0).Sub(since.Round(0)) - time.Since(since)
}
