// Package xdgconfig is the shared per-environment session store for the
// fixed-labs CLIs. It routes a tool's {api_base_url, token} to
// ~/.config/<app>/config.json (the prod session) or config.<env>.json (a named
// session), selected by a per-tool env var, and owns the env-name grammar as
// the single Go source of truth. Stdlib only. Each CLI's config package wraps
// these primitives with its own machine-mode / override behavior.
package xdgconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Session is the persisted credential pair.
type Session struct {
	APIBaseURL string `json:"api_base_url"`
	Token      string `json:"token"`
}

// envNameRE is the env-name grammar — the SINGLE Go source of truth (rift and
// fplctl both call EnvName; the only other copy is the bash one in
// bin/rift-env). A name is interpolated into a config filename, so this doubles
// as a path-safety boundary.
var envNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// EnvName resolves the active session name. If machineDiscrimVar != "" and
// getenv(machineDiscrimVar) != "", it returns "prod" WITHOUT reading
// selectorVar — the machine-mode short-circuit (rift in-VM), so a stray
// selector var in a box shell can neither redirect nor break lifecycle verbs.
// Otherwise getenv(selectorVar): "" or "prod" ⇒ "prod"; any other value must
// match the grammar or this returns an error. machineDiscrimVar == "" means the
// tool has no machine mode (fplctl), so no short-circuit can apply.
func EnvName(getenv func(string) string, selectorVar, machineDiscrimVar string) (string, error) {
	if machineDiscrimVar != "" && getenv(machineDiscrimVar) != "" {
		return "prod", nil
	}
	v := getenv(selectorVar)
	if v == "" || v == "prod" {
		return "prod", nil
	}
	if !envNameRE.MatchString(v) {
		return "", fmt.Errorf("invalid %s %q: must match ^[a-z0-9][a-z0-9-]*$", selectorVar, v)
	}
	return v, nil
}

// SessionPath is the session file for env under os.UserConfigDir()/appName:
// config.json for the prod session, else config.<env>.json.
func SessionPath(appName, env string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, appName)
	if env == "prod" {
		return filepath.Join(dir, "config.json"), nil
	}
	return filepath.Join(dir, "config."+env+".json"), nil
}

// Load reads the session file; a missing file yields a zero Session (not an
// error — a fresh CLI just isn't logged in yet).
func Load(path string) (*Session, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Session{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &s, nil
}

// Save writes the session 0600 (it holds the bearer), creating the parent dir
// 0700 if needed.
func Save(path string, s *Session) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
