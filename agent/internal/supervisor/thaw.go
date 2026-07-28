package supervisor

import (
	"context"
	"time"
)

// waitOrThaw waits out one heartbeat interval, but wakes on the short ThawPoll
// sub-tick to watch for a resume-from-Fly-RAM-snapshot suspend. It returns true
// when the caller should beat again, false when ctx is cancelled.
//
// Why not a plain time.Ticker(HeartbeatInterval): time.Now()'s monotonic reading
// PAUSES while a Fly machine is suspended, so a monotonic timer thinks almost no
// time passed on resume and would sit the box out its full interval before the
// next beat — the cluster couldn't flip starting → running for up to 30s. A real
// suspend is instead visible as a WALL-clock jump with (almost) no matching
// monotonic advance (probe: wall +164s, monotonic +36.5s across one suspend).
//
// Each sub-poll compares the wall delta (from s.now, monotonic stripped) against
// the monotonic delta (from the SEPARATE s.monoNow source) since the previous
// sample:
//   - wallDelta = now.Round(0).Sub(prev.Round(0))       // Round(0) strips monotonic → wall-only
//   - monoDelta = monoNow - prevMono                     // independent monotonic-elapsed source
//   - unaccounted = wallDelta - monoDelta
//
// The two clocks are separate injectables so a test can drive wall and monotonic
// apart (see the Supervisor.now/monoNow doc) — a single time.Time clock cannot
// express the divergence a real suspend produces.
//
// Under normal operation both deltas track the ~ThawPoll interval, so unaccounted
// stays a few ms (scheduler jitter) — far below ThawThreshold, no false positive
// even at the 30s heartbeat boundary. A suspend of at least ThawThreshold seconds
// leaves unaccounted ≥ that, and we treat it as a warm resume: force an immediate
// heartbeat and resync the relay/attachment (thaw()), then return so the loop
// re-beats without waiting out the rest of the interval.
//
// This detector is a warm-resume-only path: a cold boot re-runs the init script
// and starts a fresh agent, so the process never observes a jump. It is safe to
// re-fire — the heartbeat and the pull→reconcile are both idempotent — so a
// second detection (e.g. a second suspend) simply re-syncs again.
func (s *Supervisor) waitOrThaw(ctx context.Context) bool {
	deadline := s.now().Add(s.HeartbeatInterval)
	prev := s.now()
	prevMono := s.monoNow()
	t := time.NewTicker(s.ThawPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			now := s.now()
			nowMono := s.monoNow()
			wallDelta := now.Round(0).Sub(prev.Round(0))
			monoDelta := nowMono - prevMono
			prev = now
			prevMono = nowMono
			if wallDelta-monoDelta >= s.ThawThreshold {
				s.Log.Info("resume-from-suspend detected",
					"wall_delta", wallDelta, "mono_delta", monoDelta)
				s.thaw(ctx)
				return ctx.Err() == nil
			}
			// The ticker's monotonic channel doesn't advance the wall deadline
			// across a suspend, so compare against the wall clock too: once a full
			// interval of wall time has elapsed, it's an ordinary heartbeat tick.
			if !now.Before(deadline) {
				return true
			}
		}
	}
}

// thaw is the warm-resume hook: on a detected snapshot resume the agent is live
// mid-flight (it did not re-run boot; the identity dir on the overlay survived),
// but its liveness and attachment state are stale. It (1) forces an immediate
// heartbeat so the cluster flips starting → running at once — the beat that also
// clears the row's :termination-reason — and (2) re-pulls the workspace config
// and reconciles wg0's peer set + fires the session sync, because the in-RAM
// WireGuard peer set and relay assignment went stale across the idle.
//
// The resync reuses the pull loop's mechanism rather than reimplementing peer
// reconciliation: PullConfig with an EMPTY cursor returns the current full peer
// set immediately (see api.PullConfig), which is exactly the fresh reconcile the
// cold-resume path performs.
func (s *Supervisor) thaw(ctx context.Context) {
	s.beat(ctx)
	cfg, err := s.API.PullConfig(ctx, "")
	if err != nil {
		// Not fatal: the steady-state pull loop keeps its own cursor and will
		// reconcile on its next successful poll; the forced beat has already made
		// the box ready.
		s.Log.Warn("thaw config pull failed", "err", err)
		return
	}
	if rerr := s.Reconcile.Reconcile(cfg.Peers); rerr != nil {
		s.Log.Error("thaw peer reconcile failed", "err", rerr)
	} else {
		s.Log.Info("thaw peers reconciled", "count", len(cfg.Peers))
	}
	if s.SyncSessions != nil {
		s.SyncSessions()
	}
}
