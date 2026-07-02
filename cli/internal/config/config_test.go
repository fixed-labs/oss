package config

import (
	"os"
	"testing"
)

func withConfigHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// macOS os.UserConfigDir ignores XDG; gate these tests to the XDG path.
	if home, _ := os.UserConfigDir(); home != dir {
		t.Skip("UserConfigDir doesn't honor XDG_CONFIG_HOME on this platform")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withConfigHome(t)
	want := &Config{APIBaseURL: "https://api.example.dev", Token: "tok", DefaultContext: "company:c1"}
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *got != *want {
		t.Fatalf("round trip: %+v != %+v", got, want)
	}
}

func TestLoadMissingIsZero(t *testing.T) {
	withConfigHome(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if c.APIBaseURL != "" || c.Token != "" {
		t.Fatalf("expected zero config, got %+v", c)
	}
}

func TestConfigFileIs0600(t *testing.T) {
	withConfigHome(t)
	c := &Config{APIBaseURL: "u", Token: "t"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	p, _ := path()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600 (holds bearer)", info.Mode().Perm())
	}
}

// On a laptop (no RIFT_WORKSPACE_ID) the RIFT_TOKEN/RIFT_API_URL env
// vars are NOT a credential source — the developer authenticates with
// `devbox login`, so the token/API base come solely from the saved file and
// stray env vars are ignored. (The interim RIFT_TOKEN developer override was
// removed once device-flow login shipped.)
func TestLaptopIgnoresEnvOverride(t *testing.T) {
	withConfigHome(t)
	(&Config{APIBaseURL: "https://file.example", Token: "file-tok"}).Save()
	t.Setenv("RIFT_API_URL", "https://env.example")
	t.Setenv("RIFT_TOKEN", "env-tok")
	// RIFT_WORKSPACE_ID deliberately unset → laptop mode.
	c, err := FromEnvOrFile()
	if err != nil {
		t.Fatal(err)
	}
	if c.APIBaseURL != "https://file.example" || c.Token != "file-tok" {
		t.Fatalf("laptop must ignore env, using the login file: %+v", c)
	}
	if c.MachineWorkspaceID != "" {
		t.Fatalf("laptop must not be machine mode: %+v", c)
	}
}

// In-VM (RIFT_WORKSPACE_ID set, i.e. machine mode) the env override STILL
// applies: the provisioner injects RIFT_API_URL/RIFT_TOKEN alongside it,
// and those machine credentials win over any (typically empty) login file.
// This preserves the in-VM machine path.
func TestInVMHonorsEnvOverride(t *testing.T) {
	withConfigHome(t)
	(&Config{APIBaseURL: "https://file.example", Token: "file-tok"}).Save()
	t.Setenv("RIFT_WORKSPACE_ID", "ws-self")
	t.Setenv("RIFT_API_URL", "https://env.example")
	t.Setenv("RIFT_TOKEN", "env-tok")
	c, err := FromEnvOrFile()
	if err != nil {
		t.Fatal(err)
	}
	if c.APIBaseURL != "https://env.example" || c.Token != "env-tok" {
		t.Fatalf("in-VM machine env should win: %+v", c)
	}
	if c.MachineWorkspaceID != "ws-self" {
		t.Fatalf("RIFT_WORKSPACE_ID should flag machine mode: %+v", c)
	}
}

func TestMachineWorkspaceIDFromEnv(t *testing.T) {
	withConfigHome(t)
	c, err := FromEnvOrFile()
	if err != nil {
		t.Fatal(err)
	}
	if c.MachineWorkspaceID != "" {
		t.Fatalf("laptop (no RIFT_WORKSPACE_ID) must not be machine mode: %+v", c)
	}
	t.Setenv("RIFT_WORKSPACE_ID", "ws-self")
	c, err = FromEnvOrFile()
	if err != nil {
		t.Fatal(err)
	}
	if c.MachineWorkspaceID != "ws-self" {
		t.Fatalf("RIFT_WORKSPACE_ID should flag machine mode: %+v", c)
	}
}

func TestMachineWorkspaceIDNeverPersisted(t *testing.T) {
	withConfigHome(t)
	c := &Config{APIBaseURL: "u", Token: "t", MachineWorkspaceID: "ws-self"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineWorkspaceID != "" {
		t.Fatalf("MachineWorkspaceID is env-only and must not round-trip: %+v", got)
	}
}

func TestValidate(t *testing.T) {
	if err := (&Config{}).Validate(); err == nil {
		t.Fatal("empty config should fail validate")
	}
	if err := (&Config{APIBaseURL: "u"}).Validate(); err == nil {
		t.Fatal("missing token should fail validate")
	}
	if err := (&Config{APIBaseURL: "u", Token: "t"}).Validate(); err != nil {
		t.Fatalf("complete config should validate: %v", err)
	}
}
