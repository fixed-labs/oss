// Package supervisor runs the agent's two loops, using the same hardened
// pull-reconcile shape the relay uses:
//
//   - pull-reconcile: long-poll GET agent-config with a cursor; reconcile
//     wg0's peer set on EVERY successful pull (steady-state self-heal);
//     jittered exponential backoff on errors; a poll-rate
//     floor so a server that answers instantly can't busy-loop the agent. It is
//     the SOLE reconciler, and each poll runs under a per-request context the
//     warm-resume hook cancels to force an immediate re-poll (thaw.go).
//   - heartbeat: every 30s, reporting the box-observed interactive liveness
//     (open SSH sessions + held/attached PTYs; drives idle-tiering) AND the
//     machine's identity (s.Identity). A persistent session that is currently
//     DETACHED has no open SSH conn, so its "stay alive" signal is the laptop's
//     presence ping to the control plane (updating
//     last-interactive-liveness-at) for the life of the `connect`, plus the
//     session module's keep-warm clock (LastDetachAt). The heartbeat IS the
//     readiness signal: the cluster flips provisioned/starting → running off the
//     carried wg-pubkey. There is no separate ready-report loop — readiness is
//     continuous + self-healing, so a dropped beat costs ~30s, not a stranded
//     box.
//
// All dependencies are interfaces/functions so the whole loop runs in-process
// against a mock API in tests.
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// API is the slice of the api.Client the supervisor drives. There is no
// ReportReady: readiness is asserted continuously on the heartbeat (the cluster
// flips provisioned/starting → running off the identity facts the heartbeat
// carries), so a dropped beat self-heals on the next one.
type API interface {
	Heartbeat(ctx context.Context, interactiveLive bool, sshSessions int, identity api.Identity) error
	PullConfig(ctx context.Context, cursor string) (*api.Config, error)
	// CloseIdleConnections drops pooled keep-alive sockets. It is part of this
	// interface because only the supervisor knows a resume happened, and the
	// pool does not survive one (see thaw, and api.CloseIdleConnections).
	CloseIdleConnections()
}

// Reconciler applies a pulled desired peer set (wgnet.Net in production).
type Reconciler interface {
	Reconcile(desired []api.Peer) error
}

type Supervisor struct {
	API       API
	Reconcile Reconciler
	Log       *slog.Logger
	// Identity is the machine's public, VM-self-generated identity, asserted on
	// EVERY heartbeat (there is no one-shot ready report). The cluster persists
	// it and flips provisioned/starting → running off a non-empty WgPubkey.
	Identity api.Identity
	// SSHSessions counts the open authorized SSH connections (the embedded
	// server's sshserver.Server.ActiveSessions in production) — the box-observed
	// raw-connection liveness signal. It rides the heartbeat as ssh_sessions.
	SSHSessions func() int

	// Session-liveness accessors (the persistent-session Manager in production).
	// They count SESSIONS (a different axis from SSHSessions' connection gauge):
	//   - AttachedClients: sessions with ≥1 attached client.
	//   - HeldLivePTYs:    sessions with a live shell, attached or not.
	//   - LastDetachAt:    the most-recent detach across all held PTYs (the
	//     keep-warm clock origin); zero time = no detach yet.
	//   - SyncSessions:    POST a snapshot of all live sessions; fired on the
	//     heartbeat cadence (and, by the Manager itself, on attach/detach).
	// All optional: nil ⇒ a box with no session module (overlay-less boot /
	// tests) contributes no session liveness.
	AttachedClients func() int
	HeldLivePTYs    func() int
	LastDetachAt    func() time.Time
	SyncSessions    func()

	// now is the injected WALL clock (defaults time.Now), used solely by
	// sendHeartbeat to bound its retry window. The heartbeat loop's own timing
	// uses real time.Now: its arm deadline is handed to the kernel, and the
	// resume-magnitude figure it logs needs a Time carrying a genuine monotonic
	// reading (see unaccountedWall).
	//
	// FIX-280 removed the companion monoNow injectable. It existed only so a test
	// could drive wall and monotonic apart for the old sampling detector; the
	// detector is now event-driven (resumewatch_linux.go) and tests fake the
	// resumeWatch itself, so a second clock source has no remaining consumer.
	now func() time.Time

	// newWatch builds the heartbeat loop's wait primitive (resumewatch.go). nil ⇒
	// newResumeWatch: the Linux timerfd detector, else a plain timer. Tests inject
	// a fake to drive a resume without CAP_SYS_TIME.
	newWatch func() resumeWatch

	// thawSpacing is the floor on how often a detected clock step may drive a
	// thaw (defaults to minThawSpacing). A field only so tests can shrink it —
	// production never sets it. See minThawSpacing for why the floor exists.
	thawSpacing time.Duration

	// resumeMinWall is the minimum NEW wall-vs-monotonic divergence a clock step
	// must carry to count as a resume (defaults to defaultResumeMinWall). A field
	// only so tests can move it — production never sets it.
	resumeMinWall time.Duration

	// divergence reports the total wall-vs-monotonic divergence accumulated since
	// a reference time (defaults to unaccountedWall).
	//
	// It is injectable because no test can produce a real one: driving the two
	// clocks apart needs CAP_SYS_TIME, and Go offers no way to synthesise a
	// time.Time whose monotonic reading moves independently of its wall reading.
	// That is the same reason thawDelayFrom is split out from thawDelay. Now that
	// the figure GATES the thaw rather than merely being logged, the seam has to
	// exist or the gate is untestable.
	divergence func(since time.Time) time.Duration

	// pullMu is the resume interlock between the two goroutines. It is held by
	// pullLoop while it mints the per-request context for a poll (i.e. it must be
	// ACQUIRED before any poll is issued), and by thaw across cancel +
	// CloseIdleConnections. That is what orders the two — see
	// cancelPollAndDropPool for why hoping for the right order is not enough.
	//
	// It guards pullCancel, and nothing else: it is NOT a lock over the reconcile
	// or over any peer state.
	pullMu sync.Mutex
	// pullDone is closed when the in-flight poll's PullConfig has RETURNED (nil
	// before the first poll). Cancelling is not the same as finished — see
	// cancelPollAndDropPool — so the drop waits on this, not on cancel().
	pullDone chan struct{}
	// pullCancel cancels the config poll currently in flight (nil before the
	// first one). It is the handle thaw pulls to wake pullLoop; see pullOnce.
	// Calling it after the poll has already returned is a no-op, which is what
	// makes thaw safe to fire whatever the loop happens to be doing.
	pullCancel context.CancelFunc

	// Tunables (defaulted by Run; overridden in tests).
	HeartbeatInterval time.Duration
	PollFloor         time.Duration
	BackoffMin        time.Duration
	BackoffMax        time.Duration
	// DetachedKeepWarm is how long a fully-detached box with a held PTY still
	// reports interactive liveness (so a detached job keeps the box warm before
	// it idle-parks). Measured from LastDetachAt. Default ~3h.
	DetachedKeepWarm time.Duration
}

func (s *Supervisor) defaults() {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.SSHSessions == nil {
		s.SSHSessions = func() int { return 0 }
	}
	if s.HeartbeatInterval == 0 {
		s.HeartbeatInterval = 30 * time.Second
	}
	if s.PollFloor == 0 {
		s.PollFloor = 1 * time.Second
	}
	if s.BackoffMin == 0 {
		s.BackoffMin = 2 * time.Second
	}
	if s.BackoffMax == 0 {
		s.BackoffMax = 30 * time.Second
	}
	if s.DetachedKeepWarm == 0 {
		s.DetachedKeepWarm = 3 * time.Hour
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newWatch == nil {
		s.newWatch = func() resumeWatch { return newResumeWatch(s.Log) }
	}
	if s.thawSpacing == 0 {
		s.thawSpacing = minThawSpacing
	}
	if s.resumeMinWall == 0 {
		s.resumeMinWall = defaultResumeMinWall
	}
	if s.divergence == nil {
		s.divergence = unaccountedWall
	}
}

// interactiveLive folds the session-liveness axes into the heartbeat's
// interactive flag:
//
//	interactiveLive = AttachedClients() > 0
//	               || (HeldLivePTYs() > 0 AND within DetachedKeepWarm of the last detach)
//
// A box with attached clients is plainly live. A box whose sessions are all
// detached but still hold a live PTY stays "live" for the keep-warm window (so
// a detached build/test keeps the box warm), then lets it idle-park. With no
// session module wired the accessors are nil and this returns false (raw-conn
// liveness still rides ssh_sessions separately).
func (s *Supervisor) interactiveLive() bool {
	if s.AttachedClients != nil && s.AttachedClients() > 0 {
		return true
	}
	if s.HeldLivePTYs != nil && s.HeldLivePTYs() > 0 {
		if s.LastDetachAt == nil {
			return false
		}
		last := s.LastDetachAt()
		if last.IsZero() {
			// A held PTY that was never detached (e.g. created then the only
			// client dropped before any clean detach) — treat as within window.
			return true
		}
		return time.Since(last) < s.DetachedKeepWarm
	}
	return false
}

// Run starts the two loops and blocks until ctx is cancelled. There is no
// readyLoop: the heartbeat IS the readiness assertion (it carries s.Identity),
// so readiness, liveness, and config-pull are all continuous and self-healing.
func (s *Supervisor) Run(ctx context.Context) {
	s.defaults()
	go s.heartbeatLoop(ctx)
	// The config-pull reconcile runs on the MAIN goroutine — for crash
	// containment it is deliberately NOT recovered (a panic there exits the
	// process, the intended behavior; recovery is only for the side goroutines
	// the agent spawns directly).
	s.pullLoop(ctx)
}

// heartbeatLoop is one of the two goroutines the agent spawns directly, so for
// crash containment its body is wrapped in recover(): an unrelated panic here
// must not exit the whole process (and end every persistent session).
//
// The wait between beats is a resumeWatch (resumewatch.go), not a monotonic
// ticker. A monotonic timer PAUSES across a Fly RAM-snapshot suspend, so it
// would sit the box out a full interval after a resume — up to 30s during which
// the cluster cannot flip the row to `running` and attach 409s. The watch instead
// wakes on either an absolute WALL-clock deadline or, on Linux, the realtime
// clock STEP the platform performs at resume (FIX-280).
func (s *Supervisor) heartbeatLoop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("heartbeat loop panic recovered", "panic", r)
		}
	}()
	w := s.newWatch()
	defer func() { _ = w.Close() }()

	// degrade is the SINGLE disposition for every watch malfunction, whether it
	// surfaces from Arm or from Wait: swap in the plain timer, once. Detection is
	// lost until the agent restarts; the heartbeat's cadence is not.
	//
	// One policy rather than two on purpose. A per-error-class policy would have
	// to be right about which failures are transient, and it is not worth being
	// wrong: a watch that fails permanently would then re-log its ERROR every
	// interval forever, burying the very line that is supposed to be the alert.
	//
	// This must never be reachable from an ordinary clock step — see Arm's
	// contract, where ECANCELED is a report and not a failure.
	degrade := func(stage string, err error) {
		s.Log.Error("resume watch failed, degrading to a plain timer — warm resumes "+
			"will no longer be detected until the agent restarts",
			"stage", stage, "err", err)
		_ = w.Close()
		w = newTimerWatch()
	}

	// rearm re-anchors the wake one interval out and reports whether a step was
	// latched since the last arm or wait.
	//
	// It is called AFTER the iteration's work, so the next beat is a full idle
	// interval away — the same guarantee the pre-FIX-280 loop gave by computing
	// its deadline after beat(). Anchoring before the work instead would let a
	// beat that burns its whole retry budget be followed immediately by another.
	//
	// Arming after the work is only safe because Arm REPORTS a step it consumes
	// (timerfd_settime returns ECANCELED and rearms anyway). That report is the
	// channel covering steps that land while the loop is busy; Wait covers the
	// rest. Between them there is no window in which a resume is missed — which
	// matters because the Fly suspend a resume undoes is triggered BY a beat, so
	// the step lands in the busy window by construction.
	rearm := func() (stepped bool) {
		stepped, err := w.Arm(time.Now().Add(s.HeartbeatInterval))
		if err == nil {
			return stepped
		}
		// An arm that failed leaves nothing to wait on: an unarmed pollable
		// timerfd never becomes readable, so a Wait on it would return only when
		// ctx is cancelled — i.e. the box would stop beating for good.
		degrade("arm", err)
		_, _ = w.Arm(time.Now().Add(s.HeartbeatInterval)) // timerWatch cannot fail
		return false
	}

	// Prime the watch before the first beat so cancel-latching is active: until
	// the first Arm the kernel does not mark the fd on a clock step at all.
	_ = rearm()
	pendingStep := false
	// lastThawAt paces thaws (see thawSpacing). Zero ⇒ never thawed, so the first
	// step is never delayed.
	var lastThawAt time.Time
	// stepChannel records WHICH of the two channels reported the step ("arm" or
	// "wait"). It rides the log line because the design's expectation — that a
	// park lands while the loop is busy, so "arm" is the common case — is
	// otherwise unfalsifiable in production.
	stepChannel := ""
	// stepMagnitude is the divergence the pending step carried — the suspend
	// duration, and the value the detection line reports.
	var stepMagnitude time.Duration

	// A clock STEP is not a resume. CANCEL_ON_SET latches on any clock SET, and
	// on Fly the platform sets the guest CLOCK_REALTIME continuously — measured on
	// prod 2026-07-29, once every few seconds on every running machine, INCLUDING
	// one that had never been suspended (FIX-292). Treating each as a resume drove
	// a thaw — four control-plane requests — every 5s per box forever, bounded only
	// by thawSpacing: exactly the append-rate blowup minThawSpacing exists to name.
	//
	// What separates the two is MAGNITUDE, not frequency. A Fly RAM-snapshot resume
	// pauses CLOCK_MONOTONIC while the wall clock advances, so a genuine resume
	// carries new wall-vs-monotonic divergence equal to the park (prod: 465254ms
	// for a 464s park). A plain clock set carries none (prod: 0ms, every time).
	//
	// newDivergence returns the divergence accumulated since the PREVIOUS call and
	// consumes it. Measuring incrementally rather than against the live arm is what
	// makes the second channel honest: divergence is permanent once a suspend has
	// happened, so any later reading taken against a pre-suspend reference still
	// contains that suspend — which is why the arm channel re-reported an already
	// thawed 465254ms resume on prod. Consuming also stops sub-threshold noise from
	// accumulating across hours into a false positive.
	epoch := time.Now()
	var accountedWall time.Duration
	newDivergence := func() time.Duration {
		total := s.divergence(epoch)
		d := total - accountedWall
		accountedWall = total
		return d
	}
	// absorbed counts sub-threshold sets since the last beat, so the rate the
	// platform sets the clock at stays visible rather than becoming silence.
	absorbed := 0
	// lastAbsorbAt bounds the cost of absorbing (see absorbSpacing).
	var lastAbsorbAt time.Time
	for {
		if pendingStep {
			pendingStep = false
			// Rate-limit, NOT a detection threshold: the first step after a quiet
			// period thaws immediately, so a genuine warm resume pays nothing. Only
			// a box whose clock is being stepped over and over — a user workload
			// faking time, an `ntpdate` loop — is held to the floor, and it is held
			// by waiting rather than by dropping the step.
			if d := thawDelay(lastThawAt, s.thawSpacing); d > 0 {
				// Deliberately NOT worded "resume-from-suspend detected": §5's
				// acceptance greps that exact phrase, and a paced duplicate would
				// otherwise read as a detection with no magnitude field attached.
				s.Log.Warn("thaw paced by the minimum spacing floor", "wait", d)
				t := time.NewTimer(d)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
			// The magnitude IS the suspend duration, the per-claim figure FIX-280's
			// prod measurement lacked.
			s.Log.Info("resume-from-suspend detected",
				"trigger", "realtime-clock-step",
				"channel", stepChannel,
				"unaccounted_wall_ms", stepMagnitude.Milliseconds())
			// thaw's forced beat IS this iteration's beat — it is the whole point
			// (it flips the row to `running`), so there is no second beat here.
			s.thaw(ctx)
			lastThawAt = time.Now()
			if ctx.Err() != nil {
				return
			}
		} else {
			if absorbed > 0 {
				s.Log.Info("absorbed realtime-clock sets carrying no suspend",
					"count", absorbed, "min_wall", s.resumeMinWall)
				absorbed = 0
			}
			s.beat(ctx)
		}

		if rearm() {
			// A step latched while we were beating or thawing. The Fly suspend a
			// resume undoes is triggered BY a beat, so a genuine resume lands in
			// this window by construction — but so does every platform clock set,
			// hence the same magnitude test as the wait channel.
			if d := newDivergence(); d >= s.resumeMinWall {
				stepMagnitude, stepChannel = d, "arm"
				pendingStep = true
				continue
			}
			absorbed++
		}

		// Wait for a wake that means something. A sub-threshold set is absorbed
		// HERE, without falling through to the top of the loop: that would beat on
		// every clock set, turning a thaw spin into a heartbeat spin. The armed
		// deadline is untouched by a consumed cancellation, so waiting again simply
		// resumes the same interval.
	waiting:
		for {
			switch w.Wait(ctx) {
			case wakeCancelled:
				return
			case wakeBroken:
				degrade("wait", nil) // the watch already logged the underlying error
				break waiting
			case wakeClockStep:
				if d := newDivergence(); d >= s.resumeMinWall {
					stepMagnitude, stepChannel = d, "wait"
					pendingStep = true
					break waiting
				}
				absorbed++
				if !s.pauseAbsorb(ctx, lastAbsorbAt) {
					return
				}
				lastAbsorbAt = time.Now()
			case wakeDeadline:
				break waiting
			}
		}
	}
}

// pauseAbsorb bounds what a clock-set storm can cost. Absorbing is cheap — a read
// and two clock reads, no I/O — but a workload stepping the clock in a tight loop
// (the `date -s` loop minThawSpacing was written for, now with CAP_SYS_TIME inside
// a devbox) would otherwise wake this loop as fast as it can issue the syscall, and
// a 1-vCPU box has no headroom to spare for that.
//
// Spacing absorbs at absorbSpacing caps the cost at a bounded wake rate. It cannot
// delay a resume by more than absorbSpacing, which is nothing against the 30s beat
// it replaces — and only ever applies BETWEEN two sub-threshold sets, never before
// the first one.
//
// Reports false when ctx ended, i.e. the caller must return.
func (s *Supervisor) pauseAbsorb(ctx context.Context, lastAbsorbAt time.Time) bool {
	if lastAbsorbAt.IsZero() {
		return ctx.Err() == nil
	}
	d := absorbSpacing - time.Since(lastAbsorbAt)
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// thawFirstRetry is the first backoff the WARM-RESUME beat uses, before handing
// back to the ordinary BackoffMin ladder.
//
// The ordinary ladder is sized for a control plane under load: wait 2s, then 4s,
// then 8s, so a fleet of agents does not pile onto a recovering API. A resume's
// first heartbeat failure is a different animal — the socket it went down was
// killed while the box was frozen, and the retry is a fresh dial. There is no
// server to be gentle with, and the beat is the single write that flips the row
// to `running`, so every millisecond it is late is a millisecond attach 409s.
// So the first retry waits ~200ms instead of BackoffMin (2s): a resume whose
// first beat dies on a stale socket becomes attachable roughly 1.8s sooner. It
// is not a saving of 200ms — 200ms is the new wait. A beat whose first attempt
// SUCCEEDS pays nothing at all; on the failing path the shorter first rung may
// fit one extra attempt inside the HeartbeatInterval deadline. If that retry
// also fails, the failure is plausibly the server's after all and the 2s ladder
// takes over unchanged.
const thawFirstRetry = 200 * time.Millisecond

// pollReturnWait bounds how long the resume path waits for a cancelled poll to
// actually return before dropping the pool anyway. Overrunning it costs a no-op
// drop — the pre-fix behaviour — not a hang, and the beat that makes the box
// attachable must not queue behind a poll that will not come back.
const pollReturnWait = 250 * time.Millisecond

// streamCleanupSettle gives net/http's cleanupWriteRequest a moment to land. It
// runs on the request's own goroutine AFTER RoundTrip returns, and it is what
// calls forgetStreamID — so without this pause closeIfIdle still sees a
// connection carrying a stream and declines to close it. Measured: 0/5 evictions
// without it, 5/5 with it. See cancelPollAndDropPool.
const streamCleanupSettle = 25 * time.Millisecond

// beat sends one heartbeat (retrying a transient failure, §4.1 Lever 1) and
// fires the piggybacked session/peer refreshes. It is the steady-cadence loop
// body factored out; the warm-resume variant is thawBeat.
func (s *Supervisor) beat(ctx context.Context) {
	s.sendHeartbeat(ctx, s.BackoffMin)
	// SyncSessions piggybacks the heartbeat cadence (the Manager also fires it on
	// attach/detach). A snapshot of all live sessions, no terminal bytes. Fires
	// once, after the retry loop settles.
	if s.SyncSessions != nil {
		s.SyncSessions()
	}
}

// thawBeat is beat's warm-resume variant. Two deliberate differences from the
// cadence beat, both about the resume critical path:
//
//   - the first retry is thawFirstRetry rather than BackoffMin (see there);
//   - the session sync is fired asynchronously rather than inline.
//
// The retry itself stays INSIDE the beat — a forced beat that gave up on its
// first transient failure would leave the box unattachable for a whole
// HeartbeatInterval, which is the entire cost thaw exists to avoid.
func (s *Supervisor) thawBeat(ctx context.Context) {
	s.sendHeartbeat(ctx, thawFirstRetry)
	s.syncSessionsAsync()
}

// syncSessionsAsync fires the session snapshot on its own goroutine.
//
// SyncSessions contributes nothing to readiness and nothing to the peer set: it
// is a projection of box-side session metadata that the control plane self-heals
// on the next 30s beat anyway. In production it is sessions.Manager.SyncNow — a
// POST on its own 15s budget — so running it inline on the resume path put up to
// 15s of a measured 85s thaw in front of work that actually mattered. The
// Manager already fires the same call from a goroutine on attach and on detach,
// so this is the established shape rather than a new one.
//
// This POST does hold an h2 stream while it is in flight, and a held stream is
// what makes CloseIdleConnections a no-op — so a sync overlapping a LATER
// resume's pool drop would blunt that drop. It is deliberately not gated for
// that, because a gate here could not deliver the property: sessions.Manager
// spawns the very same SyncNow from touchAttach and touchDetach, on attach and
// detach — i.e. exactly around a connect — so a stream can be held across a drop
// through a door the supervisor does not own. The drop is best-effort by
// construction; what actually guarantees a dead connection goes away is the
// HTTP/2 health-check ping config on the transport (see api.New). Adding a gate
// that narrows one of several doors would buy the appearance of an invariant
// rather than the invariant.
//
// The goroutine carries a recover() for the same crash-containment reason
// heartbeatLoop does: a panic in a side goroutine the agent spawns must not take
// the process — and every persistent session — down with it.
func (s *Supervisor) syncSessionsAsync() {
	sync := s.SyncSessions
	if sync == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.Log.Error("thaw session sync panic recovered", "panic", r)
			}
		}()
		start := time.Now()
		sync()
		// sync_ms lives on its own line rather than on thaw's, because after this
		// restructure it is no longer a component of the thaw's duration — that is
		// the point of firing it here.
		s.Log.Info("thaw session sync complete", "sync_ms", time.Since(start).Milliseconds())
	}()
}

// jittered spreads a backoff so a fleet of agents recovering from the same API
// blip does not thundering-herd it. Returns the TOTAL wait (backoff + jitter),
// because every caller needs the same figure twice — once to log or to test
// against a deadline, once to sleep — and computing it twice would make the
// logged number a lie.
//
// The guard is not decorative: rand.Int63n panics on a non-positive bound, so a
// backoff configured below 2ns would take the process down from inside the error
// path. Nothing configures one today; this makes that unable to matter.
func jittered(backoff time.Duration) time.Duration {
	half := int64(backoff) / 2
	if half <= 0 {
		return backoff
	}
	return backoff + time.Duration(rand.Int63n(half))
}

// sleepCtx waits for d, reporting false if the context ended first (in which
// case the caller must unwind rather than continue).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// sendHeartbeat sends one heartbeat, retrying a transient failure with the same
// jittered-exponential backoff the pull loop uses (BackoffMin→BackoffMax). The
// retry is bounded by a deadline captured ONCE at entry — the shape the
// pre-FIX-280 waitOrThaw used (now.Add(HeartbeatInterval)) — so retries can
// never slide into the next
// scheduled beat (preserving at-most-one-append-in-flight-per-box). It stops
// when the next backoff sleep would cross that deadline, logging a DISTINCT
// "heartbeat retries exhausted" line for recurrence alerting. A ctx cancellation
// during a backoff sleep returns promptly via the select on ctx.Done().
//
// firstBackoff is the FIRST retry's wait; every retry after it climbs the
// ordinary BackoffMin→BackoffMax ladder. The cadence beat passes BackoffMin, so
// its behaviour is bit-for-bit what it was; thaw passes thawFirstRetry.
func (s *Supervisor) sendHeartbeat(ctx context.Context, firstBackoff time.Duration) {
	// interactive_live folds session liveness (attached clients, or a held PTY
	// within the keep-warm window) — the box-observed signal driving idle-suspend.
	// ssh_sessions rides along as the raw authorized-connection count (the api
	// re-folds it defensively). Every beat also re-asserts s.Identity so the
	// cluster can flip the row to running (provisioned/starting) idempotently.
	deadline := s.now().Add(s.HeartbeatInterval)
	backoff := firstBackoff
	for {
		ssh := s.SSHSessions()
		err := s.API.Heartbeat(ctx, s.interactiveLive(), ssh, s.Identity)
		if err == nil {
			return
		}
		s.Log.Warn("heartbeat failed", "err", err)
		wait := jittered(backoff)
		// Stop before a sleep that would cross the deadline captured at entry —
		// a retry must not overlap the next scheduled beat. The loop's work-END
		// rearm then waits its own fresh interval (INV-5), and INV-R heals the
		// box on the next successful beat.
		if s.now().Add(wait).After(deadline) {
			s.Log.Warn("heartbeat retries exhausted", "err", err)
			return
		}
		if !sleepCtx(ctx, wait) {
			return
		}
		// Climb the ordinary ladder. The max() is what makes a sub-BackoffMin
		// first retry (thawFirstRetry) hand back to it rather than doubling its
		// own way up from 200ms: 200ms → 2s → 4s → 8s. On the cadence path backoff
		// starts at BackoffMin, so backoff*2 always wins the max and this is
		// exactly the pre-existing min(backoff*2, BackoffMax).
		backoff = min(max(backoff*2, s.BackoffMin), s.BackoffMax)
	}
}

// pullOnce issues ONE config poll, under the resume interlock.
//
// The per-request context is a child of the run context, and its cancel handle
// is published on the Supervisor so thaw can reach it. That is what turns "the
// box resumed" into "the poll that has been asleep since before the freeze ends
// NOW" — see cancelPollAndDropPool, and thaw for why the stale poll would
// otherwise sit there for the balance of its 35s client timeout.
//
// pullMu is acquired BEFORE the request is issued and released before blocking
// on it, so a thaw holding the lock cannot be starved by a 25s long-poll, and a
// poll cannot be issued while a thaw is mid-drop. If a cancel lands in the gap
// between the unlock and the call, the request context is already Done when
// PullConfig runs: net/http's Transport.roundTrip checks ctx.Done() at the top
// of its loop and returns before it ever reaches the connection pool, so no
// stream is opened on the doomed ClientConn either way.
func (s *Supervisor) pullOnce(ctx context.Context, cursor string) (*api.Config, error) {
	s.pullMu.Lock()
	reqCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.pullCancel, s.pullDone = cancel, done
	s.pullMu.Unlock()
	// close(done) AFTER PullConfig has returned. That is the signal
	// cancelPollAndDropPool waits on: a cancelled request is not finished when
	// cancel() returns, and the pool cannot be dropped until it is.
	defer func() { cancel(); close(done) }()
	return s.API.PullConfig(reqCtx, cursor)
}

// cancelPollAndDropPool is thaw's first act, and it is ONE act: cancel the
// in-flight config poll, then drop the idle connection pool, with pullLoop
// locked out in between.
//
// Three steps, and the order is fixed. The drop is what evicts the sockets that
// died while the box was frozen; the cancel is what lets the drop work at all,
// because Transport.CloseIdleConnections reaches http2ClientConn.closeIfIdle,
// which bails out while the conn still carries streams — and the long-poll holds
// one essentially always (see thaw).
//
// The third step is the one that is easy to omit, and omitting it silently
// undoes the other two. **Cancelling a request does not synchronously remove its
// stream.** net/http's roundTrip aborts the stream and returns, but the stream is
// unregistered by forgetStreamID inside cleanupWriteRequest, which runs on the
// request's own goroutine AFTER RoundTrip has already returned. A drop issued on
// the next line therefore still sees len(cc.streams) > 0 and is a no-op. Measured
// on a real h2 server: cancel-then-drop evicted the connection in 0 of 5 runs,
// and so did waiting merely for PullConfig to return; waiting for the poll to
// return AND letting the cleanup settle evicted it in 5 of 5. On a 1-vCPU box
// (GOMAXPROCS=1) the bad case is near-deterministic, not a narrow race: close(ctx.done)
// only enqueues the waiter while this goroutine runs straight on into the drop.
// So: cancel, wait for the poll to actually return, let the cleanup land, then
// drop. TestThawForcedBeatLandsOnAFreshConnection is the end-to-end proof, and it
// counts connections server-side because that is the only observation that can
// distinguish a working drop from a no-op.
//
// The interlock is the part that is easy to get wrong. cancel() wakes pullLoop
// on a DIFFERENT goroutine, and pullLoop's whole job on waking is to re-poll
// immediately. If it wins that race it opens a new stream on the very
// ClientConn we are about to drop, closeIfIdle sees len(cc.streams) > 0, and the
// drop is a no-op again — the exact bug, restored, with the added insult that we
// caused it ourselves. Holding pullMu across both calls makes the sequence a
// guarantee rather than a hope: the woken loop blocks in pullOnce until
// CloseIdleConnections has returned.
//
// One thing this does NOT do is shorten a sleep. If pullLoop happens to be in
// its backoff or poll-floor wait rather than in a request, there is nothing in
// flight to cancel and the kick lands whenever that wait ends. Deliberate: the
// long-poll holds the connection essentially always — that is precisely what
// makes the pool drop a no-op in the first place — and correctness is unaffected
// either way, since the next poll is issued after the drop and so dials fresh.
func (s *Supervisor) cancelPollAndDropPool() {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	// pullCancel and pullDone are published together under pullMu, so they are
	// nil together — one guard covers both.
	if s.pullCancel != nil {
		s.pullCancel()
		// Bounded: a poll that will not return must not hold up the beat that
		// makes the box attachable. Overrunning this costs a no-op drop, which is
		// the pre-fix behaviour, not a hang.
		select {
		case <-s.pullDone:
		case <-time.After(pollReturnWait):
		}
		// And the settle for cleanupWriteRequest, which runs after RoundTrip
		// returns and is what actually calls forgetStreamID.
		time.Sleep(streamCleanupSettle)
	}
	s.API.CloseIdleConnections()
}

// pullLoop is the config-pull reconcile loop: long-poll, reconcile on every
// 200 (idempotent full replacement — also the steady-state self-heal), keep
// the cursor on 304, back off jittered on errors, and never poll faster than
// the floor.
//
// It is also the SOLE owner of peer reconciliation. thaw used to reconcile too,
// from the heartbeat goroutine, with nothing synchronising the two — see thaw
// for what that cost.
func (s *Supervisor) pullLoop(ctx context.Context) {
	cursor := ""
	backoff := s.BackoffMin
	for {
		start := time.Now()
		cfg, err := s.pullOnce(ctx, cursor)
		switch {
		case err == nil:
			if rerr := s.Reconcile.Reconcile(cfg.Peers); rerr != nil {
				// HOLD the cursor, and PACE the retry. Both halves are required.
				//
				// Holding it: the cursor is the server's content hash, so advancing
				// it after a reconcile that did not happen asserts "I am holding
				// this state" when the box is not. The server would then 304 every
				// later poll and the failure would never be retried — and since a
				// failed pass can leave a peer wholly absent from wg0 (see
				// wgnet.Reconcile), that is a durable strand, exactly the class this
				// change set exists to remove.
				//
				// Pacing it: a held cursor means the server no longer long-polls at
				// all. livepoll returns 200 the instant the supplied cursor differs
				// from the rendered content, which it always does while we hold a
				// stale one. So without a sleep here the loop would run
				// pull→reconcile→PollFloor→repeat at ~1 Hz forever on any box whose
				// `wg` calls keep failing — re-running the whole fork/exec sequence
				// each pass, against a control plane serving the whole fleet, in
				// exactly the memory-pressured conditions that caused the failure.
				// The retry therefore rides the same jittered ladder a pull error
				// uses, so a persistent failure costs ~1 request per BackoffMax.
				// Written out rather than routed through this package's jittered/
				// sleepCtx helpers ON PURPOSE. Those are introduced by the PR below
				// this one in the stack; calling them from here would mean reverting
				// that PR alone no longer compiles, and the rollout explicitly
				// promises it is independently revertible. A few duplicated lines
				// are cheaper than a revert that breaks the build at 3am.
				half := int64(backoff) / 2
				wait := backoff
				if half > 0 {
					wait += time.Duration(rand.Int63n(half))
				}
				s.Log.Error("peer reconcile failed; holding the cursor so the next poll re-applies",
					"err", rerr, "backoff", wait)
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
				backoff = min(backoff*2, s.BackoffMax)
				continue
			}
			cursor = cfg.Cursor
			s.Log.Info("peers reconciled", "count", len(cfg.Peers), "cursor", cursor)
			backoff = s.BackoffMin
		case errors.Is(err, api.ErrNotModified):
			backoff = s.BackoffMin
		case ctx.Err() != nil:
			// The RUN context ended: shut down. This case MUST be tested before
			// the context.Canceled one below, because a cancelled run context
			// cancels the per-request child too, so the error the poll returns at
			// shutdown is indistinguishable from a resume kick by inspecting the
			// error alone. Reading ctx (the run context) is what tells them apart.
			// Get this backwards and the loop re-polls forever at shutdown, at
			// whatever rate the cancelled context returns — a spin, not a loop.
			return
		case errors.Is(err, context.Canceled):
			// A resume cancelled this poll (cancelPollAndDropPool) while the run
			// context is still live. The cancel IS the kick: the whole point is to
			// replace a snapshot taken before the freeze with a fresh one NOW, so
			// re-poll with neither a backoff sleep (this is not a failure — nothing
			// is wrong with the server) nor the poll-rate floor (the floor exists to
			// stop a fast-answering server busy-looping the agent, and a resume is
			// neither fast nor frequent: thawSpacing already bounds it).
			//
			// The cursor is deliberately KEPT. It is the loop's own, threaded from
			// its last successful pull; re-polling with it asks the right question
			// ("anything newer?"), and a 304 answer is a correct answer.
			s.Log.Info("config poll cancelled by a resume; re-polling immediately")
			backoff = s.BackoffMin
			continue
		default:
			wait := jittered(backoff)
			s.Log.Warn("config pull failed", "err", err, "backoff", wait)
			if !sleepCtx(ctx, wait) {
				return
			}
			backoff = min(backoff*2, s.BackoffMax)
		}
		// Poll-rate floor: a long-poll that answers instantly (empty config,
		// dead-poll server bug) must not become a busy loop.
		if elapsed := time.Since(start); elapsed < s.PollFloor {
			if !sleepCtx(ctx, s.PollFloor-elapsed) {
				return
			}
		}
	}
}
