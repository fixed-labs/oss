// Package config persists the rift developer CLI's local state — the API base
// URL and the bearer minted by `rift login` — under the user's config dir (XDG:
// ~/.config/rift/config.json, or config.<env>.json for a named RIFT_ENV
// session), 0600 since it holds the bearer. It wraps the shared clikit/xdgconfig
// per-env session store with rift's machine-mode behavior.
//
// The same static binary is baked into the workspace image, where the in-VM
// `rift resize`/`keepalive`/`suspend` instead read RIFT_API_URL + RIFT_TOKEN +
// RIFT_WORKSPACE_ID from the machine env the provisioner injects. The in-VM
// token is a MACHINE bearer (subject = the workspace-id). RIFT_WORKSPACE_ID is
// the machine-mode discriminator: its presence routes EnvName to "prod" without
// reading RIFT_ENV, and makes FromEnvOrFile honor the RIFT_TOKEN/RIFT_API_URL
// overrides. A laptop sets none of the three.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/fixed-labs/oss/cli/clikit/xdgconfig"
)

// DefaultAPIBaseURL is the hosted production control plane. `rift login` falls
// back to it when neither --api / RIFT_API_URL nor a previously-saved config
// supplies a URL, so a first-time login on a laptop needs no flag.
const DefaultAPIBaseURL = "https://fixedlabs.dev"

type Config struct {
	APIBaseURL string `json:"api_base_url"`
	Token      string `json:"token"`
	// MachineWorkspaceID is set only in-VM (RIFT_WORKSPACE_ID). Its presence
	// means Token is a machine token whose subject is this workspace, so
	// lifecycle verbs must use the machine-token agent routes and may only act
	// on this workspace. Never persisted.
	MachineWorkspaceID string `json:"-"`
}

// dir is rift's config directory (~/.config/rift). The env-agnostic store
// gen-epochs.json (genepoch.go) builds off this directly, not config.path().
func dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "rift"), nil
}

// EnvName returns the active session name via the shared grammar, with rift's
// machine-mode short-circuit: a non-empty RIFT_WORKSPACE_ID means in-VM —
// resolve to "prod" WITHOUT reading RIFT_ENV, so a stray var in a box shell can
// neither redirect nor break lifecycle verbs. Otherwise RIFT_ENV selects the
// session; an invalid name errors.
func EnvName() (string, error) {
	return xdgconfig.EnvName(os.Getenv, "RIFT_ENV", "RIFT_WORKSPACE_ID")
}

// path returns the active session file: config.json for the prod session, else
// config.<env>.json (via the shared router; propagates an invalid-env error).
func path() (string, error) {
	env, err := EnvName()
	if err != nil {
		return "", err
	}
	return xdgconfig.SessionPath("rift", env)
}

// Load reads the persisted config; a missing file yields a zero Config (not an
// error — a fresh CLI just isn't logged in yet). An invalid RIFT_ENV errors
// (via path()), never a silent fall-through to prod.
func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	s, err := xdgconfig.Load(p)
	if err != nil {
		return nil, err
	}
	return &Config{APIBaseURL: s.APIBaseURL, Token: s.Token}, nil
}

// Save writes the config 0600 (it holds the bearer) to the active session file,
// so a non-prod session persists to config.<env>.json, not prod's config.json.
func (c *Config) Save() error {
	p, err := path()
	if err != nil {
		return err
	}
	return xdgconfig.Save(p, &xdgconfig.Session{APIBaseURL: c.APIBaseURL, Token: c.Token})
}

// FromEnvOrFile resolves the effective {APIBaseURL, Token}. In machine mode —
// only when RIFT_WORKSPACE_ID is set — the env (RIFT_API_URL / RIFT_TOKEN,
// injected by the provisioner) wins. On a laptop RIFT_WORKSPACE_ID is unset, so
// the values come solely from the saved login file; stray RIFT_TOKEN/RIFT_API_URL
// env vars are ignored. The RIFT_WORKSPACE_ID read itself is unconditional,
// since its presence is the machine-mode discriminator.
func FromEnvOrFile() (*Config, error) {
	c, err := Load()
	if err != nil {
		return nil, err
	}
	c.MachineWorkspaceID = os.Getenv("RIFT_WORKSPACE_ID")
	if c.MachineWorkspaceID != "" {
		if v := os.Getenv("RIFT_API_URL"); v != "" {
			c.APIBaseURL = v
		}
		if v := os.Getenv("RIFT_TOKEN"); v != "" {
			c.Token = v
		}
	}
	return c, nil
}

func (c *Config) Validate() error {
	// Name the env in the message when non-prod. EnvName is only for the name;
	// a bad env has already errored via Load() before Validate runs, so the
	// error here is unreachable and ignored. Prod → suffix "" → today's wording.
	suffix := ""
	if env, _ := EnvName(); env != "prod" {
		suffix = fmt.Sprintf(" (RIFT_ENV=%s)", env)
	}
	if c.APIBaseURL == "" {
		return fmt.Errorf("no API URL configured%s — run `rift login` (or set RIFT_API_URL)", suffix)
	}
	if c.Token == "" {
		return fmt.Errorf("not logged in%s — run `rift login`", suffix)
	}
	return nil
}
