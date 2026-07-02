// Package config loads the devboxes-agent's boot configuration from the
// machine env the provisioner injects (the RIFT_* contract) plus local paths.
//
// The agent is the control-plane liaison inside each workspace: it brings up
// wg0, generates + persists the machine's identity on first boot, runs the
// WG-identity SSH server, reports the workspace is ready, pulls its authorized-peer
// config, and heartbeats with interactive (SSH/session) liveness.
package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	// WorkspaceID is this machine's workspace id — the bearer's subject; the
	// agent may only act as itself.
	WorkspaceID string
	// APIBaseURL is the PUBLIC devboxes-api base (e.g. https://api.example.dev).
	APIBaseURL string
	// Token is the workspace-scoped bearer minted at provision time.
	Token string
	// WgIP is the cluster-owned deterministic overlay address (a ULA /128).
	WgIP string
	// RelayEndpoint is the assigned relay's public IPv4 (the DEFAULT relay for
	// fresh attaches; per-pairing transport arrives via the pulled config).
	RelayEndpoint string
	// StateDir holds first-boot identity (wg keypair, SSH host key). It MUST
	// be an overlay path (NOT under /persist directly is fine — everything is
	// the overlay) so identity survives stop/resume/resize.
	StateDir string
	// ImageCommit is the git commit baked into this workspace image — the
	// agent reports it as resolved_commit when it reports the workspace is
	// ready. Read best-effort
	// from /etc/devboxes/image-commit (which mkDevimage writes when the image
	// carries a repo checkout); "" when the image records no commit. Never a
	// boot error: an image without it is valid.
	ImageCommit string
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// readImageCommit returns the git commit baked into the image, or "" if the
// image records none. Best-effort by design: a missing/unreadable file is the
// normal case for images built without a baked repo and must never fail the
// agent's boot. The path is env-overridable for dev/test (dev/002, unit tests).
func readImageCommit() string {
	b, err := os.ReadFile(env("DEVBOX_IMAGE_COMMIT_FILE", "/etc/devboxes/image-commit"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// FromEnv loads and validates the agent config. WG_IP / RELAY_ENDPOINT are
// optional at load (a workspace can boot before a relay exists; attach just
// isn't possible yet); the id/url/token trio is mandatory — without it the
// agent can never talk home, which must fail loudly at startup.
func FromEnv() (*Config, error) {
	c := &Config{
		WorkspaceID:   os.Getenv("RIFT_WORKSPACE_ID"),
		APIBaseURL:    os.Getenv("RIFT_API_URL"),
		Token:         os.Getenv("RIFT_TOKEN"),
		WgIP:          os.Getenv("RIFT_WG_IP"),
		RelayEndpoint: os.Getenv("RIFT_RELAY_ENDPOINT"),
		StateDir:      env("RIFT_STATE_DIR", "/var/lib/devboxes"),
		ImageCommit:   readImageCommit(),
	}
	if c.WorkspaceID == "" {
		return nil, fmt.Errorf("RIFT_WORKSPACE_ID is required")
	}
	if c.APIBaseURL == "" {
		return nil, fmt.Errorf("RIFT_API_URL is required")
	}
	if c.Token == "" {
		return nil, fmt.Errorf("RIFT_TOKEN is required")
	}
	return c, nil
}
