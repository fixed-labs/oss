// Package nsexec routes session/exec/sftp spawns into the client system and
// tracks the client system's health (rift-image-model §3.6/§3.7).
//
// Two modes, selected once at startup from RIFT_BOOTSTRAP:
//
//   - Legacy (RIFT_BOOTSTRAP unset/empty): the agent runs INSIDE the client's
//     own system, as it always has. Route returns the caller's spawn untouched,
//     Status returns "" (the client_system heartbeat key stays entirely absent
//     — absent = pre-bootstrap agent, which the promote gate treats as the
//     compat arm), and the recovery driver never runs. Behavior is
//     byte-for-byte today's.
//
//   - Bootstrap (RIFT_BOOTSTRAP=1, set by the bootstrap image's init): the
//     agent runs OUTSIDE the client system (INV-5 — same network namespace,
//     different pid+mount namespace). Every interactive session, exec and sftp
//     must then enter the client system, or it would land in the bootstrap's
//     mount namespace where none of the client's packages/env/services exist
//     (§3.6):
//
//     nsenter -t <sd-pid> -p -m -- /run/current-system/sw/bin/runuser -l <login> [-c "<cmd>"]
//
//     `-p -m` only — deliberately NOT -S/-G: dropping to the uid via nsenter
//     loses the wheel supplementary group (§3.6, probe-verified); `runuser -l`
//     inside the target does the login (groups, HOME, PATH, cwd) correctly.
//     runuser is addressed by its absolute in-target path because after -m the
//     visible filesystem is the client system's; nsenter itself resolves from
//     the AGENT's PATH (the bootstrap init provides util-linux). The nsenter
//     process runs as root — no SysProcAttr.Credential — because nsenter needs
//     root; runuser drops privileges inside the target.
//
// The pid file is the handoff (§3.6): the supervisor writes the client
// system's PID-1 host pid to /run/rift/sd-pid; the agent never re-derives it
// (no pgrep). When the file is absent or stale (process gone), spawns fall
// back to today's in-namespace path with an explicit banner, rather than
// pretending to be a normal box. On a recovery boot the sd-pid points at the
// second-stage bootstrap system, so routed spawns still nsenter — but carry
// the recovery banner, because that target is not the user's environment
// either.
package nsexec

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// File/path contract with the bootstrap init and supervisor (§3.6/§3.7).
const (
	// DefaultSDPidFile holds the running child system's PID-1 host pid,
	// written by the supervisor at every child start — whichever rung, so
	// presence does NOT mean a client system was attempted (that is
	// DefaultBootRungFile's job). Absent/stale ⇒ nothing to nsenter into.
	DefaultSDPidFile = "/run/rift/sd-pid"
	// DefaultRecoveryBootFile exists iff this boot is a recovery boot (the
	// supervisor killed a failed client system and booted BOOTSTRAP_SYSTEM).
	DefaultRecoveryBootFile = "/run/rift/recovery-boot"
	// DefaultBootRungFile carries the boot-ladder rung the supervisor chose at
	// every child start — the literal string "profile" or "bootstrap". The
	// recovery driver arms its timeout action ONLY on "profile": a
	// bootstrap-rung boot has no client system to recover, and signalling
	// there would tear down a healthy bootstrap and recovery-boot the
	// identical image.
	DefaultBootRungFile = "/run/rift/boot-rung"
	// DefaultRealizationStatusFile is optionally written by the realizer on
	// failure; a `reason=…` line carries the failure reason (logged, nothing
	// else — the report records don't exist server-side yet).
	DefaultRealizationStatusFile = "/run/rift/realization-status"
	// DefaultProfilePath is the volume profile whose symlink chain names the
	// realized client system.
	DefaultProfilePath = "/persist/nix/var/nix/profiles/system"
	// DefaultPersistRoot prefixes a logical /nix/store/... target to reach its
	// physical location on the volume.
	DefaultPersistRoot = "/persist"
	// DefaultRunuserPath is runuser's absolute path INSIDE the client system
	// (after nsenter -m the visible filesystem is the client's).
	DefaultRunuserPath = "/run/current-system/sw/bin/runuser"
	// DefaultSystemctlPath is systemctl's absolute path INSIDE the client
	// system, for the health probe.
	DefaultSystemctlPath = "/run/current-system/sw/bin/systemctl"
	// DefaultPersistAgentPath is where the agent self-copies its own binary in
	// bootstrap mode, for the sftp re-exec ONLY. Under boot rung 1 the volume
	// store is bound over the image store, so the agent's own image-store path
	// does not resolve inside the client system after nsenter -m; the /persist
	// bind is visible in both namespaces. (PR 7 revisits in-box store
	// composition.)
	DefaultPersistAgentPath = "/persist/rift/agent"
)

// DefaultHealthTimeout is how long the recovery driver waits for the client
// system to become healthy before signalling the supervisor (§3.7's T).
// Overridable via RIFT_CLIENT_SYSTEM_HEALTH_TIMEOUT (seconds) for tests.
const DefaultHealthTimeout = 180 * time.Second

// recoveryPollInterval paces the recovery driver's health polling.
const recoveryPollInterval = 5 * time.Second

// probeTimeout bounds one nsenter+systemctl health probe so a wedged D-Bus
// inside the client system can't stall a heartbeat.
const probeTimeout = 10 * time.Second

// maxProfileHops bounds the profile symlink walk (a nix profile chain is
// profile → system-N-link → store path; anything deeper than this is a loop).
const maxProfileHops = 16

// killFn is syscall.Kill, indirected for tests.
var killFn = syscall.Kill

// getppidFn is os.Getppid, indirected for tests: timeoutAction's
// orphaned-agent refusal (getppid() == 1) is unreachable under `go test`
// otherwise.
var getppidFn = os.Getppid

// NS is the process-wide spawner + client-system monitor, built once by
// FromEnv. A nil *NS behaves as legacy mode everywhere, so callers that never
// wire one (tests) keep today's behavior.
type NS struct {
	// Bootstrap is true iff RIFT_BOOTSTRAP was non-empty at startup.
	Bootstrap bool

	// File contract fields, defaulted from the constants above (fields so a
	// later test phase can point them at a scratch dir).
	SDPidFile             string
	RecoveryBootFile      string
	BootRungFile          string
	RealizationStatusFile string
	ProfilePath           string
	PersistRoot           string
	RunuserPath           string
	SystemctlPath         string
	PersistAgentPath      string

	// HealthTimeout is the recovery driver's window (DefaultHealthTimeout,
	// overridable via RIFT_CLIENT_SYSTEM_HEALTH_TIMEOUT seconds).
	HealthTimeout time.Duration
	// SystemPath is the SYSTEM_PATH machine env at startup. Non-empty ⇒ a
	// resolved profile must equal it for "healthy" (§3.7 — the operator escape
	// hatch retargets the realizer, and health must agree with it).
	SystemPath string

	// RunCommand, when non-nil, replaces the RAW execution of the health
	// probe's command: it receives the command name + args and returns the
	// command's stdout bytes and error (tests inject it; nil ⇒ the real
	// exec.CommandContext execution). The seam is pinned at the raw level so
	// probeHealthy's parse — output-based, exit-code-ignored, "degraded"
	// accepted — always runs against what it returns.
	RunCommand func(ctx context.Context, name string, args ...string) ([]byte, error)

	log   *slog.Logger
	start time.Time

	mu               sync.Mutex
	reasonLogged     bool // realization-status reason logged (once)
	mismatchLogged   bool // SYSTEM_PATH mismatch logged (once)
	unresolvedLogged bool // profile-resolution failure logged (once)

	// signalled latches SIGUSR1 having been sent. Touched only by the single
	// recovery-driver goroutine (timeoutAction is reached at most once), so it
	// needs no mutex.
	signalled bool
}

// FromEnv builds the process-wide NS from the boot env. RIFT_BOOTSTRAP
// unset/empty ⇒ legacy in-system mode: Route passes spawns through untouched,
// Status returns "", RunRecoveryDriver is a no-op.
func FromEnv(log *slog.Logger) *NS {
	if log == nil {
		log = slog.Default()
	}
	n := &NS{
		Bootstrap:             os.Getenv("RIFT_BOOTSTRAP") != "",
		SDPidFile:             DefaultSDPidFile,
		RecoveryBootFile:      DefaultRecoveryBootFile,
		BootRungFile:          DefaultBootRungFile,
		RealizationStatusFile: DefaultRealizationStatusFile,
		ProfilePath:           DefaultProfilePath,
		PersistRoot:           DefaultPersistRoot,
		RunuserPath:           DefaultRunuserPath,
		SystemctlPath:         DefaultSystemctlPath,
		PersistAgentPath:      DefaultPersistAgentPath,
		HealthTimeout:         DefaultHealthTimeout,
		SystemPath:            os.Getenv("SYSTEM_PATH"),
		log:                   log,
		start:                 time.Now(),
	}
	if v := os.Getenv("RIFT_CLIENT_SYSTEM_HEALTH_TIMEOUT"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			n.HealthTimeout = time.Duration(secs) * time.Second
		} else {
			log.Warn("nsexec: bad RIFT_CLIENT_SYSTEM_HEALTH_TIMEOUT (want seconds); using default",
				"value", v, "default", DefaultHealthTimeout)
		}
	}
	return n
}

// Spec describes one spawn the agent is about to perform (a persistent-session
// shell, an SSH exec/shell, or the sftp re-exec).
type Spec struct {
	// Login is the login user inside the client system (runuser -l's target).
	Login string
	// Command is "" for an interactive login shell; otherwise the command line
	// handed to `runuser … -c` (run by the login shell inside the target).
	Command string
	// Env is extra env for the nsenter process, appended to os.Environ().
	// `runuser -l` resets the target environment to a login environment and
	// preserves only TERM, so TERM is the one entry that matters here.
	Env []string
	// Legacy is the byte-for-byte today spawn — used verbatim in legacy mode,
	// and as the fallback when the client system is absent.
	Legacy *exec.Cmd
}

// Spawn is Route's decision.
type Spawn struct {
	// Cmd is the command to start (not yet started).
	Cmd *exec.Cmd
	// Banner, when non-empty, is written to the session's output before wiring
	// I/O so the user is told this is not their environment (§3.6). It
	// accompanies both the fallback spawn (no live client system) and — on a
	// recovery boot — the nsenter spawn, whose target is the second-stage
	// bootstrap system, not the user's environment.
	Banner string
}

// Session banners (§3.6's "explicit banner saying so").
const (
	recoveryBanner = "⚠ recovery boot: this session is in the bootstrap system, not your environment\r\n"
	noClientBanner = "⚠ client system not running: this is a bootstrap shell, not your environment\r\n"
)

// routeSpecObserver is a TEST-ONLY hook, nil in production: when installed via
// SetRouteSpecObserver it receives every Spec passed to Route — covering all
// three production caller sites (sessions.startShell, sshserver exec/shell,
// sshserver handleSFTP) through Route's single funnel. Held atomically so
// consumer-package tests can install/clear it around servers whose handler
// goroutines wind down asynchronously.
var routeSpecObserver atomic.Pointer[func(Spec)]

// SetRouteSpecObserver installs (fn non-nil) or clears (fn nil) the test-only
// captured-Spec hook. Production never calls this.
func SetRouteSpecObserver(fn func(Spec)) {
	if fn == nil {
		routeSpecObserver.Store(nil)
		return
	}
	routeSpecObserver.Store(&fn)
}

// Route decides how one spawn runs. Legacy mode (or a nil *NS): the Legacy
// cmd, untouched, no banner. Bootstrap mode with a live sd-pid: the
// nsenter+runuser form. Bootstrap mode with the client system absent/stale:
// the Legacy cmd plus an explicit banner. A recovery boot carries the
// recovery banner on EITHER form — sd-pid then points at the second-stage
// bootstrap system, so even the nsenter path lands in the bare-base system.
func (n *NS) Route(s Spec) Spawn {
	if ob := routeSpecObserver.Load(); ob != nil {
		(*ob)(s)
	}
	if n == nil || !n.Bootstrap {
		return Spawn{Cmd: s.Legacy}
	}
	banner := ""
	if n.recoveryBoot() {
		banner = recoveryBanner
	}
	pid, ok := n.liveSDPid()
	if !ok {
		if banner == "" {
			banner = noClientBanner
		}
		return Spawn{Cmd: s.Legacy, Banner: banner}
	}
	args := []string{"-t", strconv.Itoa(pid), "-p", "-m", "--", n.RunuserPath, "-l", s.Login}
	if s.Command != "" {
		args = append(args, "-c", s.Command)
	}
	cmd := exec.Command("nsenter", args...)
	cmd.Env = append(os.Environ(), s.Env...)
	// Deliberately no cmd.Dir (the target home may not exist in the agent's
	// mount namespace; runuser -l chdirs to the login home inside the target)
	// and no SysProcAttr.Credential (nsenter needs root; runuser drops
	// privileges inside the target).
	return Spawn{Cmd: cmd, Banner: banner}
}

// InTargetSFTPCommand is the command line the sftp nsenter re-exec runs INSIDE
// the client system: the self-copied /persist/rift/agent (EnsurePersistAgent),
// never the agent's own image-store path — rung 1 binds the volume store over
// the image store, so that path is invisible after nsenter -m, while the
// /persist bind is visible in both namespaces. Interactive/exec sessions keep
// using in-target absolute paths; only sftp re-execs the agent itself. Unused
// in legacy mode (Route never reads Command there); nil-safe for callers that
// never wire an NS.
func (n *NS) InTargetSFTPCommand() string {
	if n == nil {
		return ""
	}
	return n.PersistAgentPath + " sftp-subsystem"
}

// EnsurePersistAgent self-copies the running agent binary to PersistAgentPath
// so the sftp re-exec resolves inside the client system (see
// InTargetSFTPCommand; PR 7 revisits in-box store composition). Bootstrap mode
// only; legacy mode never touches the volume. Idempotent — skips when the
// existing copy is already byte-identical (size + SHA-256) — and atomic (temp
// name + rename), so an in-flight sftp exec never sees a torn binary.
func (n *NS) EnsurePersistAgent() error {
	if n == nil || !n.Bootstrap {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve running binary: %w", err)
	}
	if same, ierr := filesIdentical(self, n.PersistAgentPath); ierr == nil && same {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(n.PersistAgentPath), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", n.PersistAgentPath, os.Getpid())
	if err := copyFile(self, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, n.PersistAgentPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	n.log.Info("nsexec: agent binary self-copied for in-target sftp", "path", n.PersistAgentPath)
	return nil
}

// filesIdentical reports whether dst already carries src's exact bytes. A
// missing dst is (false, nil) — "copy needed", not an error.
func filesIdentical(src, dst string) (bool, error) {
	fs, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	fd, err := os.Stat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if fs.Size() != fd.Size() {
		return false, nil
	}
	hs, err := hashFile(src)
	if err != nil {
		return false, err
	}
	hd, err := hashFile(dst)
	if err != nil {
		return false, err
	}
	return hs == hd, nil
}

func hashFile(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	// Explicit chmod: OpenFile's mode is filtered by the umask.
	if err := out.Chmod(0o755); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Status computes the client_system heartbeat value (§3.7's promote-gate
// field). Bootstrap mode returns exactly one of "recovery" | "unrealized" |
// "realizing" | "healthy"; legacy mode returns "" and the caller omits the key
// entirely (absent = pre-bootstrap agent, the compat arm that still promotes).
func (n *NS) Status() string {
	if n == nil || !n.Bootstrap {
		return ""
	}
	if n.recoveryBoot() {
		return "recovery"
	}
	if !n.profileResolves() {
		// Realization failed or never ran — terminal, promotes nothing and
		// poisons nothing. The realizer may have left a reason; log it once.
		n.logRealizationReasonOnce()
		return "unrealized"
	}
	if n.probeHealthy() {
		return "healthy"
	}
	if time.Since(n.start) < n.HealthTimeout {
		return "realizing"
	}
	// Profile resolves but the client system never reported healthy within the
	// window (the recovery driver has signalled the supervisor if this boot
	// took the profile rung). "unrealized" is the only remaining value that is
	// safe: it neither promotes ("realizing" would misstate a terminal
	// condition as in-progress) nor feeds the boot-poison gate ("recovery"
	// would let a transient poison a good artifact).
	return "unrealized"
}

// RunRecoveryDriver waits up to HealthTimeout for the client system to become
// healthy; on timeout — only when the supervisor started this boot on the
// profile rung (BootRungFile reads "profile") and this is not already a
// recovery boot — it sends SIGUSR1 to the supervisor exactly once. The
// supervisor (the entrypoint script, which traps USR1) is the agent's direct
// PARENT process, NOT VM PID 1 — Fly's own init is PID 1 and the supervisor
// runs as its child. The supervisor owns
// everything after the signal (§3.7's two-stage recovery). Bootstrap mode
// only; a no-op in legacy mode. Run as a goroutine from main.
func (n *NS) RunRecoveryDriver(ctx context.Context) {
	if n == nil || !n.Bootstrap {
		return
	}
	// Crash containment, same as the agent's other side goroutines: a panic
	// here must not exit the process (and end every persistent session).
	defer func() {
		if r := recover(); r != nil {
			n.log.Error("nsexec: recovery driver panic recovered", "panic", r)
		}
	}()
	if n.recoveryBoot() {
		n.log.Info("nsexec: recovery boot — recovery driver idle (never signals twice)")
		return
	}
	deadline := n.start.Add(n.HealthTimeout)
	t := time.NewTicker(recoveryPollInterval)
	defer t.Stop()
	for {
		if n.profileResolves() && n.probeHealthy() {
			n.log.Info("nsexec: client system healthy", "elapsed", time.Since(n.start))
			return
		}
		if time.Now().After(deadline) {
			n.timeoutAction()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// timeoutAction is the driver's on-timeout arm: signal the supervisor for a
// recovery boot, exactly once, and only when the supervisor chose the profile
// rung for this boot (BootRungFile reads "profile") and this is not already a
// recovery boot. The rung file — not sd-pid — is what says "a client system
// was attempted": sd-pid is written for the bootstrap rung too, so arming on
// it would tear down a perfectly healthy bootstrap boot (no realized profile,
// nothing to recover) and recovery-boot the identical image, flipping
// heartbeats to "recovery" and feeding the boot-poison gate for nothing.
func (n *NS) timeoutAction() {
	if n.recoveryBoot() {
		return
	}
	if rung := n.bootRung(); rung != bootRungProfile {
		n.log.Warn("nsexec: client system not healthy within timeout, but this boot did not take the profile rung; not signalling",
			"boot_rung", rung, "timeout", n.HealthTimeout)
		return
	}
	if n.signalled {
		return
	}
	n.signalled = true
	// The supervisor script traps USR1 and is the agent's direct parent; Fly's
	// own init is VM PID 1 and must never be the target. A getppid() of 1 means
	// the agent was orphaned and reparented to init — the supervisor is gone,
	// and signalling would deliver USR1 to Fly's init instead, so log and do
	// nothing.
	ppid := getppidFn()
	if ppid == 1 {
		n.log.Error("nsexec: client system failed to become healthy, but the supervisor is gone (agent reparented to init); not signalling",
			"timeout", n.HealthTimeout)
		return
	}
	n.log.Error("nsexec: client system failed to become healthy; signalling supervisor for recovery boot (SIGUSR1 → parent)",
		"timeout", n.HealthTimeout, "supervisor_pid", ppid)
	if err := killFn(ppid, syscall.SIGUSR1); err != nil {
		n.log.Error("nsexec: SIGUSR1 to supervisor failed", "supervisor_pid", ppid, "err", err)
	}
}

// recoveryBoot reports whether this boot is a recovery boot.
func (n *NS) recoveryBoot() bool {
	_, err := os.Lstat(n.RecoveryBootFile)
	return err == nil
}

// bootRungProfile is the BootRungFile content that means the supervisor
// started a realized client system (ladder rung 1).
const bootRungProfile = "profile"

// bootRung reads the supervisor's chosen rung for the current child —
// "profile" or "bootstrap", trimmed — or "" when the file is absent or
// unreadable.
func (n *NS) bootRung() string {
	b, err := os.ReadFile(n.BootRungFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// liveSDPid reads the handoff pid file and checks the process is still there.
// (0, false) when the file is absent, unparsable, or the process is gone.
// Liveness here serves nsenter ROUTING only — sd-pid presence is not "a
// client system was attempted" (the supervisor writes it for the bootstrap
// rung too); that question is answered by bootRung.
func (n *NS) liveSDPid() (int, bool) {
	b, err := os.ReadFile(n.SDPidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	// kill(pid, 0): existence probe. ESRCH ⇒ stale; EPERM ⇒ alive (can't
	// happen for a root agent, but alive is the right reading regardless).
	if err := killFn(pid, 0); err != nil && err != syscall.EPERM {
		return 0, false
	}
	return pid, true
}

// profileResolves applies §3.7's "healthy is gated on the profile" conjunct:
// the volume profile's symlink chain reaches a logical /nix/store/... target
// whose physical /persist<target> exists, and — when SYSTEM_PATH is set — the
// resolved target equals it.
func (n *NS) profileResolves() bool {
	target, err := n.resolveProfile()
	if err != nil {
		n.mu.Lock()
		logged := n.unresolvedLogged
		n.unresolvedLogged = true
		n.mu.Unlock()
		if !logged {
			n.log.Info("nsexec: volume profile does not resolve", "profile", n.ProfilePath, "err", err)
		}
		return false
	}
	if n.SystemPath != "" && target != n.SystemPath {
		n.mu.Lock()
		logged := n.mismatchLogged
		n.mismatchLogged = true
		n.mu.Unlock()
		if !logged {
			n.log.Warn("nsexec: resolved profile does not match SYSTEM_PATH",
				"resolved", target, "system_path", n.SystemPath)
		}
		return false
	}
	return true
}

// resolveProfile walks the volume profile's symlink chain. Relative targets
// resolve against their physical parent dir; the chain "resolves" when it
// reaches an absolute logical /nix/store/... target whose physical path
// PersistRoot+target exists (link targets are written in logical /nix/store
// form while the store rides the volume under /persist).
func (n *NS) resolveProfile() (string, error) {
	cur := n.ProfilePath
	for hop := 0; hop < maxProfileHops; hop++ {
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("profile chain hit non-symlink %s before any /nix/store target", cur)
		}
		target, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		switch {
		case strings.HasPrefix(target, "/nix/store/"):
			physical := filepath.Join(n.PersistRoot, target)
			if _, err := os.Lstat(physical); err != nil {
				return "", fmt.Errorf("profile target %s: physical %s: %w", target, physical, err)
			}
			return target, nil
		case filepath.IsAbs(target):
			// An absolute non-store target (e.g. /nix/var/nix/profiles/…) is
			// still a logical volume path: keep walking its physical location.
			cur = filepath.Join(n.PersistRoot, target)
		default:
			cur = filepath.Join(filepath.Dir(cur), target)
		}
	}
	return "", fmt.Errorf("profile symlink chain exceeded %d hops (loop?)", maxProfileHops)
}

// probeHealthy runs §3.7's health probe inside the client system:
//
//	nsenter -t <sd-pid> -p -m -- /run/current-system/sw/bin/systemctl is-system-running
//
// "running" or "degraded" counts (degraded = a client's own failing unit is
// their bug, not a brick). is-system-running exits non-zero for every state
// but "running", so the OUTPUT is parsed and the exit code ignored.
func (n *NS) probeHealthy() bool {
	pid, ok := n.liveSDPid()
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	run := n.RunCommand
	if run == nil {
		run = defaultRunCommand
	}
	out, _ := run(ctx, "nsenter",
		"-t", strconv.Itoa(pid), "-p", "-m", "--",
		n.SystemctlPath, "is-system-running")
	state := strings.TrimSpace(string(out))
	return state == "running" || state == "degraded"
}

// defaultRunCommand is the production probe runner: real process execution via
// exec.CommandContext, capturing stdout.
func defaultRunCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// logRealizationReasonOnce surfaces the realizer's failure reason in a log
// line (and nothing else — the §3.7 report records don't exist server-side
// until a later PR, and the server 400s unknown event types).
func (n *NS) logRealizationReasonOnce() {
	n.mu.Lock()
	logged := n.reasonLogged
	n.reasonLogged = true
	n.mu.Unlock()
	if logged {
		return
	}
	b, err := os.ReadFile(n.RealizationStatusFile)
	if err != nil {
		return // no status file — nothing to report
	}
	reason := ""
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "reason="); ok {
			reason = v
			break
		}
	}
	n.log.Warn("nsexec: realization failed", "reason", reason, "status_file", n.RealizationStatusFile)
}
