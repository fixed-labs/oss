package config

import (
	"os"
	"path/filepath"
	"testing"
)

func setRequired(t *testing.T) {
	t.Setenv("RIFT_WORKSPACE_ID", "ws-1")
	t.Setenv("RIFT_API_URL", "https://api.example.dev")
	t.Setenv("RIFT_TOKEN", "tok")
}

func TestFromEnvHappyPath(t *testing.T) {
	setRequired(t)
	t.Setenv("RIFT_WG_IP", "fd5e:de7b::1")
	t.Setenv("RIFT_RELAY_ENDPOINT", "1.2.3.4")
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.WorkspaceID != "ws-1" || c.APIBaseURL != "https://api.example.dev" ||
		c.Token != "tok" || c.WgIP != "fd5e:de7b::1" || c.RelayEndpoint != "1.2.3.4" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if c.StateDir != "/var/lib/devboxes" {
		t.Fatalf("default StateDir: %q", c.StateDir)
	}
}

func TestFromEnvRequiresIdentityTrio(t *testing.T) {
	for _, missing := range []string{"RIFT_WORKSPACE_ID", "RIFT_API_URL", "RIFT_TOKEN"} {
		t.Run(missing, func(t *testing.T) {
			setRequired(t)
			t.Setenv(missing, "")
			if _, err := FromEnv(); err == nil {
				t.Fatalf("expected error when %s missing", missing)
			}
		})
	}
}

func TestWgFieldsOptional(t *testing.T) {
	setRequired(t)
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.WgIP != "" || c.RelayEndpoint != "" {
		t.Fatalf("expected blank optional fields: %+v", c)
	}
}

func TestImageCommitFromFile(t *testing.T) {
	setRequired(t)
	f := filepath.Join(t.TempDir(), "image-commit")
	// Trailing newline is the natural shape of a baked file — it must be trimmed.
	if err := os.WriteFile(f, []byte("abc123def\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVBOX_IMAGE_COMMIT_FILE", f)
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.ImageCommit != "abc123def" {
		t.Fatalf("ImageCommit = %q, want abc123def", c.ImageCommit)
	}
}

func TestImageCommitMissingIsBlank(t *testing.T) {
	setRequired(t)
	// An image with no baked commit: a missing file must yield "" and no error.
	t.Setenv("DEVBOX_IMAGE_COMMIT_FILE", filepath.Join(t.TempDir(), "absent"))
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.ImageCommit != "" {
		t.Fatalf("ImageCommit = %q, want empty", c.ImageCommit)
	}
}
