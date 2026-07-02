package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/compositor"
	"github.com/fixed-labs/oss/cli/internal/session"
	"github.com/fixed-labs/oss/cli/internal/tunnel"
)

// runSessionLoop is the reconnect loop. It wraps ONLY the attach→composite step;
// the keypair, tunnel bring-up, SSH bridge, and presenceLoop already live OUTSIDE
// it (in connect, on leaseCtx). ONE session.Client (one SSH connection over the
// existing tunnel) backs the whole loop: a `~s` switch re-selects and re-attaches
// over the SAME connection (never re-dials), while a transport failure drops the
// client, backs off gated on tunnel readiness (a probe to wg0:22), and re-dials —
// capped at a max-consecutive-failure count.
//
// sessionID is the chosen session ("" → create a new one via New); it is updated
// in place as the loop attaches/switches so a reconnect re-attaches the SAME one.
// newName is the name to give a freshly-created session (from `--session NAME`
// alongside `--new`); empty means let the server auto-name. It is only consulted
// the first time a session is created (sessionID == ""); once created, the minted
// id drives reconnects.
//
// forwardAgent enables SSH agent forwarding: each (re)dial sets
// up forwarding over the new connection, so forwarding survives reconnect.
func runSessionLoop(ctx context.Context, t *tunnel.Tunnel, bundle *client.AttachBundle, workspaceID, sessionID, newName string, forwardAgent bool) error {
	const (
		maxConsecutiveFailures = 6
		backoffBase            = 2 * time.Second
		backoffMax             = 20 * time.Second
	)
	failures := 0
	var sc *session.Client
	defer func() {
		if sc != nil {
			_ = sc.Close()
		}
	}()

	// loopCtx lets the disconnect overlay cancel the whole loop on Ctrl-C: the
	// overlay holds the terminal in raw mode while it's up, so a tty Ctrl-C no
	// longer raises SIGINT on its own — the overlay translates it into this cancel.
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	ctx = loopCtx

	// recon is the "Disconnected — reconnecting" overlay, raised on a transport
	// drop and torn down the instant the live session is about to repaint. It's
	// created lazily on the first drop and only ever touched from this goroutine.
	var recon reconnectScreen
	showRecon := func(status string) {
		if recon == nil {
			recon = showReconnectingFn(compositor.Label(workspaceID, sessionID), loopCancel)
		}
		recon.SetStatus(status)
	}
	hideRecon := func() {
		if recon != nil {
			recon.Close()
			recon = nil
		}
	}
	defer hideRecon()

	// dropClient tears down the current SSH connection so the next iteration
	// re-dials (used after a transport failure).
	dropClient := func() {
		if sc != nil {
			_ = sc.Close()
			sc = nil
		}
	}

	for {
		if ctx.Err() != nil {
			return nil // leaseCtx canceled (SIGINT / parent done) — clean stop
		}

		// (Re)dial the box if we have no live SSH connection.
		if sc == nil {
			dctx, dcancel := ctxTimeout(ctx, 30*time.Second)
			c, err := dialSessionFn(dctx, t, bundle, forwardAgent)
			dcancel()
			if err != nil {
				failures++
				if failures >= maxConsecutiveFailures {
					return fmt.Errorf("could not reach the box after %d attempts: %w", failures, err)
				}
				// Keep the overlay's attempt line current across a failed re-dial,
				// but don't raise it here: the very first dial is the initial connect,
				// not a reconnect (the drop path raises it before we ever re-dial).
				if recon != nil {
					recon.SetStatus(reconnectStatus(failures))
				}
				if !backoffAndProbeFn(ctx, t, bundle.WorkspaceWgIP, failures, backoffBase, backoffMax) {
					return nil
				}
				continue
			}
			sc = c
		}

		outcome, attached, exitCode, exitKnown, transportErr := attachOnceFn(ctx, sc, workspaceID, &sessionID, newName, hideRecon)

		// A SUCCESSFUL (re)attach means the box was reachable and the compositor
		// ran for this session — so the consecutive-failure streak is broken.
		// Resetting here (before classifying the outcome) makes maxConsecutiveFailures
		// a cap on CONSECUTIVE drops with no intervening success: a box that
		// reconnects cleanly N times is never abandoned, while a genuinely
		// unreachable box (no successful attach) still gives up at the cap.
		if attached {
			failures = 0
		}

		switch outcome {
		case compositor.OutcomeClientGone:
			return nil // local terminal closed / stdin EOF

		case compositor.OutcomeDetach:
			fmt.Println("detached (session left running on the box)")
			return nil

		case compositor.OutcomeSwitch:
			// Re-select a session over the SAME connection (no re-dial).
			next, err := switchSession(ctx, sc, workspaceID)
			if err != nil {
				slog.Warn("session switch failed; reconnecting", "err", err)
				// A failed switch likely means the connection dropped — re-dial.
				dropClient()
				continue
			}
			if next != "" {
				sessionID = next
			}
			failures = 0
			continue

		case compositor.OutcomeChildExit:
			act := classifyChildExit(exitCode, exitKnown, transportErr)
			switch act.kind {
			case childExitReconnect:
				// A transport failure / mid-session drop, NOT a real session end.
				// Drop the client, back off gated on tunnel readiness, and re-dial —
				// capped at maxConsecutiveFailures.
				dropClient()
				failures++
				if failures >= maxConsecutiveFailures {
					return fmt.Errorf("session lost after %d reconnect attempts: %w", failures, transportErr)
				}
				// Raise the disconnect overlay for the whole gap (backoff + re-dial +
				// re-attach). attachOnce's onAttached (hideRecon) tears it down the
				// instant the live session is about to repaint.
				showRecon(reconnectStatus(failures))
				if !backoffAndProbeFn(ctx, t, bundle.WorkspaceWgIP, failures, backoffBase, backoffMax) {
					return nil // ctx canceled during backoff
				}
				// On-disk diagnostic trail (the user-facing indication is the overlay
				// above; slog goes to the rotating logfile, not the terminal).
				slog.Info("reconnecting", "attempt", failures+1)
				continue
			case childExitStopWithCode:
				// A genuine session end: the agent wrote "session terminated, exit
				// code N" then exited the channel with N.
				return fmt.Errorf("session ended with exit code %d", act.exitCode)
			default: // childExitCleanStop
				return nil
			}
		}
	}
}

// childExitKind is the loop's terminal action for an OutcomeChildExit.
type childExitKind int

const (
	childExitCleanStop    childExitKind = iota // exit 0, real end → clean stop
	childExitStopWithCode                      // exit N≠0, real end → stop, propagate N
	childExitReconnect                         // no exit status → transport drop → re-dial
)

type childExitAction struct {
	kind     childExitKind
	exitCode int
}

// classifyChildExit maps the (exitCode, exitKnown, transportErr) triple a
// child-exit produces to the loop's terminal action — the heart of the
// child-exit classification fix. It is pure so the three terminal cases are
// unit-testable without a PTY:
//   - transportErr != nil OR no exit status (exitKnown==false) → reconnect (a
//     mid-session transport drop is NOT a session end).
//   - a known exit status of 0 → clean stop.
//   - a known non-zero exit status → stop and propagate the code.
func classifyChildExit(exitCode int, exitKnown bool, transportErr error) childExitAction {
	if transportErr != nil || !exitKnown {
		return childExitAction{kind: childExitReconnect}
	}
	if exitCode != 0 {
		return childExitAction{kind: childExitStopWithCode, exitCode: exitCode}
	}
	return childExitAction{kind: childExitCleanStop}
}

// runCompositor runs the chrome compositor for an attached session. Indirected
// so a test can drive attachOnce's terminal classification deterministically
// (the real compositor needs a TTY and reads os.Stdin).
var runCompositor = compositor.Run

// The reconnect loop's three network/terminal-touching steps are indirected
// through package-level fn vars so a test can drive the loop's control flow —
// in particular the consecutive-failure-counter reset (the cap counts
// CONSECUTIVE drops, not cumulative) — without a real tunnel, SSH dial, or PTY.
// Production wires the real implementations; tests swap them.
var (
	dialSessionFn     = dialSession
	attachOnceFn      = attachOnce
	backoffAndProbeFn = backoffAndProbe
)

// reconnectScreen is the disconnect overlay surface runSessionLoop drives — a
// *compositor.Reconnecting in production, a fake in tests.
type reconnectScreen interface {
	SetStatus(string)
	Close()
}

// showReconnectingFn raises the disconnect overlay. Indirected so a loop test can
// observe show/hide without taking over a real terminal.
var showReconnectingFn = func(label string, onAbort func()) reconnectScreen {
	return compositor.ShowReconnecting(label, onAbort)
}

// reconnectStatus is the overlay's status line for the Nth consecutive reconnect
// attempt (the first attempt omits the count to keep the common case quiet).
func reconnectStatus(attempt int) string {
	if attempt <= 1 {
		return "Will reconnect automatically…"
	}
	return fmt.Sprintf("Will reconnect automatically… (attempt %d)", attempt)
}

// attachOnce attaches the session (or creates one when *sessionID is "") over the
// live client sc and runs the compositor. newName names a freshly-created session
// (only used when *sessionID is ""); empty lets the server auto-name.
//
// attached reports whether the attach actually SUCCEEDED — set true once the
// attach is live (the compositor is about to run / has run). The caller resets
// its consecutive-failure counter on a true value, so maxConsecutiveFailures
// caps consecutive drops with no intervening success (the reconnect counter is
// consecutive, not cumulative). A pre-compositor attach failure returns
// attached=false with transportErr set.
//
// On a child-exit outcome it classifies the terminal cause:
//   - the attach itself failed → transportErr set (re-dial).
//   - the agent sent an exit status → exitKnown=true with that code (a real
//     session end; the loop stops, propagating a non-zero code).
//   - no exit status (the channel closed without one) → a MID-SESSION TRANSPORT
//     DROP, NOT a real end → transportErr set so the caller drops the client and
//     re-dials. Returning nil here would route a drop to a silent clean exit and
//     defeat the reconnect loop.
func attachOnce(ctx context.Context, sc *session.Client, workspaceID string, sessionID *string, newName string, onAttached func()) (outcome compositor.Outcome, attached bool, exitCode int, exitKnown bool, transportErr error) {
	cols, rows, _ := compositor.InitialSize()
	icols, irows := compositor.PostChromeSize(cols, rows)

	actx, acancel := ctxTimeout(ctx, 30*time.Second)
	var att *session.Attached
	var err error
	if *sessionID == "" {
		att, err = sc.New(actx, newName, icols, irows)
	} else {
		att, err = sc.Attach(actx, *sessionID, icols, irows)
	}
	acancel()
	if err != nil {
		return compositor.OutcomeChildExit, false, 0, false, fmt.Errorf("attach: %w", err)
	}
	// Record the session we landed on so a reconnect re-attaches the SAME one
	// (a `New` mints an id we didn't know up front). The attach is now live, so
	// report success: the consecutive-failure streak is broken.
	*sessionID = att.ID
	attached = true

	// The attach is live — tear down any disconnect overlay the loop raised so it
	// doesn't sit over (or fight raw mode with) the compositor that's about to take
	// the terminal. Done here, just before runCompositor, to keep the gap covered
	// right up to the repaint.
	if onAttached != nil {
		onAttached()
	}

	out := runCompositor(att, compositor.Label(workspaceID, att.ID))

	if out == compositor.OutcomeChildExit {
		// Distinguish a real session end (agent sent an exit status) from a
		// transport drop (no status). A drop must surface as a NON-NIL transportErr
		// so runSessionLoop engages backoff/probe/re-dial; returning nil here would
		// route a drop to a silent clean exit and defeat the whole reconnect loop
		// (the keepalive that closes a blackholed conn would be pointless).
		code, ok := att.Wait()
		if !ok {
			// No exit status = the channel/connection dropped mid-session, not a
			// real end. Re-dial.
			return out, attached, 0, false, errors.New("transport dropped (no exit status)")
		}
		// A genuine session end with a known exit code.
		return out, attached, code, true, nil
	}
	return out, attached, 0, false, nil
}

// switchSession runs the picker (~s) over the live client to choose another
// session without re-dialling. Returns the new session id, "" to abort (re-attach
// the current), or an error (likely a dropped connection).
func switchSession(ctx context.Context, sc *session.Client, workspaceID string) (string, error) {
	lctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	list, err := sc.List(lctx)
	if err != nil {
		return "", err
	}
	_ = storeGenEpoch(workspaceID, list.GenEpoch)
	if len(list.Sessions) == 0 {
		return "", nil // nothing to switch to
	}
	return pickSession(list.Sessions)
}

// backoffAndProbe waits an exponential backoff bounded by ctx, then probes the
// tunnel by dialling wgIP:22 — so a re-dial isn't attempted until the path is
// back. Returns false if ctx was canceled during the wait.
func backoffAndProbe(ctx context.Context, t *tunnel.Tunnel, wgIP string, attempt int, base, max time.Duration) bool {
	d := base << (attempt - 1)
	if d > max {
		d = max
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
	}
	// Gate on tunnel readiness: a quick TCP probe to wg0:22 over the netstack.
	deadline := time.Now().Add(max)
	for {
		if ctx.Err() != nil {
			return false
		}
		pctx, pcancel := ctxTimeout(ctx, 5*time.Second)
		conn, err := t.DialContext(pctx, "tcp", net.JoinHostPort(wgIP, "22"))
		pcancel()
		if err == nil {
			_ = conn.Close()
			return true
		}
		if time.Now().After(deadline) {
			return true // give up probing; let the re-dial surface the error
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}
