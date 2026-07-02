package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
)

// --- humanizeAge buckets + the <=0 guard ---
//
// humanizeAge is relative to wall-clock time.Now(), so each input is
// constructed as now-minus-a-duration comfortably INSIDE its bucket (never at
// a 60m/24h threshold, which would flake on call-time drift). The <=0 guard
// uses fixed sentinel epochs (0 and negative) that are bucket-independent.
func TestHumanizeAge(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }

	cases := []struct {
		name string
		in   int64
		want string
	}{
		{"sub-minute", at(30 * time.Second), "<1m"},
		{"minutes", at(5 * time.Minute), "5m"},
		{"hours", at(90 * time.Minute), "1h"},   // 90m → 1h (truncated)
		{"more-hours", at(5 * time.Hour), "5h"}, // mid-bucket, away from 24h
		{"days", at(50 * time.Hour), "2d"},      // 50h → 2d (truncated)
		{"more-days", at(7 * 24 * time.Hour), "7d"},
		{"zero-is-unknown", 0, "?"},
		{"negative-is-unknown", -1, "?"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := humanizeAge(c.in); got != c.want {
				t.Fatalf("humanizeAge(%d) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// --- imageFlags renders the default/pinned FLAGS column (4 combinations) ---
func TestImageFlags(t *testing.T) {
	cases := []struct {
		name     string
		default_ bool
		pinned   bool
		want     string
	}{
		{"neither", false, false, ""},
		{"default-only", true, false, "default"},
		{"pinned-only", false, true, "pinned"},
		{"both", true, true, "default,pinned"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := imageFlags(client.ImageItem{Default: c.default_, Pinned: c.pinned})
			if got != c.want {
				t.Fatalf("imageFlags(default=%v,pinned=%v) = %q, want %q",
					c.default_, c.pinned, got, c.want)
			}
		})
	}
}

// --- cmdImage subcommand dispatch ---
//
// Seam note: a VALID subcommand (ls/pin/unpin) routes into imageLs/imagePin,
// which call the package-level authedClient() (reads config from disk/env) and
// resolveRepo() (shells `git`) before ever touching the HTTP client — there is
// no client-injection seam at this layer (unlike internal/client, whose own
// tests exercise it with an httptest server). So the dispatch's happy path can't be
// driven against a stubbed client/server here. What IS deterministically
// testable at this layer is the dispatch's ROUTING decision and its usage
// guards, which all resolve BEFORE authedClient():
//
//   - unknown subcommand → cmdImage's `default` arm errors immediately.
//   - `image pin`/`unpin` with no <sha> → imagePin's NArg guard errors before
//     authedClient().
//   - a valid subcommand → does NOT hit the unknown-subcommand arm; it routes
//     into the handler and surfaces a DOWNSTREAM error (here: the config error,
//     made deterministic by pointing XDG_CONFIG_HOME at an empty temp dir so no
//     login file is found). Reaching that error proves the `case` arm dispatched
//     rather than falling through to `default`.

// hermeticNoLogin points the config lookup at an empty dir so authedClient()
// fails deterministically with the "no API URL configured" error regardless of
// any real ~/.config/rift/config.json on the host. (os.UserConfigDir honors
// XDG_CONFIG_HOME first, then $HOME/.config — set both to be safe.)
func hermeticNoLogin(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", d)
	t.Setenv("HOME", d)
	// Ensure laptop-mode (no machine-token env override).
	t.Setenv("RIFT_WORKSPACE_ID", "")
}

func TestCmdImageUnknownSubcommand(t *testing.T) {
	err := cmdImage(context.Background(), []string{"frobnicate"})
	if err == nil {
		t.Fatal("unknown subcommand must error")
	}
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("want unknown-subcommand error, got %v", err)
	}
}

func TestCmdImageNoArgsIsUsageError(t *testing.T) {
	err := cmdImage(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("no subcommand must be a usage error, got %v", err)
	}
}

func TestCmdImagePinRequiresSha(t *testing.T) {
	// `image pin` with no <sha>: imagePin's NArg guard fires before authedClient,
	// so the usage error surfaces regardless of login/git state.
	err := cmdImage(context.Background(), []string{"pin"})
	if err == nil || !strings.Contains(err.Error(), "usage: rift image pin <sha>") {
		t.Fatalf("`image pin` with no sha must be a usage error, got %v", err)
	}
	// `image unpin` with no <sha>: same guard, verb flips to "unpin".
	err = cmdImage(context.Background(), []string{"unpin"})
	if err == nil || !strings.Contains(err.Error(), "usage: rift image unpin <sha>") {
		t.Fatalf("`image unpin` with no sha must be a usage error, got %v", err)
	}
}

// TestCmdImageValidSubcommandsRouteToHandler proves ls/pin/unpin each dispatch
// into their handler (not the unknown-subcommand default arm). With no login
// configured, each routed handler fails at authedClient() with the config
// error — distinct from the unknown-subcommand error the default arm would
// raise, and distinct from the NArg usage error (pin/unpin carry a <sha> here
// so they pass the guard and reach authedClient).
func TestCmdImageValidSubcommandsRouteToHandler(t *testing.T) {
	hermeticNoLogin(t)
	for _, args := range [][]string{
		{"ls"},
		{"list"}, // alias also routes to imageLs
		{"pin", "deadbeef"},
		{"unpin", "deadbeef"},
	} {
		err := cmdImage(context.Background(), args)
		if err == nil {
			t.Fatalf("%v: expected a downstream (config) error, got nil", args)
		}
		if strings.Contains(err.Error(), "unknown subcommand") {
			t.Fatalf("%v: routed to the default arm instead of a handler: %v", args, err)
		}
		if strings.Contains(err.Error(), "usage:") {
			t.Fatalf("%v: hit a usage guard instead of routing to the handler: %v", args, err)
		}
		// The handler was reached: it failed at authedClient() (no API URL).
		if !strings.Contains(err.Error(), "API URL") {
			t.Fatalf("%v: want the no-login config error, got %v", args, err)
		}
	}
}
