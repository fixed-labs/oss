package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withConfigHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// The suite may run from inside a `rift-env <env>` subshell; a leaked
	// RIFT_ENV would redirect every hermetic test to config.<env>.json (or,
	// if invalid, make path() error). Clear the selector so hermetic tests
	// always target prod's config.json unless they set RIFT_ENV themselves.
	t.Setenv("RIFT_ENV", "")
	// macOS os.UserConfigDir ignores XDG; gate these tests to the XDG path.
	if home, _ := os.UserConfigDir(); home != dir {
		t.Skip("UserConfigDir doesn't honor XDG_CONFIG_HOME on this platform")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withConfigHome(t)
	want := &Config{APIBaseURL: "https://api.example.dev", Token: "tok"}
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

// configPath returns the absolute path where a session with the given env name
// persists, computed off the package's own dir() so the test tracks the real
// layout. env "prod" → config.json; otherwise config.<env>.json.
func configPath(t *testing.T, env string) string {
	t.Helper()
	d, err := dir()
	if err != nil {
		t.Fatalf("dir(): %v", err)
	}
	if env == "prod" {
		return filepath.Join(d, "config.json")
	}
	return filepath.Join(d, "config."+env+".json")
}

func fileExists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", p, err)
	return false
}

// 1.1 — EnvName() is "prod" both when RIFT_ENV is unset and when explicitly
// "prod", and the two are byte-identical: Save() writes <dir>/config.json and
// never forks a config.prod.json.
func TestEnvNameProdDefaultAndExplicit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		set   bool
	}{
		{"unset", "", false},
		{"explicit-prod", "prod", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withConfigHome(t) // clears RIFT_ENV to ""
			if tc.set {
				t.Setenv("RIFT_ENV", tc.value)
			}
			got, err := EnvName()
			if err != nil {
				t.Fatalf("EnvName(): %v", err)
			}
			if got != "prod" {
				t.Fatalf("EnvName() = %q, want prod", got)
			}
			if err := (&Config{APIBaseURL: "u", Token: "t"}).Save(); err != nil {
				t.Fatalf("Save(): %v", err)
			}
			if !fileExists(t, configPath(t, "prod")) {
				t.Fatalf("prod Save() must write config.json at %s", configPath(t, "prod"))
			}
			// config.prod.json must never be forked for the prod session.
			d, _ := dir()
			if fileExists(t, filepath.Join(d, "config.prod.json")) {
				t.Fatal("prod must not fork a config.prod.json")
			}
		})
	}
}

// 1.2 — valid non-prod selectors resolve to themselves with no error, including
// leading-digit (2x) and embedded-hyphen (dev-2) boundary cases.
func TestEnvNameValidNonProd(t *testing.T) {
	for _, name := range []string{"staging", "dev-2", "2x"} {
		t.Run(name, func(t *testing.T) {
			withConfigHome(t)
			t.Setenv("RIFT_ENV", name)
			got, err := EnvName()
			if err != nil {
				t.Fatalf("EnvName() on valid %q: %v", name, err)
			}
			if got != name {
				t.Fatalf("EnvName() = %q, want %q", got, name)
			}
		})
	}
}

// 1.3 — malformed selectors error, name RIFT_ENV in the message, and touch no
// file on disk.
func TestEnvNameInvalidErrors(t *testing.T) {
	for _, bad := range []string{"../x", "Staging", "a_b", "-lead"} {
		t.Run(bad, func(t *testing.T) {
			withConfigHome(t)
			t.Setenv("RIFT_ENV", bad)
			_, err := EnvName()
			if err == nil {
				t.Fatalf("EnvName() on malformed %q: want error", bad)
			}
			if !strings.Contains(err.Error(), "RIFT_ENV") {
				t.Fatalf("error %q must name RIFT_ENV", err.Error())
			}
			d, _ := dir()
			entries, statErr := os.ReadDir(d)
			// dir() itself is never created for a bad env (Save/path short-circuit).
			if statErr == nil && len(entries) != 0 {
				t.Fatalf("a malformed env must touch no file, found %d entries in %s", len(entries), d)
			}
		})
	}
}

// 1.4 — machine mode: a non-empty RIFT_WORKSPACE_ID makes EnvName() return
// "prod" without reading RIFT_ENV, even when RIFT_ENV is invalid; Save()/Load()
// use config.json.
func TestEnvNameMachineModeIgnoresInvalidEnv(t *testing.T) {
	withConfigHome(t)
	t.Setenv("RIFT_WORKSPACE_ID", "ws-self")
	t.Setenv("RIFT_ENV", "../x")
	got, err := EnvName()
	if err != nil {
		t.Fatalf("machine mode must not error on stray RIFT_ENV: %v", err)
	}
	if got != "prod" {
		t.Fatalf("EnvName() = %q, want prod in machine mode", got)
	}
	if err := (&Config{APIBaseURL: "u", Token: "t"}).Save(); err != nil {
		t.Fatalf("Save() in machine mode: %v", err)
	}
	if !fileExists(t, configPath(t, "prod")) {
		t.Fatal("machine mode Save() must write config.json")
	}
}

// 1.4b — an EMPTY RIFT_WORKSPACE_ID is NOT machine mode (the check is != "",
// not mere presence): RIFT_ENV=staging is honored.
func TestEnvNameEmptyWorkspaceIDIsNotMachineMode(t *testing.T) {
	withConfigHome(t)
	t.Setenv("RIFT_WORKSPACE_ID", "")
	t.Setenv("RIFT_ENV", "staging")
	got, err := EnvName()
	if err != nil {
		t.Fatalf("EnvName(): %v", err)
	}
	if got != "staging" {
		t.Fatalf("empty RIFT_WORKSPACE_ID must not trigger machine mode: EnvName() = %q, want staging", got)
	}
}

// 1.5 — Save-under-env reroutes to config.<env>.json and leaves an existing
// prod config.json byte-unchanged; Load() under the env reads the new one back.
func TestSaveUnderEnvLeavesProdUnchanged(t *testing.T) {
	withConfigHome(t)
	// Seed a prod config.json (RIFT_ENV cleared by withConfigHome).
	prod := &Config{APIBaseURL: "https://prod.example", Token: "prod-tok"}
	if err := prod.Save(); err != nil {
		t.Fatalf("seed prod Save(): %v", err)
	}
	prodPath := configPath(t, "prod")
	prodBefore, err := os.ReadFile(prodPath)
	if err != nil {
		t.Fatalf("read seeded prod: %v", err)
	}

	// Save a DIFFERENT config under staging.
	t.Setenv("RIFT_ENV", "staging")
	staging := &Config{APIBaseURL: "https://staging.example", Token: "staging-tok"}
	if err := staging.Save(); err != nil {
		t.Fatalf("staging Save(): %v", err)
	}

	// config.staging.json holds the new one.
	got, err := Load()
	if err != nil {
		t.Fatalf("staging Load(): %v", err)
	}
	if *got != *staging {
		t.Fatalf("staging Load() = %+v, want %+v", got, staging)
	}
	if !fileExists(t, configPath(t, "staging")) {
		t.Fatal("staging Save() must write config.staging.json")
	}

	// config.json is byte-unchanged.
	prodAfter, err := os.ReadFile(prodPath)
	if err != nil {
		t.Fatalf("re-read prod: %v", err)
	}
	if string(prodBefore) != string(prodAfter) {
		t.Fatalf("prod config.json must be byte-unchanged: before=%q after=%q", prodBefore, prodAfter)
	}
}

// 1.6 — prod and staging sessions coexist; Load() under each returns its own
// {APIBaseURL,Token}.
func TestProdAndStagingCoexist(t *testing.T) {
	withConfigHome(t)
	prod := &Config{APIBaseURL: "https://prod.example", Token: "prod-tok"}
	if err := prod.Save(); err != nil {
		t.Fatalf("prod Save(): %v", err)
	}
	t.Setenv("RIFT_ENV", "staging")
	staging := &Config{APIBaseURL: "https://staging.example", Token: "staging-tok"}
	if err := staging.Save(); err != nil {
		t.Fatalf("staging Save(): %v", err)
	}

	gotStaging, err := Load()
	if err != nil {
		t.Fatalf("staging Load(): %v", err)
	}
	if *gotStaging != *staging {
		t.Fatalf("staging Load() = %+v, want %+v", gotStaging, staging)
	}

	t.Setenv("RIFT_ENV", "prod")
	gotProd, err := Load()
	if err != nil {
		t.Fatalf("prod Load(): %v", err)
	}
	if *gotProd != *prod {
		t.Fatalf("prod Load() = %+v, want %+v", gotProd, prod)
	}
}

// 1.7 — Validate() names the env for BOTH branches. Under prod the wording is
// the two known literals byte-for-byte; under RIFT_ENV=staging both messages
// carry the (RIFT_ENV=staging) suffix and `rift login`.
func TestValidateEnvNamed(t *testing.T) {
	// Prod wording is a hard byte-for-byte invariant (RIFT_ENV cleared).
	withConfigHome(t)
	const wantNoURL = "no API URL configured — run `rift login` (or set RIFT_API_URL)"
	const wantNoToken = "not logged in — run `rift login`"

	if err := (&Config{Token: "t"}).Validate(); err == nil {
		t.Fatal("missing URL should fail validate")
	} else if err.Error() != wantNoURL {
		t.Fatalf("prod missing-URL wording drifted:\n got %q\nwant %q", err.Error(), wantNoURL)
	}
	// Empty config falls into the no-URL branch (URL checked first).
	if err := (&Config{}).Validate(); err == nil {
		t.Fatal("empty config should fail validate")
	} else if err.Error() != wantNoURL {
		t.Fatalf("prod empty-config wording drifted:\n got %q\nwant %q", err.Error(), wantNoURL)
	}
	if err := (&Config{APIBaseURL: "u"}).Validate(); err == nil {
		t.Fatal("missing token should fail validate")
	} else if err.Error() != wantNoToken {
		t.Fatalf("prod missing-token wording drifted:\n got %q\nwant %q", err.Error(), wantNoToken)
	}

	// Non-prod: both branches name the env (substring assertions).
	t.Setenv("RIFT_ENV", "staging")
	noURL := (&Config{Token: "t"}).Validate()
	if noURL == nil {
		t.Fatal("staging missing URL should fail validate")
	}
	if !strings.Contains(noURL.Error(), "(RIFT_ENV=staging)") || !strings.Contains(noURL.Error(), "rift login") {
		t.Fatalf("staging missing-URL must name env and rift login: %q", noURL.Error())
	}
	noToken := (&Config{APIBaseURL: "u"}).Validate()
	if noToken == nil {
		t.Fatal("staging missing token should fail validate")
	}
	if !strings.Contains(noToken.Error(), "(RIFT_ENV=staging)") || !strings.Contains(noToken.Error(), "rift login") {
		t.Fatalf("staging missing-token must name env and rift login: %q", noToken.Error())
	}
}

// 1.8 — Load() and Save() PROPAGATE the EnvName error for a malformed selector
// (path() must not swallow it); neither writes a config file.
func TestLoadSavePropagateInvalidEnvErrors(t *testing.T) {
	withConfigHome(t)
	t.Setenv("RIFT_ENV", "../x")

	if _, err := Load(); err == nil {
		t.Fatal("Load() under invalid RIFT_ENV must return an error")
	}
	if err := (&Config{APIBaseURL: "u", Token: "t"}).Save(); err == nil {
		t.Fatal("Save() under invalid RIFT_ENV must return an error")
	}

	// No config file written anywhere under the config dir.
	d, _ := dir()
	if entries, statErr := os.ReadDir(d); statErr == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "config") {
				t.Fatalf("invalid env must write no config file, found %s", e.Name())
			}
		}
	}
}
