package nsexec

// T10 — timeoutAction arming matrix; T10b — RunRecoveryDriver loop behavior.
// killFn and getppidFn (planned seam ii) are the injected OS boundary; the
// seam-(iii) RAW runner drives health. The rung-file contents ("profile",
// "bootstrap") are a cross-process contract with the supervisor script and are
// written as hardcoded literals.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// sentinelPpid cannot occur ambiently as a real parent pid in these tests.
const sentinelPpid = 424242

type killRec struct {
	mu    sync.Mutex
	calls []struct {
		pid int
		sig syscall.Signal
	}
}

func (k *killRec) signals() []struct {
	pid int
	sig syscall.Signal
} {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]struct {
		pid int
		sig syscall.Signal
	}(nil), k.calls...)
}

// stubKill records real SIGNALS through killFn while keeping signal-0
// existence probes real (liveSDPid's liveness check stays honest).
func stubKill(t *testing.T) *killRec {
	t.Helper()
	rec := &killRec{}
	orig := killFn
	killFn = func(pid int, sig syscall.Signal) error {
		if sig == 0 {
			return orig(pid, sig)
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.calls = append(rec.calls, struct {
			pid int
			sig syscall.Signal
		}{pid, sig})
		return nil
	}
	t.Cleanup(func() { killFn = orig })
	return rec
}

func stubGetppid(t *testing.T, ppid int) {
	t.Helper()
	orig := getppidFn
	getppidFn = func() int { return ppid }
	t.Cleanup(func() { getppidFn = orig })
}

// timeoutNS is the shared T10 fixture; cases vary only the rung/marker axes.
func timeoutNS(t *testing.T) *NS {
	t.Helper()
	dir := t.TempDir()
	return &NS{
		Bootstrap:        true,
		BootRungFile:     filepath.Join(dir, "boot-rung"),
		RecoveryBootFile: filepath.Join(dir, "recovery-boot"),
		HealthTimeout:    time.Second,
		log:              discardLog(),
	}
}

func writeRung(t *testing.T, n *NS, rung string) {
	t.Helper()
	if err := os.WriteFile(n.BootRungFile, []byte(rung+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTimeoutActionArmingMatrix(t *testing.T) {
	// Positive anchor first: the armed case signals — proving every no-signal
	// case below is the guard's doing, not a fixture that never could.
	t.Run("profile rung clean signals exactly once across repeats", func(t *testing.T) {
		rec := stubKill(t)
		stubGetppid(t, sentinelPpid)
		n := timeoutNS(t)
		writeRung(t, n, "profile")
		n.timeoutAction()
		n.timeoutAction()
		n.timeoutAction()
		sigs := rec.signals()
		if len(sigs) != 1 {
			t.Fatalf("signals = %v, want exactly one across repeated invocations", sigs)
		}
		if sigs[0].pid != sentinelPpid || sigs[0].sig != syscall.SIGUSR1 {
			t.Fatalf("signal = %+v, want SIGUSR1 to the injected ppid %d", sigs[0], sentinelPpid)
		}
	})

	noSignal := map[string]func(t *testing.T, n *NS){
		"boot-rung absent": func(t *testing.T, n *NS) {},
		"bootstrap rung": func(t *testing.T, n *NS) {
			writeRung(t, n, "bootstrap")
		},
		"profile rung but recovery boot": func(t *testing.T, n *NS) {
			writeRung(t, n, "profile")
			if err := os.WriteFile(n.RecoveryBootFile, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, arrange := range noSignal {
		t.Run(name+" refuses", func(t *testing.T) {
			rec := stubKill(t)
			stubGetppid(t, sentinelPpid)
			n := timeoutNS(t)
			arrange(t, n)
			n.timeoutAction()
			if sigs := rec.signals(); len(sigs) != 0 {
				t.Fatalf("signals = %v, want none", sigs)
			}
		})
	}

	// The orphan refusal: getppid()==1 means the supervisor is gone and the
	// signal would land on Fly's init. The refusal (zero signals) is the
	// observable.
	t.Run("orphaned agent refuses", func(t *testing.T) {
		rec := stubKill(t)
		stubGetppid(t, 1)
		n := timeoutNS(t)
		writeRung(t, n, "profile") // armed: with a real ppid this fixture signals
		n.timeoutAction()
		if sigs := rec.signals(); len(sigs) != 0 {
			t.Fatalf("signals = %v, want none (never signal init)", sigs)
		}
	})
}

// --- T10b: RunRecoveryDriver ------------------------------------------------

// driverFixture builds the ARMED driver fixture through FromEnv (window = 1 s
// via RIFT_CLIENT_SYSTEM_HEALTH_TIMEOUT — the parse itself is pinned in T8f):
// resolving profile chain, live sd-pid, boot-rung "profile", injected
// killFn/getppidFn, injected RAW runner returning (out, err).
func driverFixture(t *testing.T, out []byte, err error) (*NS, *killRec, *cmdRec) {
	t.Helper()
	t.Setenv("RIFT_BOOTSTRAP", "1")
	t.Setenv("RIFT_CLIENT_SYSTEM_HEALTH_TIMEOUT", "1")
	t.Setenv("SYSTEM_PATH", "")
	n := FromEnv(discardLog())

	root := t.TempDir()
	realisticProfileChain(t, root)
	runDir := filepath.Join(root, "run", "rift")
	if merr := os.MkdirAll(runDir, 0o755); merr != nil {
		t.Fatal(merr)
	}
	if werr := os.WriteFile(filepath.Join(runDir, "sd-pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	n.SDPidFile = filepath.Join(runDir, "sd-pid")
	n.RecoveryBootFile = filepath.Join(runDir, "recovery-boot")
	n.BootRungFile = filepath.Join(runDir, "boot-rung")
	n.RealizationStatusFile = filepath.Join(runDir, "realization-status")
	n.ProfilePath = filepath.Join(root, "nix", "var", "nix", "profiles", "system")
	n.PersistRoot = root
	writeRung(t, n, "profile")

	rec := stubKill(t)
	stubGetppid(t, sentinelPpid)
	cr := &cmdRec{out: out, err: err}
	n.RunCommand = cr.run
	return n, rec, cr
}

func runDriver(t *testing.T, n *NS, within time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		n.RunRecoveryDriver(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("RunRecoveryDriver did not return within %v", within)
	}
}

// (a) Healthy before the deadline: the driver returns without signalling. The
// fixture is (b)'s ARMED fixture with ONLY health flipped — with no rung file,
// timeoutAction's own non-profile refusal would produce the same observables
// under a health-ignoring loop; here a deleted health check would signal at
// the first post-deadline poll and trip the zero-signal assertion.
func TestRecoveryDriverHealthyReturnsWithoutSignalling(t *testing.T) {
	n, rec, cr := driverFixture(t, []byte("running\n"), nil)
	runDriver(t, n, 4*time.Second)
	if sigs := rec.signals(); len(sigs) != 0 {
		t.Fatalf("signals = %v, want none (client system became healthy)", sigs)
	}
	if cr.count() == 0 {
		t.Fatal("the probe runner was never invoked — 'healthy' must come from the probe, not a short-circuit")
	}
}

// (b) Unhealthy past the 1 s window (profile rung): exactly one SIGUSR1 to the
// injected ppid. The driver polls on its fixed cadence, so the return lands at
// the first post-deadline poll.
func TestRecoveryDriverTimeoutSignalsSupervisorOnce(t *testing.T) {
	n, rec, cr := driverFixture(t, []byte("starting\n"), errors.New("exit status 1"))
	runDriver(t, n, 20*time.Second)
	sigs := rec.signals()
	if len(sigs) != 1 {
		t.Fatalf("signals = %v, want exactly one", sigs)
	}
	if sigs[0].pid != sentinelPpid || sigs[0].sig != syscall.SIGUSR1 {
		t.Fatalf("signal = %+v, want SIGUSR1 to the injected ppid %d", sigs[0], sentinelPpid)
	}
	if cr.count() == 0 {
		t.Fatal("the probe runner was never invoked — the timeout must be reached through real polling")
	}
}

// (c) Recovery boot: the driver idles (never signals twice). The fixture is
// the ENRICHED armed one — resolving profile, live sd-pid, runner returning
// "running" — so "the runner is NEVER invoked" is a hard, timing-free
// discriminator: deleting the recovery early-return forces a runner invocation
// on the first iteration (a bare marker-only fixture would keep the runner
// un-invoked via the profileResolves short-circuit even with the guard
// deleted).
func TestRecoveryDriverIdlesOnRecoveryBoot(t *testing.T) {
	n, rec, cr := driverFixture(t, []byte("running\n"), nil)
	if err := os.WriteFile(n.RecoveryBootFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	runDriver(t, n, 4*time.Second)
	if got := cr.count(); got != 0 {
		t.Fatalf("probe runner invoked %d times, want 0 (driver must idle on a recovery boot)", got)
	}
	if sigs := rec.signals(); len(sigs) != 0 {
		t.Fatalf("signals = %v, want none", sigs)
	}
}
