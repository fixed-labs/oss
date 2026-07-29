//go:build linux

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// aLongTimeAgo is a deadline in the distant past — setting it unblocks a read
// parked in the runtime poller immediately. Same idiom net/http uses.
var aLongTimeAgo = time.Unix(1, 0)

// timerfdWatch detects a warm resume as an EVENT rather than by sampling.
//
// timerfd_create(CLOCK_REALTIME) armed with TFD_TIMER_ABSTIME|TFD_TIMER_CANCEL_ON_SET
// makes read(2) fail with ECANCELED the moment the realtime clock is set
// discontinuously (kernel fs/timerfd.c: timerfd_clock_was_set marks every fd on
// the cancel list, bumps ctx->ticks and wakes pollers with EPOLLIN; timerfd_read's
// timerfd_canceled check then returns -ECANCELED, overriding any pending expiry).
//
// On a Fly machine the platform steps the guest clock on resume from a RAM
// snapshot — the base image ships no time daemon that could do it, and Fly's
// init stays VM PID 1 — so that step IS the resume, delivered with no polling
// latency and, critically, NO MINIMUM SUSPEND DURATION. A 200ms suspend raises
// the same event as a 200s one, which is what removes FIX-280's dominant defect
// (suspends shorter than the old 10s ThawThreshold were never detected at all,
// costing a full 30s heartbeat interval).
//
// Frequency slew — ordinary NTP discipline — is not a clock SET and never trips
// CANCEL_ON_SET, so steady-state operation produces no spurious wakes.
//
// The same fd doubles as the heartbeat's cadence timer: because the deadline is
// absolute on CLOCK_REALTIME (TFD_TIMER_ABSTIME) rather than monotonic, it is
// wall-anchored, and a monotonic clock that pauses across a snapshot cannot
// silently postpone the next beat.
type timerfdWatch struct {
	f   *os.File
	log *slog.Logger
}

func newTimerfdWatch(log *slog.Logger) (resumeWatch, error) {
	// TFD_NONBLOCK is what lets os.NewFile register the descriptor with the Go
	// runtime poller, so the Read below parks the goroutine instead of pinning
	// an OS thread — and makes SetReadDeadline (the ctx unblock in Wait) work.
	fd, err := unix.TimerfdCreate(unix.CLOCK_REALTIME, unix.TFD_CLOEXEC|unix.TFD_NONBLOCK)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "resume-timerfd")
	// os.NewFile SWALLOWS a netpoll registration failure and silently falls back
	// to blocking mode. Wait's ctx unblock is SetReadDeadline, which then returns
	// ErrNoDeadline — and a blocking read on an fd nobody will make readable pins
	// an OS thread until the process dies. Probe for it here so the caller gets
	// the plain timer instead of a watch that can never be interrupted.
	if err := f.SetReadDeadline(time.Time{}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("timerfd is not pollable (ctx could not interrupt a wait): %w", err)
	}
	return &timerfdWatch{f: f, log: log}, nil
}

// Arm re-anchors the absolute wall-clock deadline, and doubles as the SECOND
// detection channel.
//
// timerfd_settime returns ECANCELED when a clock step was latched since the last
// arm or read — and, per timerfd_create(2), "the timer is successfully rearmed"
// anyway ("This was probably an implementation accident, but won't be fixed
// now"). So ECANCELED here is a REPORT, not a failure: it is how a resume that
// landed while the loop was BUSY (mid-beat, mid-thaw) reaches the loop at all.
// Treating it as an error would be a triple fault — settime consumes the latch,
// so the step is swallowed; the ERROR line reserved for a broken detector fires
// spuriously; and the degrade path swaps a perfectly healthy timerfd for a plain
// timer, permanently disabling detection.
//
// A genuine error means the watch is NOT armed, and the caller must then not
// block on Wait: an unarmed pollable timerfd never becomes readable, so a Wait
// on one returns only when ctx is cancelled — i.e. the heartbeat would stop for
// good. The reachable trigger is a guest clock restored to a wild value:
// deadline.UnixNano() is defined only for ~[1678, 2262] and wraps outside it,
// and the kernel rejects the resulting negative tv_sec with EINVAL. The loop
// treats that like any other watch malfunction: it degrades to the plain timer
// PERMANENTLY, for the life of the agent process. Even a momentary wild clock
// therefore costs warm-resume detection until the agent restarts — the accepted
// price of having one degradation policy rather than a guess about which faults
// are transient.
//
// The raw fd is reached via SyscallConn().Control and NOT via os.File.Fd():
// Fd() switches a poller-registered descriptor back to blocking mode, which
// would silently un-register it and turn the Read in Wait into a thread-pinning
// blocking syscall that SetReadDeadline can no longer interrupt.
func (w *timerfdWatch) Arm(deadline time.Time) (bool, error) {
	spec := unix.ItimerSpec{Value: unix.NsecToTimespec(deadline.UnixNano())}
	sc, err := w.f.SyscallConn()
	if err != nil {
		return false, err
	}
	var serr error
	if cerr := sc.Control(func(fd uintptr) {
		serr = unix.TimerfdSettime(int(fd),
			unix.TFD_TIMER_ABSTIME|unix.TFD_TIMER_CANCEL_ON_SET, &spec, nil)
	}); cerr != nil {
		return false, cerr
	}
	if errors.Is(serr, unix.ECANCELED) {
		return true, nil
	}
	return false, serr
}

func (w *timerfdWatch) Wait(ctx context.Context) wake {
	// Clear the read deadline on ENTRY rather than on exit. A previous Wait's
	// AfterFunc can fire as that Wait returns, leaving a past deadline behind;
	// clearing here means such a straggler cannot poison this call. (Clearing on
	// exit loses that race.)
	if err := w.f.SetReadDeadline(time.Time{}); err != nil {
		w.log.Error("resume watch: clearing the read deadline failed", "err", err)
		return wakeBroken
	}
	stop := context.AfterFunc(ctx, func() { _ = w.f.SetReadDeadline(aLongTimeAgo) })
	defer stop()

	var buf [8]byte
	_, err := w.f.Read(buf[:])
	switch {
	case err == nil:
		return wakeDeadline
	case errors.Is(err, unix.ECANCELED):
		return wakeClockStep
	case ctx.Err() != nil:
		return wakeCancelled
	case os.IsTimeout(err):
		// A straggler AfterFunc from a previous Wait set a past deadline just as
		// this one cleared it. Benign and self-limiting (the entry-time clear
		// fixes the next call); an extra idempotent beat is the right response.
		return wakeDeadline
	default:
		// EBADF and friends: the fd is unusable and would fail identically
		// forever. Report it as broken so the loop swaps in a watch that cannot
		// fail, rather than spinning beat→fail→beat against the API.
		w.log.Error("resume watch: read failed", "err", err)
		return wakeBroken
	}
}

func (w *timerfdWatch) Close() error { return w.f.Close() }
