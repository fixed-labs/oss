// Package diag routes the devbox CLI's background / diagnostic output to a
// rotating, disk-backed logfile instead of the terminal.
//
// Why this exists: `devbox connect` runs a client-side screen compositor
// (internal/compositor) that owns the terminal's alt-screen on os.Stdout; and the
// SAME binary, baked into the workspace image, runs `devbox run` INSIDE the box
// where its stderr IS the session PTY. In both contexts a stray write to
// stdout/stderr from a background goroutine — the presence ping, the attachment
// lease refresh, the relay reattach watcher, the wireguard rebind/logging, the
// reconnect loop — lands on top of whatever full-screen program owns the terminal
// (the compositor, or a TUI like an editor / Claude Code) and corrupts the frame.
// That is the "interleaved garbage" failure mode.
//
// Diagnostics are logs, not UI: they belong in a logfile you can tail after the
// fact, not painted over the screen. Command RESULTS and interactive prompts keep
// writing to os.Stdout / os.Stderr directly — those are synchronous, pre-handoff,
// and the user is meant to see them.
//
// Setup wires log/slog's default logger at the logfile, so callers just use the
// standard library API (slog.Warn(...), slog.Info(...)); there is no bespoke
// logging type to learn.
package diag

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	// maxBytes rotates the logfile once a write would push it past ~2 MiB. With
	// maxBackups this caps devbox's on-disk logs at ~8 MiB total — plenty for
	// post-hoc debugging of a connect session, negligible on disk.
	maxBytes = 2 << 20
	// maxBackups is how many rotated files (rift.log.1 … .N) to retain.
	maxBackups = 3
)

// Setup points slog's default logger at the rotating disk logfile and returns a
// close func (closes the file) plus the resolved path.
//
// It is best-effort: if the logfile can't be opened (read-only home, unwritable
// state dir, …) it leaves slog writing to stderr and returns an empty path.
// Losing the anti-trample property in that rare case is better than silently
// swallowing every diagnostic. Call once, early, from main, and defer the close.
func Setup() (closeFn func() error, path string) {
	path = LogPath()
	rf := newRotatingFile(path, maxBytes, maxBackups)
	if err := rf.probe(); err != nil {
		// Can't open the logfile — keep diagnostics rather than drop them, even at
		// the cost of the occasional stderr line on the screen.
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: levelFromEnv()})))
		return func() error { return nil }, ""
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(rf, &slog.HandlerOptions{Level: levelFromEnv()})))
	return rf.Close, path
}

// LogPath resolves the diagnostic logfile path. An explicit RIFT_LOG_FILE wins
// (used by tests and ops); otherwise it follows the XDG state-dir convention
// (~/.local/state/rift/rift.log on Linux — the standard location for a
// user-level tool's logs). The same logic resolves correctly on a laptop and
// inside a workspace VM (the box user's home), so "both contexts" need no special
// casing.
func LogPath() string {
	if p := os.Getenv("RIFT_LOG_FILE"); p != "" {
		return p
	}
	return filepath.Join(stateDir(), "rift", "rift.log")
}

// stateDir returns the base directory for user-level state, preferring
// $XDG_STATE_HOME, then ~/.local/state, then (degenerate fallbacks) the config
// dir or the temp dir so Setup can always find SOME writable place.
func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state")
	}
	if cfg, err := os.UserConfigDir(); err == nil && cfg != "" {
		return cfg
	}
	return os.TempDir()
}

// levelFromEnv reads RIFT_LOG_LEVEL (debug|info|warn|error), defaulting to info.
// Lets an operator turn up verbosity to debug a connect without a rebuild.
func levelFromEnv() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RIFT_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
