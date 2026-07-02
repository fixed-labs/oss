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
func (s *Supervisor) heartbeatLoop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("heartbeat loop panic recovered", "panic", r)
		}
	}()
	t := time.NewTicker(s.HeartbeatInterval)
	defer t.Stop()
	for {
		// interactive_live folds session liveness (attached clients, or a held
		// PTY within the keep-warm window) — the box-observed signal driving idle-
		// suspend. ssh_sessions rides along as the raw authorized-connection count
		// (the api re-folds it defensively). Every beat also re-asserts s.Identity
		// so the cluster can flip the row to running (provisioned/starting)
		// idempotently — a dropped beat self-heals next tick.
		ssh := s.SSHSessions()
		if err := s.API.Heartbeat(ctx, s.interactiveLive(), ssh, s.Identity); err != nil {
			s.Log.Warn("heartbeat failed", "err", err)
		}
		// SyncSessions piggybacks the heartbeat cadence (the Manager also fires it
		// on attach/detach). A snapshot of all live sessions, no terminal bytes.
		if s.SyncSessions != nil {
			s.SyncSessions()
		}
		// Prune strands from the broker discovery file: re-publish only live
		// connections. Rides the heartbeat cadence (the pull loop only rewrites on a
		// peer-set change).
		if s.RefreshLivePeers != nil {
			s.RefreshLivePeers()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
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
