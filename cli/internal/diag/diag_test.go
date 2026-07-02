package diag

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogPathHonorsOverride(t *testing.T) {
	t.Setenv("RIFT_LOG_FILE", "/tmp/custom/rift.log")
	if got := LogPath(); got != "/tmp/custom/rift.log" {
		t.Fatalf("LogPath = %q, want the RIFT_LOG_FILE override", got)
	}
}

func TestLogPathUsesXDGStateHome(t *testing.T) {
	t.Setenv("RIFT_LOG_FILE", "")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	want := filepath.Join("/xdg/state", "rift", "rift.log")
	if got := LogPath(); got != want {
		t.Fatalf("LogPath = %q, want %q", got, want)
	}
}

func TestSetupWritesSlogToFile(t *testing.T) {
	// Setup mutates the global slog default; restore it afterwards.
	saved := slog.Default()
	defer slog.SetDefault(saved)

	p := filepath.Join(t.TempDir(), "rift.log")
	t.Setenv("RIFT_LOG_FILE", p)

	closeFn, got := Setup()
	if got != p {
		t.Fatalf("Setup path = %q, want %q", got, p)
	}
	slog.Warn("presence ping failed; will retry", "err", "boom")
	if err := closeFn(); err != nil {
		t.Fatalf("close: %v", err)
	}

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "presence ping failed") || !strings.Contains(s, "err=boom") {
		t.Fatalf("log missing the diagnostic; got:\n%s", s)
	}
	if !strings.Contains(s, "level=WARN") {
		t.Fatalf("log missing level; got:\n%s", s)
	}
}

func TestSetupFallsBackToStderrOnUnwritablePath(t *testing.T) {
	saved := slog.Default()
	defer slog.SetDefault(saved)

	// A path whose parent is a regular FILE can't be created as a directory →
	// open fails → Setup returns an empty path and keeps logging (to stderr).
	bad := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIFT_LOG_FILE", filepath.Join(bad, "rift.log"))

	closeFn, got := Setup()
	defer closeFn()
	if got != "" {
		t.Fatalf("Setup path = %q, want empty (fallback)", got)
	}
}
