// Package config persists the devbox CLI's local state — the API base URL
// and the developer bearer minted by `devbox login` — under the user's
// config dir (XDG: ~/.config/rift/config.json), 0600 since it holds the
// bearer. On a laptop the API base URL and token come SOLELY from that login
// file (the developer authenticates with `devbox login`'s device flow).
//
// The same static binary is baked into the workspace image, where the in-VM
// `devbox resize`/`keepalive`/`suspend` instead read RIFT_API_URL +
// RIFT_TOKEN + RIFT_WORKSPACE_ID from the machine env the provisioner
// injects. The in-VM token is a MACHINE bearer
// (subject = the workspace-id, a different secret than the developer bearer).
// RIFT_WORKSPACE_ID is the machine-mode discriminator: only when it is
// present does FromEnvOrFile honor the RIFT_TOKEN/RIFT_API_URL env
// overrides, and its presence routes lifecycle verbs through the machine-token
// agent surface rather than the developer /api/workspaces routes. A laptop
// sets none of the three, so its env never overrides the login file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultAPIBaseURL is the hosted production control plane. `rift login`
// falls back to it when neither --api / RIFT_API_URL nor a previously-saved
// config supplies a URL, so a first-time login on a laptop needs no flag.
const DefaultAPIBaseURL = "https://fixedlabs.dev"

type Config struct {
	APIBaseURL string `json:"api_base_url"`
	Token      string `json:"token"`
	// DefaultContext is the acting context form-value ("company:<uuid>" /
	// "personal:<uuid>") sent on `new`; empty until set by
	// `rift set-default-context`. Login no longer seeds it — the CLI session
	// proves identity only, and the default is a per-device create target.
	DefaultContext string `json:"default_context,omitempty"`
	// MachineWorkspaceID is set only in-VM (RIFT_WORKSPACE_ID, injected by
	// the provisioner alongside the machine bearer). Its presence means Token
	// is a machine token whose subject is
	// this workspace, so lifecycle verbs must use the machine-token agent
	// routes and may only act on this workspace. Never persisted.
	MachineWorkspaceID string `json:"-"`
}

func dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "rift"), nil
}

func path() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// Load reads the persisted config; a missing file yields a zero Config (not
// an error — a fresh CLI just isn't logged in yet).
func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &c, nil
}

// Save writes the config 0600 (it holds the bearer).
func (c *Config) Save() error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	p := filepath.Join(d, "config.json")
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// FromEnvOrFile resolves the effective {APIBaseURL, Token}. In machine mode —
// only when RIFT_WORKSPACE_ID is set, i.e. genuinely in-VM — the env
// (RIFT_API_URL / RIFT_TOKEN, injected by the provisioner alongside it)
// wins, so an autonomous agent in the box runs lifecycle verbs with the
// machine credentials and no login. On a laptop RIFT_WORKSPACE_ID is unset,
// so the API base and token come solely from the saved `devbox login` file;
// stray RIFT_TOKEN/RIFT_API_URL env vars are ignored (the interim
// developer override was removed once device-flow login shipped). The
// RIFT_WORKSPACE_ID read itself is unconditional, since its presence is the
// machine-mode discriminator.
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
	if c.APIBaseURL == "" {
		return fmt.Errorf("no API URL configured — run `rift login` (or set RIFT_API_URL)")
	}
	if c.Token == "" {
		return fmt.Errorf("not logged in — run `rift login`")
	}
	return nil
}
