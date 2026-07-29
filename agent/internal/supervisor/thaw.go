package supervisor

import (
	"context"
)

// thaw is the warm-resume hook: on a detected snapshot resume the agent is live
// mid-flight (it did not re-run boot; the identity dir on the overlay survived),
// but its liveness and attachment state are stale. It (1) forces an immediate
// heartbeat so the cluster flips starting → running at once — the beat that also
// clears the row's :termination-reason — and (2) re-pulls the workspace config
// and reconciles wg0's peer set + fires the session sync, because the in-RAM
// WireGuard peer set and relay assignment went stale across the idle.
//
// The beat comes FIRST and the resync second, deliberately: the beat is what
// makes the box attachable, so no round-trip may be placed in front of it.
//
// The resync reuses the pull loop's mechanism rather than reimplementing peer
// reconciliation: PullConfig with an EMPTY cursor returns the current full peer
// set immediately (see api.PullConfig), which is exactly the fresh reconcile the
// cold-resume path performs.
//
// This is a warm-resume-only path: a cold boot re-runs the init script and starts
// a fresh agent, so the process never observes a clock step. It is safe to
// re-fire — the heartbeat and the pull→reconcile are both idempotent — which is
// what lets the detector treat ANY realtime clock step as a resume without
// trying to classify it (resumewatch.go).
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
