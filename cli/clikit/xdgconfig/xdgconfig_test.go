package xdgconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// getenvFrom returns a getenv func backed by a map (hermetic — no process env).
func getenvFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestEnvNameProdAndUnset(t *testing.T) {
	// Unset selector → prod.
	if got, err := EnvName(getenvFrom(nil), "RIFT_ENV", "RIFT_WORKSPACE_ID"); err != nil || got != "prod" {
		t.Fatalf("unset: got %q err %v, want prod", got, err)
	}
	// Explicit "prod" behaves as unset.
	if got, err := EnvName(getenvFrom(map[string]string{"RIFT_ENV": "prod"}), "RIFT_ENV", "RIFT_WORKSPACE_ID"); err != nil || got != "prod" {
		t.Fatalf("explicit prod: got %q err %v", got, err)
	}
}

func TestEnvNameValidNonProd(t *testing.T) {
	got, err := EnvName(getenvFrom(map[string]string{"RIFT_ENV": "staging"}), "RIFT_ENV", "RIFT_WORKSPACE_ID")
	if err != nil || got != "staging" {
		t.Fatalf("got %q err %v, want staging", got, err)
	}
}

func TestEnvNameInvalidGrammarErrors(t *testing.T) {
	for _, bad := range []string{"../x", "Staging", "a b", "-lead", "UPPER"} {
		if _, err := EnvName(getenvFrom(map[string]string{"RIFT_ENV": bad}), "RIFT_ENV", "RIFT_WORKSPACE_ID"); err == nil {
			t.Fatalf("%q must error", bad)
		}
	}
}

// Machine mode: a non-empty discriminator var short-circuits to prod WITHOUT
// reading the selector — even an invalid selector value can neither redirect
// nor break it.
func TestEnvNameMachineModeShortCircuits(t *testing.T) {
	got, err := EnvName(getenvFrom(map[string]string{"RIFT_WORKSPACE_ID": "ws-1", "RIFT_ENV": "../evil"}),
		"RIFT_ENV", "RIFT_WORKSPACE_ID")
	if err != nil || got != "prod" {
		t.Fatalf("machine mode: got %q err %v, want prod (selector not read)", got, err)
	}
	// An EMPTY discriminator value ≡ absent → NOT machine mode → selector is read.
	got, err = EnvName(getenvFrom(map[string]string{"RIFT_WORKSPACE_ID": "", "RIFT_ENV": "staging"}),
		"RIFT_ENV", "RIFT_WORKSPACE_ID")
	if err != nil || got != "staging" {
		t.Fatalf("empty workspace-id must not be machine mode: got %q err %v", got, err)
	}
}

// fplctl passes "" for machineDiscrimVar → it can NEVER short-circuit, even
// with a workspace-id var set in the environment.
func TestEnvNameNoMachineModeWhenDiscrimEmpty(t *testing.T) {
	got, err := EnvName(getenvFrom(map[string]string{"RIFT_WORKSPACE_ID": "ws-1", "FPLCTL_ENV": "staging"}),
		"FPLCTL_ENV", "")
	if err != nil || got != "staging" {
		t.Fatalf("no machine mode: got %q err %v, want staging", got, err)
	}
}

func TestSessionPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xcfg-test")
	prod, err := SessionPath("rift", "prod")
	if err != nil || prod != "/tmp/xcfg-test/rift/config.json" {
		t.Fatalf("prod path: %q err %v", prod, err)
	}
	named, err := SessionPath("rift", "staging")
	if err != nil || named != "/tmp/xcfg-test/rift/config.staging.json" {
		t.Fatalf("named path: %q err %v", named, err)
	}
}

func TestLoadMissingAndSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	// Missing file → zero Session, no error.
	if s, err := Load(p); err != nil || s.APIBaseURL != "" || s.Token != "" {
		t.Fatalf("missing load: %+v err %v", s, err)
	}
	if err := Save(p, &Session{APIBaseURL: "https://x", Token: "tok"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(p)
	if err != nil || got.APIBaseURL != "https://x" || got.Token != "tok" {
		t.Fatalf("round-trip: %+v err %v", got, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm: %v, want 0600", fi.Mode().Perm())
	}
}
