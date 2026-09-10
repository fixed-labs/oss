// devboxes-agent — the control-plane liaison inside each workspace.
// Boot order:
//
//  1. load the RIFT_* boot config injected by the provisioner
//  2. ensure first-boot identity (wg keypair + SSH host key, persisted on
//     the overlay so the public halves are byte-stable for the VM's life)
//  3. bring up wg0 at the cluster-owned deterministic ULA address
//  4. start the WG-identity SSH server bound to wg0's address
//  5. run the supervisor: the config-pull reconcile loop (feeding BOTH wg0's
//     peer set and the SSH server's source-ip → identity table — one config,
//     two uses) and the heartbeat. The heartbeat carries the public identity
//     and IS the readiness signal (no separate workspace-ready report) — the
//     cluster flips the row to running off it, so a dropped beat self-heals.
//
// Special invocation: `devboxes-agent sftp-subsystem` serves sftp on stdio —
// the SSH server re-execs the binary this way under the login user's
// credential, so sftp file ownership comes out right with no extra image
// deps.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
	"github.com/fixed-labs/oss/agent/internal/config"
	"github.com/fixed-labs/oss/agent/internal/identity"
	"github.com/fixed-labs/oss/agent/internal/nsexec"
	"github.com/fixed-labs/oss/agent/internal/sessions"
	"github.com/fixed-labs/oss/agent/internal/sshserver"
	"github.com/fixed-labs/oss/agent/internal/supervisor"
	"github.com/fixed-labs/oss/agent/internal/wgnet"
)

// loginUser is the single login user every authorized peer lands as (devboxes-
// base loginUser, default "dev"). The session Manager spawns its shells as this
// user.
func loginUser() string {
	if u := os.Getenv("RIFT_LOGIN_USER"); u != "" {
		return u
	}
	return "dev"
}

// stdioRWC adapts stdin/stdout into the io.ReadWriteCloser sftp serves on.
type stdioRWC struct{}

func (stdioRWC) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdioRWC) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdioRWC) Close() error                { return nil }

func runSFTPSubsystem() {
	if err := sshserver.ServeSFTP(stdioRWC{}); err != nil {
		os.Exit(1)
	}
}

// tableReconciler maps the pulled peer set into the SSH server's
// source-wg-ip → identity table (the second use of the one config).
type tableReconciler struct{ table *sshserver.Table }

func (t tableReconciler) Reconcile(peers []api.Peer) error {
	m := make(map[string]sshserver.Peer, len(peers))
	for _, p := range peers {
		m[p.LaptopWgIP] = sshserver.Peer{DeveloperID: p.DeveloperID, LoginUser: p.LoginUser}
	}
	t.table.Replace(m)
	return nil
}

// multiReconciler fans one pulled config out to every consumer; the first
// error wins (the pull loop logs + repulls — idempotent full replacement).
type multiReconciler []supervisor.Reconciler

func (m multiReconciler) Reconcile(peers []api.Peer) error {
	for _, r := range m {
		if err := r.Reconcile(peers); err != nil {
			return err
		}
	}
	return nil
}

// serialReconciler makes reconciliation a serialized, monotonic operation: at
// most one reconcile runs at a time, and a reconcile carrying an older desired
// set can never land on top of a newer one. It wraps the WHOLE fan-out, so the
// SSH table and wg0 move together as one indivisible step and keep their
// table-first ordering (see the wiring in main).
//
// Two distinct hazards, both closed here:
//
//   - Overlap. wgnet.Reconcile is a read-then-mutate sequence of `wg`
//     shell-outs (read the live peer set, then add/remove/re-add against what
//     it read); two of those interleaved race on state that lives in the
//     kernel, where a mutex cannot reach. sshserver.Table.Replace is
//     individually safe but is only coherent with wg0 if the two move in step.
//   - Stale overwrite. A reconcile with an EMPTY peer set is a full wipe:
//     wgnet removes every peer, tableReconciler deauthorizes every laptop. The
//     resulting strand is durable rather than self-healing, because the pull
//     loop's cursor already hashes the peer set it last saw — the server then
//     answers 304 forever and nothing re-drives the reconcile. It presents as
//     "connect hangs; ctrl-C and retry works".
//
// This is belt-and-braces, honestly labelled: since thaw stopped doing its own
// pull, the pull loop is the sole caller of Reconcile and calls it from one
// goroutine, so there is no live bug for this to fix today. The point is that
// "no two reconciles overlap, and the newest desired set wins" should be a
// property the code ENFORCES, not one the current call graph happens to have —
// a second caller added later then inherits the guarantee instead of
// rediscovering the hazard.
type serialReconciler struct {
	inner supervisor.Reconciler

	// issued hands out generation numbers at call ENTRY, outside the mutex. That
	// placement is the whole mechanism: it records the order in which callers
	// ARRIVED — the freshness order, since a caller reconciles what it has just
	// pulled — which is precisely the ordering the mutex destroys, because
	// waiters acquire it in no guaranteed relation to when they queued.
	issued atomic.Uint64

	mu sync.Mutex
	// seen is the newest generation ADMITTED. It is what fences stale callers,
	// and it advances at admission rather than on success — see Reconcile.
	// Guarded by mu.
	seen uint64
}

func (s *serialReconciler) Reconcile(peers []api.Peer) error {
	return s.reconcileGen(s.issued.Add(1), peers)
}

// reconcileGen applies `peers` unless a newer generation already landed. Split
// out from Reconcile purely as a test seam: it lets a test state an
// out-of-order arrival outright, rather than trying to provoke one out of the
// scheduler and hoping it shows up.
func (s *serialReconciler) reconcileGen(gen uint64, peers []api.Peer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen <= s.seen {
		// A newer desired set landed while this call waited for the lock, so
		// this one is stale and applying it would move wg0 and the SSH table
		// BACKWARDS. Reporting success is right: the caller's actual intent —
		// "wg0 and the SSH table reflect the freshest config" — holds, and an
		// error here would have the pull loop log a failure and back off over
		// what is, in effect, a no-op.
		return nil
	}
	// Advance the fence at ADMISSION, not on success. If it only advanced when
	// the inner reconcile succeeded, a failed gen 5 would leave the fence at 3 —
	// and gen 4, older and possibly carrying the empty wipe, would then pass the
	// guard above and be applied on top. That is the exact hazard this type
	// exists to stop, and it is reachable precisely when `wg` shell-outs are
	// failing. A caller whose reconcile failed still gets the error, and under
	// guard 2 (supervisor.pullLoop) holds its cursor and retries.
	s.seen = gen
	return s.inner.Reconcile(peers)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sftp-subsystem" {
		runSFTPSubsystem()
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := config.FromEnv()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	// ns is the bootstrap-mode router/monitor (rift-image-model §3.6/§3.7).
	// With RIFT_BOOTSTRAP unset (legacy in-system mode) every one of its hooks
	// is a strict no-op and behavior is byte-for-byte today's.
	ns := nsexec.FromEnv(log)

	client := api.New(cfg.APIBaseURL, cfg.WorkspaceID, cfg.Token)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if ns.Bootstrap {
		// The client_system heartbeat field (the promote gate's input) is
		// emitted iff bootstrap mode; legacy agents leave the key entirely
		// absent (the server's compat arm).
		client.ClientSystem = ns.Status
		// The recovery driver waits for the client system's health and, on
		// timeout, signals the supervisor (SIGUSR1 → the agent's parent — Fly's
		// init is VM PID 1, never the target) for a recovery boot.
		go ns.RunRecoveryDriver(ctx)
		// Self-copy the agent binary to /persist/rift/agent so the sftp re-exec
		// resolves inside the client system (rung 1's volume-store bind hides
		// the image store). Non-fatal: sessions and exec still work without it.
		if perr := ns.EnsurePersistAgent(); perr != nil {
			log.Warn("bootstrap: agent self-copy for in-target sftp failed; sftp into the client system will not work",
				"path", ns.PersistAgentPath, "err", perr)
		}
		log.Info("bootstrap mode: client-system monitor active",
			"health_timeout", ns.HealthTimeout)
	}

	id, err := identity.Ensure(cfg.StateDir, identity.ExecWgKeygen{})
	if err != nil {
		log.Error("identity", "err", err)
		_ = client.ReportFailed(ctx, "identity: "+err.Error())
		os.Exit(1)
	}

	net := wgnet.New(wgnet.ExecRunner, id.WgPrivateKeyPath)
	table := sshserver.NewTable()
	// Order matters: authorize FIRST (SSH table), then enable the path (wg
	// peer). wgnet.Reconcile shells out (`wg set`/`ip route`/`wg show`) and is
	// slow, so if it ran first the laptop's wg peer could be live — and its SSH
	// connection arrive — before the auth table is populated, and the
	// ConnCallback gate would refuse it. Table-first closes that window (and on
	// removal, deauthorizing before tearing down the peer is the safe order too).
	//
	// Wrapped so the pair is applied serially and monotonically: the ordering
	// above is only meaningful if one reconcile finishes before the next starts,
	// and if an older desired set can never land after a newer one (see
	// serialReconciler).
	reconcilers := &serialReconciler{inner: multiReconciler{tableReconciler{table}, net}}

	// Boot reconcile (the monotonic gen-epoch mechanism): on a fresh process
	// bump the gen-epoch BEFORE reporting (a crash mid-write still advances it,
	// keeping the gate
	// monotonic). The bumped value stamps every session this generation creates;
	// the tombstone (posted below, once the api client/session manager exist)
	// removes prior generations. An agent restart and a cold boot are identical
	// here — both lose the in-memory map, so a fresh process always reconciles.
	genEpoch, gerr := sessions.ReadAndBumpGenEpoch(cfg.StateDir)
	if gerr != nil {
		// Non-fatal: a state-dir write failure must not strand the box. Fall back
		// to a wall-clock-derived epoch so the value still advances across boots.
		log.Warn("gen-epoch bump failed; falling back to wall clock", "err", gerr)
		genEpoch = time.Now().UnixMilli()
	}

	login := loginUser()
	shell, home := sshserver.LoginShellAndHome(login)
	cred, credErr := sshserver.ResolveCredential(login)
	if credErr != nil {
		// The login user may not exist on an overlay-less / minimal boot; log and
		// continue with no setuid (shells run as the agent's own uid).
		log.Warn("session login credential", "login", login, "err", credErr)
		cred = nil
	}
	mgr := sessions.NewManager(sessions.Config{
		Shell:    shell,
		Home:     home,
		Login:    login,
		Cred:     cred,
		NS:       ns,
		API:      client,
		GenEpoch: genEpoch,
		Log:      log,
	})
	// Tombstone prior generations now that the manager (carrying genEpoch) and
	// api client exist. main is created lazily on next connect, stamped genEpoch,
	// so the strict-less-than tombstone spares the current generation.
	go func() {
		bctx, bcancel := context.WithTimeout(ctx, 30*time.Second)
		defer bcancel()
		_ = mgr.BootReconcile(bctx)
	}()

	var sshSrv *sshserver.Server
	if cfg.WgIP != "" {
		if err := net.Up(cfg.WgIP); err != nil {
			log.Error("wg0 up", "err", err)
			_ = client.ReportFailed(ctx, "wg0: "+err.Error())
			os.Exit(1)
		}

		hostKeyPEM, rerr := os.ReadFile(id.SSHHostKeyPath)
		if rerr != nil {
			log.Error("host key", "err", rerr)
			_ = client.ReportFailed(ctx, "host key: "+rerr.Error())
			os.Exit(1)
		}
		self, _ := os.Executable()
		sshSrv = &sshserver.Server{
			// wg0-bound: only overlay peers can even reach the listener; the
			// table gate then authorizes per-source.
			Addr:       "[" + cfg.WgIP + "]:22",
			HostKeyPEM: hostKeyPEM,
			Table:      table,
			SFTPExec:   self,
			Sessions:   mgr,
			NS:         ns,
			Log:        log,
		}
		if serr := sshSrv.Start(); serr != nil {
			log.Error("ssh server", "err", serr)
			_ = client.ReportFailed(ctx, "ssh: "+serr.Error())
			os.Exit(1)
		}
		defer sshSrv.Close()
	} else {
		log.Warn("RIFT_WG_IP empty — overlay not configured; attach unavailable")
	}

	log.Info("devboxes-agent starting",
		"workspace_id", cfg.WorkspaceID,
		"wg_ip", cfg.WgIP,
		"relay_endpoint", cfg.RelayEndpoint,
		"wg_pubkey", id.WgPubkey,
		"image_commit", cfg.ImageCommit)

	s := &supervisor.Supervisor{API: client, Reconcile: reconcilers, Log: log}
	if sshSrv != nil {
		// Open SSH connections count as raw activity — the heartbeat reports them
		// as ssh_sessions so idle-suspend honors a plain ssh/sftp/forward client.
		s.SSHSessions = sshSrv.ActiveSessions
	}
	// Session liveness (a different axis from raw connections): attached clients,
	// or a held PTY within the keep-warm window, fold into interactive_live; the
	// snapshot piggybacks the heartbeat cadence.
	s.AttachedClients = mgr.AttachedClients
	s.HeldLivePTYs = mgr.HeldLivePTYs
	s.LastDetachAt = mgr.LastDetachAt
	s.SyncSessions = mgr.SyncNow
	s.Identity = api.Identity{
		SSHHost:        cfg.WgIP,
		WgPubkey:       id.WgPubkey,
		SSHHostPubkey:  id.SSHHostPubkey,
		ResolvedCommit: cfg.ImageCommit,
	}
	s.Run(ctx)
}
