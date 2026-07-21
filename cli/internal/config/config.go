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
	"regexp"
)

// DefaultAPIBaseURL is the hosted production control plane. `rift login`
// falls back to it when neither --api / RIFT_API_URL nor a previously-saved
// config supplies a URL, so a first-time login on a laptop needs no flag.
const DefaultAPIBaseURL = "https://fixedlabs.dev"

type Config struct {
	APIBaseURL string `json:"api_base_url"`
	Token      string `json:"token"`
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

// envNameRE is the env-name grammar. It is interpolated into a session
// filename (config.<env>.json), so it doubles as a path-safety boundary.
// Keep in sync with the same grammar in fplctl's config package and in
// bin/rift-env.
var envNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// EnvName returns the active session name. Machine mode (rift only): a
// non-empty RIFT_WORKSPACE_ID means in-VM — return "prod" WITHOUT reading
// RIFT_ENV, so a stray var in a box shell can neither redirect nor break
// lifecycle verbs. Otherwise the selector RIFT_ENV chooses the session:
// empty or "prod" resolves to prod (today's behavior, byte-for-byte); any
// other value must match the grammar or this errors.
func EnvName() (string, error) {
	if os.Getenv("RIFT_WORKSPACE_ID") != "" {
		return "prod", nil
	}
	v := os.Getenv("RIFT_ENV")
	if v == "" || v == "prod" {
		return "prod", nil
	}
	if !envNameRE.MatchString(v) {
		return "", fmt.Errorf("invalid RIFT_ENV %q: must match ^[a-z0-9][a-z0-9-]*$", v)
	}
	return v, nil
}

func path() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	env, err := EnvName()
	if err != nil {
		return "", err
	}
	if env == "prod" {
		return filepath.Join(d, "config.json"), nil
	}
	return filepath.Join(d, "config."+env+".json"), nil
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

// Save writes the config 0600 (it holds the bearer). It routes through path()
// so a non-prod session persists to config.<env>.json, not prod's config.json —
// a Load()-only reroute would read the env file while writes clobbered prod.
func (c *Config) Save() error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	p, err := path()
	if err != nil {
		return err
	}
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
