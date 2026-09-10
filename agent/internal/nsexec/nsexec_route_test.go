package nsexec

// T8 — Route matrix (rift-image-model §3.6). File paths are injected through
// the NS fields; fixtures live in t.TempDir(). Cross-process contract values
// (paths, argv shapes, banners' identifying tokens) are asserted against
// hardcoded literals, never the production constants that define them.

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Banner-identifying tokens (the §3.6 user-visible contract). The full prose
// is incidental; these tokens are what distinguish the two banners.
const (
	tokNoClient = "client system not running"
	tokRecovery = "recovery boot"
)

// cmdSnap is a deep snapshot of an *exec.Cmd's caller-visible spawn inputs,
// taken BEFORE Route is called — Route may return the same pointer, and
// self-comparison after the call cannot catch a Route that mutates the legacy
// cmd.
type cmdSnap struct {
	path    string
	args    []string
	env     []string
	dir     string
	cred    syscall.Credential
	hasCred bool
}

func snapshotCmd(c *exec.Cmd) cmdSnap {
	s := cmdSnap{
		path: c.Path,
		args: slices.Clone(c.Args),
		env:  slices.Clone(c.Env),
		dir:  c.Dir,
	}
	if c.SysProcAttr != nil && c.SysProcAttr.Credential != nil {
		s.hasCred = true
		s.cred = *c.SysProcAttr.Credential
	}
	return s
}

func assertMatchesSnapshot(t *testing.T, snap cmdSnap, got *exec.Cmd) {
	t.Helper()
	if got == nil {
		t.Fatal("Route returned a nil Cmd")
	}
	if got.Path != snap.path {
		t.Fatalf("Path = %q, want snapshot %q", got.Path, snap.path)
	}
	if !slices.Equal(got.Args, snap.args) {
		t.Fatalf("Args = %q, want snapshot %q", got.Args, snap.args)
	}
	if !slices.Equal(got.Env, snap.env) {
		t.Fatalf("Env = %q, want snapshot %q", got.Env, snap.env)
	}
	if got.Dir != snap.dir {
		t.Fatalf("Dir = %q, want snapshot %q", got.Dir, snap.dir)
	}
	gotCred := got.SysProcAttr != nil && got.SysProcAttr.Credential != nil
	if gotCred != snap.hasCred {
		t.Fatalf("credential presence = %v, want snapshot %v", gotCred, snap.hasCred)
	}
	if snap.hasCred {
		gc := *got.SysProcAttr.Credential
		if gc.Uid != snap.cred.Uid || gc.Gid != snap.cred.Gid ||
			!slices.Equal(gc.Groups, snap.cred.Groups) || gc.NoSetGroups != snap.cred.NoSetGroups {
			t.Fatalf("credential = %+v, want snapshot %+v", gc, snap.cred)
		}
	}
}

// legacySpec builds a Spec whose Legacy cmd carries sentinel values on every
// caller-visible axis (dir, env, credential) so any Route-side mutation or
// substitution is detectable.
func legacySpec() Spec {
	legacy := exec.Command("/bin/sh", "-l")
	legacy.Dir = "/rift-test-home-a"
	legacy.Env = []string{
		"HOME=/rift-test-home-a",
		"USER=rift-user-a",
		"TERM=rift-legacy-term-a",
	}
	legacy.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: 4321, Gid: 8765},
	}
	return Spec{
		Login:  "rift-user-a",
		Env:    []string{"TERM=rift-spec-term-a"},
		Legacy: legacy,
	}
}

// liveBootstrapNS is the shared bootstrap fixture: a live sd-pid (this test
// process's own pid) in a temp dir, no recovery marker, the production
// runuser path. The fallback cases (T8c) reuse it varying ONLY the sd-pid
// axis; the recovery cases (T8d) vary only the marker axis.
func liveBootstrapNS(t *testing.T) (*NS, string) {
	t.Helper()
	dir := t.TempDir()
	sd := filepath.Join(dir, "sd-pid")
	if err := os.WriteFile(sd, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n := &NS{
		Bootstrap:        true,
		SDPidFile:        sd,
		RecoveryBootFile: filepath.Join(dir, "recovery-boot"),
		BootRungFile:     filepath.Join(dir, "boot-rung"),
		RunuserPath:      "/run/current-system/sw/bin/runuser",
		log:              discardLog(),
	}
	return n, dir
}

// T8(a): legacy mode (nil *NS, and Bootstrap:false) — the spawned cmd is
// behaviorally identical to the input Legacy cmd, compared against a snapshot
// captured BEFORE the call; empty banner; probe paths point at a nonexistent
// dir and nothing errors.
func TestRouteLegacyPassthrough(t *testing.T) {
	cases := map[string]func(t *testing.T) *NS{
		"nil NS": func(t *testing.T) *NS { return nil },
		"bootstrap false": func(t *testing.T) *NS {
			miss := filepath.Join(t.TempDir(), "absent")
			return &NS{
				Bootstrap:        false,
				SDPidFile:        filepath.Join(miss, "sd-pid"),
				RecoveryBootFile: filepath.Join(miss, "recovery-boot"),
				BootRungFile:     filepath.Join(miss, "boot-rung"),
				log:              discardLog(),
			}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			n := mk(t)
			spec := legacySpec()
			snap := snapshotCmd(spec.Legacy) // BEFORE Route
			sp := n.Route(spec)
			if sp.Banner != "" {
				t.Fatalf("legacy Route attached a banner: %q", sp.Banner)
			}
			assertMatchesSnapshot(t, snap, sp.Cmd)
		})
	}
}

// T8(b): bootstrap + live sd-pid — the exact nsenter/runuser argv (a
// cross-namespace execution contract; `-p -m` only, no -S/-G), no Credential,
// and Spec.Env appended on top of os.Environ() so the LAST TERM entry (the
// effective value under exec's last-entry-wins semantics) is the Spec's.
func TestRouteBootstrapNsenterArgvAndEnv(t *testing.T) {
	// The sentinel TERM cannot occur ambiently; the ambient TERM is forced to a
	// different known value so a Route that drops Spec.Env is caught (the last
	// TERM entry would then be the ambient one).
	t.Setenv("TERM", "ambient-other")
	const sentinelTerm = "TERM=rift-nsexec-test-1"

	t.Run("interactive shell", func(t *testing.T) {
		n, _ := liveBootstrapNS(t)
		sp := n.Route(Spec{
			Login:  "rift-login-b",
			Env:    []string{sentinelTerm},
			Legacy: exec.Command("/bin/sh", "-l"),
		})
		want := []string{
			"nsenter", "-t", strconv.Itoa(os.Getpid()), "-p", "-m", "--",
			"/run/current-system/sw/bin/runuser", "-l", "rift-login-b",
		}
		if !slices.Equal(sp.Cmd.Args, want) {
			t.Fatalf("argv = %q, want %q", sp.Cmd.Args, want)
		}
		if sp.Cmd.SysProcAttr != nil {
			t.Fatalf("nsenter spawn must carry no SysProcAttr/Credential (runuser drops privileges in-target); got %+v", sp.Cmd.SysProcAttr)
		}
		if sp.Banner != "" {
			t.Fatalf("live-client Route attached a banner: %q", sp.Banner)
		}
		assertEffectiveTERM(t, sp.Cmd.Env, sentinelTerm)
	})

	t.Run("command", func(t *testing.T) {
		n, _ := liveBootstrapNS(t)
		sp := n.Route(Spec{
			Login:   "rift-login-b",
			Command: "echo rift-cmd-b",
			Env:     []string{sentinelTerm},
			Legacy:  exec.Command("/bin/sh", "-c", "echo rift-cmd-b"),
		})
		want := []string{
			"nsenter", "-t", strconv.Itoa(os.Getpid()), "-p", "-m", "--",
			"/run/current-system/sw/bin/runuser", "-l", "rift-login-b",
			"-c", "echo rift-cmd-b",
		}
		if !slices.Equal(sp.Cmd.Args, want) {
			t.Fatalf("argv = %q, want %q", sp.Cmd.Args, want)
		}
		assertEffectiveTERM(t, sp.Cmd.Env, sentinelTerm)
	})
}

// assertEffectiveTERM asserts the LAST TERM= entry equals want (the effective
// value — mere containment would survive an append-order inversion), and that
// the ambient TERM entry is present earlier (proving the env is os.Environ()
// plus the Spec's, and arming the assertion: were Spec.Env dropped, the last
// TERM entry would be the ambient one).
func assertEffectiveTERM(t *testing.T, env []string, want string) {
	t.Helper()
	last, lastIdx, ambientIdx := "", -1, -1
	for i, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			last, lastIdx = e, i
		}
		if e == "TERM=ambient-other" {
			ambientIdx = i
		}
	}
	if lastIdx < 0 {
		t.Fatal("spawn env carries no TERM entry at all")
	}
	if last != want {
		t.Fatalf("last TERM entry = %q, want %q (last-entry-wins is the effective value)", last, want)
	}
	if ambientIdx < 0 || ambientIdx >= lastIdx {
		t.Fatalf("ambient TERM entry not present before the Spec entry (ambient at %d, spec at %d) — the base env must be os.Environ()", ambientIdx, lastIdx)
	}
}

// T8(c): bootstrap with sd-pid absent / empty (the supervisor's own
// `: > sd-pid` truncation — a first-class production state) / unparsable /
// pid-dead — all fall back to the Legacy cmd with the no-client banner. Every
// case reuses the live fixture (the positive anchor asserted first), varying
// only the sd-pid axis.
func TestRouteFallbackWhenClientSystemAbsent(t *testing.T) {
	const deadPid = 999999931 // impossible ambiently (beyond pid_max)
	mutate := map[string]func(t *testing.T, sdPidFile string){
		"absent": func(t *testing.T, sd string) {
			if err := os.Remove(sd); err != nil {
				t.Fatal(err)
			}
		},
		"empty (supervisor truncation)": func(t *testing.T, sd string) {
			if err := os.WriteFile(sd, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"unparsable": func(t *testing.T, sd string) {
			if err := os.WriteFile(sd, []byte("notapid\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"pid dead": func(t *testing.T, sd string) {
			if err := os.WriteFile(sd, []byte(strconv.Itoa(deadPid)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// killFn is the declared OS-boundary seam: the existence probe
			// (signal 0) for the sentinel pid reports ESRCH — a dead process.
			orig := killFn
			killFn = func(pid int, sig syscall.Signal) error {
				if pid == deadPid {
					return syscall.ESRCH
				}
				return orig(pid, sig)
			}
			t.Cleanup(func() { killFn = orig })
		},
	}
	for name, mut := range mutate {
		t.Run(name, func(t *testing.T) {
			n, _ := liveBootstrapNS(t)
			// Positive anchor: the unmutated fixture routes into the client
			// system — proving the fallback below is caused by the mutation,
			// not a broken fixture.
			if pre := n.Route(legacySpec()); pre.Cmd.Args[0] != "nsenter" {
				t.Fatalf("anchor: live fixture did not route via nsenter: argv %q", pre.Cmd.Args)
			}
			mut(t, n.SDPidFile)
			spec := legacySpec()
			snap := snapshotCmd(spec.Legacy)
			sp := n.Route(spec)
			assertMatchesSnapshot(t, snap, sp.Cmd)
			if !strings.Contains(sp.Banner, tokNoClient) {
				t.Fatalf("fallback banner = %q, want the no-client banner (token %q)", sp.Banner, tokNoClient)
			}
		})
	}
}

// T8(d): the recovery marker attaches the recovery banner on BOTH forms — the
// nsenter path (target kept) and the fallback path (where it takes precedence
// over the no-client banner).
func TestRouteRecoveryBannerOnBothPaths(t *testing.T) {
	t.Run("nsenter path keeps target", func(t *testing.T) {
		n, _ := liveBootstrapNS(t)
		if err := os.WriteFile(n.RecoveryBootFile, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		sp := n.Route(legacySpec())
		if sp.Cmd.Args[0] != "nsenter" {
			t.Fatalf("recovery boot must still nsenter into the (second-stage) target; argv %q", sp.Cmd.Args)
		}
		if !strings.Contains(sp.Banner, tokRecovery) {
			t.Fatalf("banner = %q, want the recovery banner (token %q)", sp.Banner, tokRecovery)
		}
	})
	t.Run("fallback path", func(t *testing.T) {
		n, _ := liveBootstrapNS(t)
		if err := os.WriteFile(n.RecoveryBootFile, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(n.SDPidFile); err != nil {
			t.Fatal(err)
		}
		spec := legacySpec()
		snap := snapshotCmd(spec.Legacy)
		sp := n.Route(spec)
		assertMatchesSnapshot(t, snap, sp.Cmd)
		if !strings.Contains(sp.Banner, tokRecovery) {
			t.Fatalf("banner = %q, want the recovery banner (token %q)", sp.Banner, tokRecovery)
		}
		if strings.Contains(sp.Banner, tokNoClient) {
			t.Fatalf("recovery banner must take precedence over the no-client banner; got %q", sp.Banner)
		}
	})
}

// T8(e): the sftp re-exec command inside the client system is the self-copied
// persist agent, never the agent's own image-store path; nil-receiver safe.
func TestInTargetSFTPCommand(t *testing.T) {
	n := &NS{PersistAgentPath: "/persist/rift/agent"}
	if got := n.InTargetSFTPCommand(); got != "/persist/rift/agent sftp-subsystem" {
		t.Fatalf("InTargetSFTPCommand = %q, want %q", got, "/persist/rift/agent sftp-subsystem")
	}
	var nilNS *NS
	if got := nilNS.InTargetSFTPCommand(); got != "" {
		t.Fatalf("nil-receiver InTargetSFTPCommand = %q, want empty", got)
	}
}

// specRecorder is a race-safe capture for the RouteSpecObserver seam (used by
// the consumer-package suites; declared here only where in-package tests need
// it — kept minimal).
type specRecorder struct {
	mu    sync.Mutex
	specs []Spec
}

func (r *specRecorder) add(s Spec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs = append(r.specs, s)
}

// TestRouteSpecObserverSeesEveryCall pins the seam-(vi) funnel itself: with
// the observer installed, Route passes it exactly the caller's Spec; cleared,
// Route runs unhooked.
func TestRouteSpecObserverSeesEveryCall(t *testing.T) {
	rec := &specRecorder{}
	SetRouteSpecObserver(rec.add)
	t.Cleanup(func() { SetRouteSpecObserver(nil) })
	n, _ := liveBootstrapNS(t)
	n.Route(Spec{Login: "rift-observer-login", Command: "rift-observer-cmd", Legacy: exec.Command("/bin/sh")})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.specs) != 1 {
		t.Fatalf("observer saw %d specs, want 1", len(rec.specs))
	}
	if rec.specs[0].Login != "rift-observer-login" || rec.specs[0].Command != "rift-observer-cmd" {
		t.Fatalf("observed spec = %+v", rec.specs[0])
	}
}
