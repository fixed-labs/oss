package supervisor

import (
	"context"
	"time"
)

// thaw is the warm-resume hook. On a detected snapshot resume the agent is live
// mid-flight — it did not re-run boot, and the identity dir on the overlay
// survived — but every socket it had pooled died while the box was frozen, and
// its liveness state at the cluster is stale. thaw's critical path is exactly
// three things, in this order:
//
//  1. cancel the config poll that has been in flight since before the freeze,
//     and wait for it to actually finish unwinding;
//  2. drop the idle connection pool;
//  3. force one heartbeat, so the cluster flips starting → running at once —
//     the beat that also clears the row's :termination-reason.
//
// (1) and (2) are one indivisible step (cancelPollAndDropPool). (3) comes after
// them and nothing may be placed in front of it, because it is the write that
// makes the box attachable again.
//
// One further thing happens OFF that path: the forced beat also kicks the
// session-metadata sync onto its own goroutine (syncSessionsAsync). It is
// deliberately not one of the three — it contributes nothing to readiness and
// nothing to the peer set, and running it inline is what put up to 15s of a
// measured 85s thaw in front of work that mattered.
//
// Note what is absent: thaw performs no pull and no reconcile of its own. The
// cancel in (1) IS the kick — pullLoop re-polls immediately and reconciles what
// it gets, as the sole owner of the peer set.
//
// # Why the cancel has to come first
//
// Because a pool drop performed while the long-poll still holds a stream is a
// no-op, and because cancelling is necessary but NOT sufficient to end that
// stream. cancelPollAndDropPool carries the full derivation — the net/http
// internals, and the measurements showing which orderings actually evict the
// connection and which quietly do not. Read it before changing the sequence;
// every step in it is load-bearing and two of them look redundant.
//
// # Why thaw performs no pull and no reconcile of its own
//
// Cancelling the stale poll IS the kick. pullLoop treats context.Canceled as
// "re-poll immediately" (no backoff, no poll floor), so the fresh peer set is
// fetched and reconciled by the loop that owns that job, on its own goroutine,
// with its own cursor.
//
// thaw used to do a second PullConfig("") + Reconcile itself, and that was a
// genuine race with durable consequences. thaw runs on the heartbeat goroutine
// and pullLoop on the main one, and NOTHING synchronised them — not the
// Supervisor, not the multiReconciler, not wgnet.Net. Reconcile is a full
// REPLACEMENT on both sides: wgnet.Reconcile(desired) removes every current peer
// absent from desired (a wipe when desired is empty), and the SSH table
// reconciler Replaces wholesale. So whichever of the two calls lands SECOND wins
// outright, and a stale result carrying fewer peers deletes the laptop's
// WireGuard peer and deauthorises it for SSH. The strand is durable rather than
// self-healing: pullLoop's cursor already hashes the good, one-peer content, so
// the server 304s forever and recovery needs the 12h lease to move or a fresh
// connect. It presents as "connect hangs; ctrl-C and retry works". The window is
// not theoretical — the two reconciles were observed landing 291–749ms apart,
// unsynchronised, in 3 of 3 staging runs.
//
// A note for anyone re-reading FIX-352: the ticket argued that thaw's pull was
// "structurally guaranteed" to read a PRE-attach snapshot. That argument is
// wrong and must not be repeated — in the observed runs thaw's pull returned
// count=2, i.e. post-attach; the attach had already happened by the time it ran.
// The justification for removing it is the missing synchronisation and the
// full-replacement semantics above, and nothing else.
//
// # Why the session sync is fired asynchronously, once
//
// beat already fires SyncSessions; thaw used to fire it a SECOND time on top,
// so every resume shipped the session snapshot twice. That duplicate is gone,
// and the one that remains is fired off the critical path — see
// syncSessionsAsync for why it belongs nowhere near it.
//
// # Instrumentation
//
// The log line carries thaw_ms (the whole hook), beat_ms (the part that gates
// attachability) and close_idle_ms — which covers the WHOLE first step (cancel
// + the bounded wait for the poll to return + the settle + the drop itself),
// not just the CloseIdleConnections call. With no poll in flight that is the
// 25ms settle; the ~250ms case is a poll that never returns and rides
// pollReturnWait out. So a slow resume is readable from one line
// instead of reconstructed from source. There is deliberately no pull_ms —
// thaw no longer pulls — and sync_ms is logged by the async sync itself, since
// it is no longer a component of this duration.
//
// It also does NOT report how many connections the drop actually closed.
// Neither http.Client.CloseIdleConnections nor http.Transport returns a count or
// exposes a pooled-conn gauge, so that number cannot be obtained without either
// fabricating it or bolting a shim onto the transport purely for a diagnostic.
// A field that is sometimes a guess is worse than no field.
//
// # Scope
//
// This is a warm-resume-only path: a cold boot re-runs the init script and
// starts a fresh agent, so the process never observes a clock step. It is safe
// to re-fire — the cancel, the pool drop and the heartbeat are all idempotent —
// which is what lets the detector treat ANY realtime clock step as a resume
// without trying to classify it (resumewatch.go).
func (s *Supervisor) thaw(ctx context.Context) {
	start := time.Now()
	s.cancelPollAndDropPool()
	beatStart := time.Now()
	s.thawBeat(ctx)
	s.Log.Info("thaw complete",
		"close_idle_ms", beatStart.Sub(start).Milliseconds(),
		"beat_ms", time.Since(beatStart).Milliseconds(),
		"thaw_ms", time.Since(start).Milliseconds())
}
