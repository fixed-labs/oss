package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/compositor"
	"github.com/fixed-labs/oss/cli/internal/session"
	"github.com/fixed-labs/oss/cli/internal/tunnel"
)

// connectOpts carries the connect verb's flags. `--new` forces a fresh session
// (skips the picker / default-session attach); `--session` names an explicit
// session to (re)attach.
type connectOpts struct {
	newSession  bool
	sessionName string // explicit name to attach/create (empty → default-session selection)
}

// asAPIError is errors.As specialized for *client.APIError.
func asAPIError(err error, target **client.APIError) bool {
	return errors.As(err, target)
}

// connect is the heart of the CLI: resume the box if stopped, wait for it to be
// running, open an attachment, bring up the userspace tunnel, select a session
// (default-session selection: create-or-attach the single default session), and
// attach to it under the chrome compositor. Background
// loops keep the connection alive (the sessionKeeper refreshes the 12h
// attachment lease and follows a relay drain/failover without a teardown;
// watchNetworkChanges rebinds the wireguard socket on laptop sleep/wake/roam;
// presenceLoop holds the cluster's idle-suspend off while the operator is
// attached). All die with leaseCtx when connect returns.
//
// `id` is the WORKSPACE id; the chosen SESSION id is a separate local value
// (sessionID) threaded through the reconnect loop so a reconnect re-attaches the
// SAME session.
func connect(ctx context.Context, c *client.Client, id string, opts connectOpts) error {
	ws, err := waitRunning(ctx, c, id)
	if err != nil {
		return err
	}

	// Fresh per-session laptop keypair; the public half is the authorized peer.
	priv, pub, err := tunnel.GenerateKeypair()
	if err != nil {
		return err
	}

	actx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	bundle, err := c.Attach(actx, id, pub, "")
	if err != nil {
		var ae *client.APIError
		if asAPIError(err, &ae) && ae.Status == 503 {
			return fmt.Errorf("relay port pool exhausted — try again shortly or ask an operator to add relay capacity")
		}
		return fmt.Errorf("attach: %w", err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = c.Detach(dctx, id, pub)
	}()

	// Keep the attachment lease alive for the whole connection: the server leases
	// each (laptop, workspace) pairing for attachment-lease-ms (12h) and the
	// contract is a refresh at lease/2. Re-Attach with the same pubkey is the
	// refresh.
	leaseCtx, leaseCancel := context.WithCancel(ctx)
	defer leaseCancel()

	params := tunnel.Params{
		LaptopPrivateKey:   priv,
		WorkspacePublicKey: bundle.WorkspaceWgPubkey,
		RelayEndpoint:      bundle.RelayPublicEndpoint,
		RelayPort:          bundle.RelayPort,
		WorkspaceWgIP:      bundle.WorkspaceWgIP,
	}
	t, err := tunnel.Up(ctx, params, bundle.LaptopWgIP)
	if err != nil {
		return fmt.Errorf("tunnel: %w", err)
	}
	defer t.Close()

	// Background maintenance for the life of the connection (all die with
	// leaseCtx). The keeper is the sole owner of post-connect Attach + peer-
	// endpoint mutation, so the lease refresh and the relay-reassign watcher can't
	// race two endpoints onto the device.
	keeper := &sessionKeeper{c: c, t: t, id: id, pub: pub}
	go keeper.leaseLoop(leaseCtx)       // attachment-lease refresh
	go keeper.watchRelay(leaseCtx)      // follow a drain/failover to a new relay, no drop
	go watchNetworkChanges(leaseCtx, t) // rebind the wg socket on sleep/wake/roam

	// Host the secret-broker handler for the life of the session: the box's
	// `devbox run` shim reaches it over the tunnel at the laptop's overlay IP.
	// The handler resolves named
	// secrets against the same user config + repo the connect-time reconcile uses
	// and injects credentials into a `devbox run` child — the value never touches
	// the box's disk or the agent's context. Best-effort: if the listener can't
	// come up we warn and continue (the shell still works; `devbox run` just gets
	// an unreachable-handler error).
	stopBroker := startBrokerHandler(leaseCtx, t, ws.Repo)
	defer stopBroker()
	// Hold the box's interactive-liveness for the whole connection: the
	// laptop-side ping keeps the cluster from idle-suspending us while attached.
	go presenceLoop(leaseCtx, c, id)

	// Bridge the in-process netstack to a localhost listener so the external
	// `ssh` binary can run the pre-shell secrets reconcile over the tunnel. (The
	// interactive session itself rides a Go SSH client dialed directly over the
	// netstack — the devbox-session subsystem — not this bridge.)
	localAddr, err := t.BridgeSSH(ctx, bundle.WorkspaceWgIP, 22)
	if err != nil {
		return fmt.Errorf("ssh bridge: %w", err)
	}
	host, port, err := splitHostPort(localAddr)
	if err != nil {
		return err
	}
	khFile, cleanup, err := writeKnownHosts(host, port, bundle.SSHHostPubkey)
	if err != nil {
		return err
	}
	defer cleanup()

	// Sync declared secrets onto the box over the same tunnel, before the shell
	// opens. Best-effort: a hiccup warns but never blocks the shell. forwardAgent
	// is true when a std:ssh key resolved → request SSH agent forwarding on the
	// session connection(s) below.
	forwardAgent := reconcileSecrets(ctx, host, port, khFile, ws.Repo)

	// A Ctrl-C during the secrets sync cancels ctx — abort cleanly rather than
	// print "Opening shell…" and then fail to dial.
	if ctx.Err() != nil {
		return nil
	}

	// Select the session to attach (default-session selection) and report any detached loss
	// (gen-epoch) before attaching. This opens a short-lived session.Client to
	// `list`; the attach loop opens its own.
	sessionID, err := selectSession(ctx, t, bundle, id, opts, forwardAgent)
	if err != nil {
		return err
	}

	fmt.Printf("Connected to %s (%s). Opening shell…\n", id, ws.Status)
	return runSessionLoop(leaseCtx, t, bundle, id, sessionID, opts.sessionName, forwardAgent)
}

// dialSession opens an SSH connection to the box over the tunnel for the
// devbox-session subsystem (host key pinned, NoClientAuth). forwardAgent enables
// SSH agent forwarding over this connection when a local agent is reachable.
func dialSession(ctx context.Context, t *tunnel.Tunnel, bundle *client.AttachBundle, forwardAgent bool) (*session.Client, error) {
	return session.Dial(ctx, t.DialContext, bundle.WorkspaceWgIP, 22, loginUser(), bundle.SSHHostPubkey, forwardAgent)
}

// sessionDecision is the pure outcome of applying default-session selection (and the
// explicit --session/--new flags) to a list result. It separates the decision
// from the network dial and terminal I/O so the policy is unit-testable.
type sessionDecision struct {
	// sessionID is the session to attach; "" means "create a new one" (the
	// attach loop calls New).
	sessionID string
	// needsPicker is true for the >1-session case: the caller must run the
	// interactive picker over candidates.
	needsPicker bool
	candidates  []session.SessionInfo
	// lossNotice is true when the box's gen-epoch advanced past the last we saw,
	// so a detached restart lost the prior sessions.
	lossNotice bool
}

// decideSession applies the selection policy. prevEpoch/havePrev are the
// last-recorded gen-epoch for this box; opts carries --new/--session.
func decideSession(list *session.ListResult, opts connectOpts, prevEpoch int64, havePrev bool) sessionDecision {
	d := sessionDecision{}
	if havePrev && list.GenEpoch > prevEpoch {
		d.lossNotice = true
	}
	// Explicit --session <name>: attach if it exists by name, else create it.
	if opts.sessionName != "" {
		for _, s := range list.Sessions {
			if s.Name == opts.sessionName {
				d.sessionID = s.ID
				return d
			}
		}
		return d // sessionID "" → create with this name
	}
	switch len(list.Sessions) {
	case 0:
		return d // "" → create+attach `main`
	case 1:
		d.sessionID = list.Sessions[0].ID
		return d
	default:
		d.needsPicker = true
		d.candidates = list.Sessions
		return d
	}
}

// selectSession applies default-session selection and the gen-epoch loss notice. It returns
// the session id to attach (empty means "create a new one" — the attach loop
// then calls New). When `--new` is set it skips the list and returns "" (new).
func selectSession(ctx context.Context, t *tunnel.Tunnel, bundle *client.AttachBundle, workspaceID string, opts connectOpts, forwardAgent bool) (string, error) {
	if opts.newSession {
		// "" → the attach loop calls New; opts.sessionName (threaded separately into
		// runSessionLoop) becomes the new session's name, or auto-named if empty.
		return "", nil
	}
	lctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	sc, err := dialSession(lctx, t, bundle, forwardAgent)
	if err != nil {
		// The subsystem may be unavailable (older agent); fall back to the bare
		// shell path, which gives the default `main` session server-side.
		fmt.Fprintf(os.Stderr, "rift: session list unavailable (%v); attaching default session\n", err)
		return "", nil
	}
	defer sc.Close()

	list, err := sc.List(lctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rift: session list failed (%v); attaching default session\n", err)
		return "", nil
	}

	prev, havePrev := lastGenEpoch(workspaceID)
	d := decideSession(list, opts, prev, havePrev)
	// Loss notice: announce a detached restart before
	// applying default-session selection, then record the new epoch.
	if d.lossNotice {
		fmt.Println("the box restarted; previous sessions did not survive")
	}
	_ = storeGenEpoch(workspaceID, list.GenEpoch)

	if opts.sessionName != "" && d.sessionID == "" {
		fmt.Fprintf(os.Stderr, "rift: no session named %q; creating it\n", opts.sessionName)
	}
	if d.needsPicker {
		return pickSession(d.candidates)
	}
	return d.sessionID, nil
}

// pickSession renders the interactive picker for the >1 case and returns the
// chosen session id ("" → create new). Aborting the picker cancels the connect.
// RunPickerTTY handles the non-TTY case (defaults to the first session).
func pickSession(sessions []session.SessionInfo) (string, error) {
	items := make([]compositor.PickItem, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, compositor.PickItem{
			ID:    s.ID,
			Label: compositor.FormatPickLabel(s.Name, "", "", s.AttachedCount),
		})
	}
	res, err := compositor.RunPickerTTY(items, "Select a session to attach:")
	if err != nil {
		return "", err
	}
	if res.Aborted {
		return "", fmt.Errorf("no session selected")
	}
	if res.New {
		return "", nil
	}
	return res.Selected, nil
}

const (
	// attachRefreshInterval is half the server's attachment-lease-ms (12h).
	attachRefreshInterval = 6 * time.Hour
	// attachRefreshRetry is the backoff after a failed refresh — well inside the
	// remaining ~6h of lease, so transient API blips can't lapse it.
	attachRefreshRetry = time.Minute
	// presenceInterval is how often the running connect process pings presence.
	// Well under suspend-window-ms (30 min) so several missed pings (a network
	// blip, a short laptop sleep) don't lapse the interactive-liveness floor.
	presenceInterval = 5 * time.Minute
)

// presenceLoop keeps the box's interactive-liveness fresh for the whole
// connection. This laptop-side ping tells the cluster "an operator is still
// attached" and prevents idle-suspend. Best-effort: a failed ping just retries
// next tick — the 30-min suspend window tolerates several misses.
func presenceLoop(ctx context.Context, c *client.Client, id string) {
	t := time.NewTicker(presenceInterval)
	defer t.Stop()
	for {
		pctx, cancel := ctxTimeout(ctx, 30*time.Second)
		err := c.Presence(pctx, id)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("presence ping failed; will retry", "workspace", id, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// waitRunning resumes a stopped box and polls (via the live long-poll) until it
// reaches running (or a terminal failure).
func waitRunning(ctx context.Context, c *client.Client, id string) (*client.Workspace, error) {
	gctx, cancel := ctxTimeout(ctx, 30*time.Second)
	ws, cursor, err := c.Get(gctx, id, "")
	cancel()
	if err != nil {
		return nil, err
	}
	if ws.Status == "stopped" {
		rctx, rcancel := ctxTimeout(ctx, 30*time.Second)
		err := c.Resume(rctx, id)
		rcancel()
		if err != nil {
			return nil, fmt.Errorf("resume: %w", err)
		}
		fmt.Printf("%s is stopped — resuming…\n", id)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for ws.Status != "running" {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("workspace %s did not reach running (last: %s)", id, ws.Status)
		}
		switch ws.Status {
		case "failed", "done", "destroying":
			return nil, fmt.Errorf("workspace %s is %s: %s", id, ws.Status, ws.ErrorMessage)
		}
		// Long-poll the live channel for the next state change.
		pctx, pcancel := ctxTimeout(ctx, 40*time.Second)
		next, ncursor, err := c.Get(pctx, id, cursor)
		pcancel()
		if err != nil {
			return nil, err
		}
		if next.WorkspaceID != "" {
			ws = next
		}
		cursor = ncursor
	}
	return ws, nil
}

func loginUser() string {
	if u := os.Getenv("RIFT_LOGIN_USER"); u != "" {
		return u
	}
	return "dev"
}
