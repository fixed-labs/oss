package nsexec

// T9 — Status() state machine (rift-image-model §3.7's promote-gate value).
// The seam-(iii) RAW command runner is injected: it returns output bytes +
// error, so probeHealthy's real parse (output-based, exit-code-ignored,
// "degraded" accepted) stays under test — the stub never decides health.
//
// Every case derives from ONE armed healthy fixture (realistic profile chain,
// live sd-pid, runner returning "running"), varying only the axis under test.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// cmdRec is the injected RAW runner: records every invocation, returns fixed
// (out, err).
type cmdRec struct {
	mu    sync.Mutex
	out   []byte
	err   error
	calls [][]string
}

func (c *cmdRec) run(_ context.Context, name string, args ...string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, append([]string{name}, args...))
	return c.out, c.err
}

func (c *cmdRec) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *cmdRec) last() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return nil
	}
	return slices.Clone(c.calls[len(c.calls)-1])
}

// realisticProfileChain lays down the REALISTIC volume-profile chain under
// root: a relative hop (system → system-3-link), then a logical
// /nix/store/... target whose physical PersistRoot-prefixed path exists — so
// the relative-hop and physical-existence conjuncts are both exercised, not
// just a direct symlink. Returns the logical target.
func realisticProfileChain(t *testing.T, root string) string {
	t.Helper()
	profiles := filepath.Join(root, "nix", "var", "nix", "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	const target = "/nix/store/abc123-rift-system"
	if err := os.MkdirAll(filepath.Join(root, target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("system-3-link", filepath.Join(profiles, "system")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(profiles, "system-3-link")); err != nil {
		t.Fatal(err)
	}
	return target
}

// absoluteHopProfileChain builds a chain whose intermediate hop is an ABSOLUTE
// non-store logical path (re-rooted under PersistRoot) — the third resolver
// branch, whose regression means spurious "unrealized" ⇒ spurious recovery.
func absoluteHopProfileChain(t *testing.T, root string) string {
	t.Helper()
	profiles := filepath.Join(root, "nix", "var", "nix", "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	const target = "/nix/store/def456-rift-system"
	if err := os.MkdirAll(filepath.Join(root, target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/var/nix/profiles/system-4-link", filepath.Join(profiles, "system")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(profiles, "system-4-link")); err != nil {
		t.Fatal(err)
	}
	return target
}

// statusNS builds the armed healthy fixture around an already-laid profile
// chain: live sd-pid, no markers, in-window clock, injected runner.
func statusNS(t *testing.T, root string, rec *cmdRec) *NS {
	t.Helper()
	runDir := filepath.Join(root, "run", "rift")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "sd-pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &NS{
		Bootstrap:             true,
		SDPidFile:             filepath.Join(runDir, "sd-pid"),
		RecoveryBootFile:      filepath.Join(runDir, "recovery-boot"),
		BootRungFile:          filepath.Join(runDir, "boot-rung"),
		RealizationStatusFile: filepath.Join(runDir, "realization-status"),
		ProfilePath:           filepath.Join(root, "nix", "var", "nix", "profiles", "system"),
		PersistRoot:           root,
		SystemctlPath:         "/run/current-system/sw/bin/systemctl",
		RunuserPath:           "/run/current-system/sw/bin/runuser",
		PersistAgentPath:      "/persist/rift/agent",
		HealthTimeout:         time.Hour,
		RunCommand:            rec.run,
		start:                 time.Now(),
		log:                   discardLog(),
	}
}

func healthyStatusNS(t *testing.T, rec *cmdRec) *NS {
	t.Helper()
	root := t.TempDir()
	realisticProfileChain(t, root)
	return statusNS(t, root, rec)
}

func TestStatusHealthyProbesInsideTarget(t *testing.T) {
	rec := &cmdRec{out: []byte("running\n")}
	n := healthyStatusNS(t, rec)
	if got := n.Status(); got != "healthy" {
		t.Fatalf("Status = %q, want healthy", got)
	}
	// The probe is a cross-namespace execution contract: nsenter into the
	// sd-pid target, then the in-target systemctl.
	want := []string{
		"nsenter", "-t", strconv.Itoa(os.Getpid()), "-p", "-m", "--",
		"/run/current-system/sw/bin/systemctl", "is-system-running",
	}
	if !slices.Equal(rec.last(), want) {
		t.Fatalf("probe argv = %q, want %q", rec.last(), want)
	}
}

func TestStatusLegacyReturnsEmpty(t *testing.T) {
	rec := &cmdRec{out: []byte("running\n")}
	n := healthyStatusNS(t, rec) // armed: same fixture reports healthy in bootstrap mode
	n.Bootstrap = false
	if got := n.Status(); got != "" {
		t.Fatalf("legacy Status = %q, want empty (key stays absent)", got)
	}
	if rec.count() != 0 {
		t.Fatal("legacy mode must never probe the client system")
	}
}

func TestStatusNilNSReturnsEmpty(t *testing.T) {
	var n *NS
	if got := n.Status(); got != "" {
		t.Fatalf("nil-NS Status = %q, want empty", got)
	}
}

func TestStatusRecoveryMarkerWins(t *testing.T) {
	rec := &cmdRec{out: []byte("running\n")}
	n := healthyStatusNS(t, rec) // armed: would be healthy without the marker
	if err := os.WriteFile(n.RecoveryBootFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := n.Status(); got != "recovery" {
		t.Fatalf("Status = %q, want recovery", got)
	}
}

func TestStatusUnrealizedWhenProfileAbsentOrDangling(t *testing.T) {
	t.Run("profile absent", func(t *testing.T) {
		rec := &cmdRec{out: []byte("running\n")}
		n := healthyStatusNS(t, rec)
		if err := os.Remove(n.ProfilePath); err != nil {
			t.Fatal(err)
		}
		if got := n.Status(); got != "unrealized" {
			t.Fatalf("Status = %q, want unrealized", got)
		}
	})
	t.Run("dangling physical target", func(t *testing.T) {
		rec := &cmdRec{out: []byte("running\n")}
		n := healthyStatusNS(t, rec)
		// Repoint the final hop at a store path with no physical presence
		// under PersistRoot — the physical-existence conjunct.
		link := filepath.Join(filepath.Dir(n.ProfilePath), "system-3-link")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/nix/store/gone-rift-system", link); err != nil {
			t.Fatal(err)
		}
		if got := n.Status(); got != "unrealized" {
			t.Fatalf("Status = %q, want unrealized", got)
		}
	})
}

func TestStatusRealizingWithinWindow(t *testing.T) {
	rec := &cmdRec{out: []byte("starting\n"), err: errors.New("exit status 1")}
	n := healthyStatusNS(t, rec)
	if got := n.Status(); got != "realizing" {
		t.Fatalf("Status = %q, want realizing", got)
	}
	if rec.count() == 0 {
		t.Fatal("the probe was never run — realizing must be probe-derived, not assumed")
	}
}

// The exit-code-ignored contract: `systemctl is-system-running` exits non-zero
// for every state but "running", so "degraded" WITH a non-zero exit error must
// still parse as healthy (a client's own failing unit is their bug, not a
// brick).
func TestStatusDegradedWithNonZeroExitIsHealthy(t *testing.T) {
	rec := &cmdRec{out: []byte("degraded\n"), err: errors.New("exit status 1")}
	n := healthyStatusNS(t, rec)
	if got := n.Status(); got != "healthy" {
		t.Fatalf("Status = %q, want healthy (output-parsed, exit code ignored)", got)
	}
}

func TestStatusSystemPathGate(t *testing.T) {
	t.Run("match is healthy", func(t *testing.T) {
		rec := &cmdRec{out: []byte("running\n")}
		n := healthyStatusNS(t, rec)
		n.SystemPath = "/nix/store/abc123-rift-system" // == the chain's target
		if got := n.Status(); got != "healthy" {
			t.Fatalf("Status = %q, want healthy (SYSTEM_PATH matches the resolved target)", got)
		}
	})
	t.Run("mismatch is unrealized", func(t *testing.T) {
		rec := &cmdRec{out: []byte("running\n")}
		n := healthyStatusNS(t, rec)
		n.SystemPath = "/nix/store/zzz-other-system"
		if got := n.Status(); got != "unrealized" {
			t.Fatalf("Status = %q, want unrealized (resolved target must equal SYSTEM_PATH)", got)
		}
	})
}

func TestStatusWindowExpiry(t *testing.T) {
	t.Run("expired un-healthy is unrealized", func(t *testing.T) {
		rec := &cmdRec{out: []byte("starting\n"), err: errors.New("exit status 1")}
		n := healthyStatusNS(t, rec)
		n.start = time.Now().Add(-2 * time.Hour) // window (1h) long gone
		if got := n.Status(); got != "unrealized" {
			t.Fatalf("Status = %q, want unrealized (the documented deviation: terminal, not realizing)", got)
		}
	})
	// The probe-before-deadline ordering: a "don't probe after the deadline"
	// refactor would permanently freeze late-healthy/flapping boxes at
	// "unrealized" (unpromotable) with every other case green.
	t.Run("expired but probe now healthy is healthy", func(t *testing.T) {
		rec := &cmdRec{out: []byte("running\n")}
		n := healthyStatusNS(t, rec)
		n.start = time.Now().Add(-2 * time.Hour)
		if got := n.Status(); got != "healthy" {
			t.Fatalf("Status = %q, want healthy (late-healthy must recover)", got)
		}
	})
}

func TestStatusAbsoluteNonStoreHopResolves(t *testing.T) {
	rec := &cmdRec{out: []byte("running\n")}
	root := t.TempDir()
	absoluteHopProfileChain(t, root)
	n := statusNS(t, root, rec)
	if got := n.Status(); got != "healthy" {
		t.Fatalf("Status = %q, want healthy (absolute non-store hop re-rooted under PersistRoot)", got)
	}
}
