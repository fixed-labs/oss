package nsexec

// T8(f) — the FromEnv constructor leg (the surface main.go actually uses).
//
// LITERALS RULE (binding): every expectation below is a hardcoded string /
// duration literal, NEVER the exported Default* constants — a constants-based
// compare is a self-comparison that moves with any typo, and this leg is the
// only Go-side pin of these paths. Four of the run/persist paths are also
// asserted on the producer side (T17's rendered init); /persist/rift/agent
// exists ONLY in the Go agent and is pinned here alone. The T9/T10b fixtures
// field-inject ProfilePath/PersistRoot at temp dirs, so a typo'd default there
// (⇒ permanent "unrealized" + spurious recovery on every profile-rung boot) is
// caught by nothing but this test.

import (
	"testing"
	"time"
)

func TestFromEnvBootstrapDefaults(t *testing.T) {
	t.Setenv("RIFT_BOOTSTRAP", "1")
	t.Setenv("SYSTEM_PATH", "/nix/store/rift-fromenv-fixture-system")
	t.Setenv("RIFT_CLIENT_SYSTEM_HEALTH_TIMEOUT", "")
	n := FromEnv(discardLog())
	if !n.Bootstrap {
		t.Fatal("RIFT_BOOTSTRAP=1 must set Bootstrap")
	}
	// ALL NINE path defaults, as hardcoded literals (the file contract with the
	// bootstrap init/supervisor, and the in-target binary paths).
	paths := []struct{ name, got, want string }{
		{"SDPidFile", n.SDPidFile, "/run/rift/sd-pid"},
		{"RecoveryBootFile", n.RecoveryBootFile, "/run/rift/recovery-boot"},
		{"BootRungFile", n.BootRungFile, "/run/rift/boot-rung"},
		{"RealizationStatusFile", n.RealizationStatusFile, "/run/rift/realization-status"},
		{"PersistAgentPath", n.PersistAgentPath, "/persist/rift/agent"},
		{"ProfilePath", n.ProfilePath, "/persist/nix/var/nix/profiles/system"},
		{"PersistRoot", n.PersistRoot, "/persist"},
		{"SystemctlPath", n.SystemctlPath, "/run/current-system/sw/bin/systemctl"},
		{"RunuserPath", n.RunuserPath, "/run/current-system/sw/bin/runuser"},
	}
	for _, p := range paths {
		if p.got != p.want {
			t.Errorf("%s = %q, want literal %q", p.name, p.got, p.want)
		}
	}
	if n.SystemPath != "/nix/store/rift-fromenv-fixture-system" {
		t.Fatalf("SystemPath = %q, want the SYSTEM_PATH env value", n.SystemPath)
	}
	if n.HealthTimeout != 180*time.Second {
		t.Fatalf("HealthTimeout = %v, want the literal 180s default", n.HealthTimeout)
	}
}

func TestFromEnvLegacyWhenBootstrapUnset(t *testing.T) {
	t.Setenv("RIFT_BOOTSTRAP", "")
	n := FromEnv(discardLog())
	if n.Bootstrap {
		t.Fatal("empty RIFT_BOOTSTRAP must leave Bootstrap false (legacy mode)")
	}
}

func TestFromEnvHealthTimeoutParse(t *testing.T) {
	t.Setenv("RIFT_BOOTSTRAP", "1")
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"integer seconds", "7", 7 * time.Second},
		// Bad values warn and fall back — asserted against the literal
		// 180 * time.Second, never DefaultHealthTimeout (a fat-fingered
		// constant would move the expectation with the typo, and T10b always
		// overrides the window, so nothing else pins it).
		{"non-numeric falls back", "nope", 180 * time.Second},
		{"zero is bad", "0", 180 * time.Second},
		{"negative is bad", "-5", 180 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("RIFT_CLIENT_SYSTEM_HEALTH_TIMEOUT", c.env)
			n := FromEnv(discardLog())
			if n.HealthTimeout != c.want {
				t.Fatalf("HealthTimeout for %q = %v, want %v", c.env, n.HealthTimeout, c.want)
			}
		})
	}
}
