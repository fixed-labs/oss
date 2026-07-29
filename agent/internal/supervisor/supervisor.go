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

	// RefreshLivePeers re-publishes the broker discovery file with only the
	// currently-LIVE connections (recent WireGuard handshake), pruning strands left
	// by closed `devbox connect`s. Rides the heartbeat cadence because the config-
	// pull loop only reconciles on a peer-set CHANGE — a peer going stale never
	// triggers a rewrite otherwise. Optional: nil ⇒ no wg net (tests).
	RefreshLivePeers func()

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
	armedAt := time.Now()
	_ = rearm()
	pendingStep := false
	// stepSince is the arm the pending step is measured against — necessarily the
	// arm that was LIVE when the clock moved, which differs per channel (see the
	// two assignments below).
	stepSince := armedAt
	// lastThawAt paces thaws (see thawSpacing). Zero ⇒ never thawed, so the first
	// step is never delayed.
	var lastThawAt time.Time
	// stepChannel records WHICH of the two channels reported the step ("arm" or
	// "wait"). It rides the log line because the design's expectation — that a
	// park lands while the loop is busy, so "arm" is the common case — is
	// otherwise unfalsifiable in production, and because picking the wrong
	// stepSince for a channel fails silently (it logs ~0 rather than erroring).
	stepChannel := ""
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
			// unaccountedWall is WALL minus MONOTONIC elapsed since that arm — on a
			// resume that IS the suspend duration, the per-claim figure FIX-280's
			// prod measurement lacked.
			s.Log.Info("resume-from-suspend detected",
				"trigger", "realtime-clock-step",
				"channel", stepChannel,
				"unaccounted_wall_ms", unaccountedWall(stepSince).Milliseconds())
			// thaw's forced beat IS this iteration's beat — it is the whole point
			// (it flips the row to `running`), so there is no second beat here.
			s.thaw(ctx)
			lastThawAt = time.Now()
			if ctx.Err() != nil {
				return
			}
		} else {
			s.beat(ctx)
		}

		// liveArm is the arm that was in force throughout the work just done — the
		// one a step during that work would have been latched against.
		liveArm := armedAt
		armedAt = time.Now()
		if rearm() {
			// A resume that landed while we were beating or thawing. Handle it now
			// rather than waiting out an interval we already know is stale.
			stepSince, stepChannel = liveArm, "arm"
			pendingStep = true
			continue
		}

		switch w.Wait(ctx) {
		case wakeCancelled:
			return
		case wakeBroken:
			degrade("wait", nil) // the watch already logged the underlying error
		case wakeClockStep:
			// The step landed during the wait we just came out of, so it is measured
			// against the arm that opened it.
			stepSince, stepChannel = armedAt, "wait"
			pendingStep = true
		case wakeDeadline:
		}
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
	// Prune strands from the broker discovery file: re-publish only live
	// connections. Rides the heartbeat cadence (the pull loop only rewrites on a
	// peer-set change).
	if s.RefreshLivePeers != nil {
		s.RefreshLivePeers()
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
