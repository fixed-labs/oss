// Package supervisor runs the agent's two loops, using the same hardened
// pull-reconcile shape the relay uses:
//
//   - pull-reconcile: long-poll GET agent-config with a cursor; reconcile
//     wg0's peer set on EVERY successful pull (steady-state self-heal);
//     jittered exponential backoff on errors; a poll-rate
//     floor so a server that answers instantly can't busy-loop the agent.
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

// beat sends one heartbeat (retrying a transient failure, §4.1 Lever 1) and
// fires the piggybacked session/peer refreshes. It is the loop body factored out
// so the thaw hook can force one out of cadence — the retry lives HERE so the
// warm-resume thaw-forced beat (thaw() calls s.beat) is covered for free.
func (s *Supervisor) beat(ctx context.Context) {
	s.sendHeartbeat(ctx)
	// SyncSessions piggybacks the heartbeat cadence (the Manager also fires it on
	// attach/detach). A snapshot of all live sessions, no terminal bytes. Fires
	// once, after the retry loop settles.
	if s.SyncSessions != nil {
		s.SyncSessions()
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
func (s *Supervisor) sendHeartbeat(ctx context.Context) {
	// interactive_live folds session liveness (attached clients, or a held PTY
	// within the keep-warm window) — the box-observed signal driving idle-suspend.
	// ssh_sessions rides along as the raw authorized-connection count (the api
	// re-folds it defensively). Every beat also re-asserts s.Identity so the
	// cluster can flip the row to running (provisioned/starting) idempotently.
	deadline := s.now().Add(s.HeartbeatInterval)
	backoff := s.BackoffMin
	for {
		ssh := s.SSHSessions()
		err := s.API.Heartbeat(ctx, s.interactiveLive(), ssh, s.Identity)
		if err == nil {
			return
		}
		s.Log.Warn("heartbeat failed", "err", err)
		// Jitter the re-arm so a fleet of agents doesn't thundering-herd a
		// recovering API (same idiom as pullLoop).
		jitter := time.Duration(rand.Int63n(int64(backoff) / 2))
		// Stop before a sleep that would cross the deadline captured at entry —
		// a retry must not overlap the next scheduled beat. The loop's work-END
		// rearm then waits its own fresh interval (INV-5), and INV-R heals the
		// box on the next successful beat.
		if s.now().Add(backoff + jitter).After(deadline) {
			s.Log.Warn("heartbeat retries exhausted", "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff + jitter):
		}
		backoff = min(backoff*2, s.BackoffMax)
	}
}

// pullLoop is the config-pull reconcile loop: long-poll, reconcile on every
// 200 (idempotent full replacement — also the steady-state self-heal), keep
// the cursor on 304, back off jittered on errors, and never poll faster than
// the floor.
func (s *Supervisor) pullLoop(ctx context.Context) {
	cursor := ""
	backoff := s.BackoffMin
	for {
		start := time.Now()
		cfg, err := s.API.PullConfig(ctx, cursor)
		switch {
		case err == nil:
			cursor = cfg.Cursor
			if rerr := s.Reconcile.Reconcile(cfg.Peers); rerr != nil {
				s.Log.Error("peer reconcile failed", "err", rerr)
			} else {
				s.Log.Info("peers reconciled", "count", len(cfg.Peers), "cursor", cursor)
			}
			backoff = s.BackoffMin
		case errors.Is(err, api.ErrNotModified):
			backoff = s.BackoffMin
		case ctx.Err() != nil:
			return
		default:
			// Jitter the re-arm so a fleet of agents doesn't thundering-herd a
			// recovering API.
			jitter := time.Duration(rand.Int63n(int64(backoff) / 2))
			s.Log.Warn("config pull failed", "err", err, "backoff", backoff+jitter)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff + jitter):
			}
			backoff = min(backoff*2, s.BackoffMax)
		}
		// Poll-rate floor: a long-poll that answers instantly (empty config,
		// dead-poll server bug) must not become a busy loop.
		if elapsed := time.Since(start); elapsed < s.PollFloor {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.PollFloor - elapsed):
			}
		}
	}
}
