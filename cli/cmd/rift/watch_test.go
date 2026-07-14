package main

import (
	"context"
	"strings"
	"testing"
)

// --- watch / unwatch usage guards ---
//
// Seam note (same as image_test.go): a routed watch/unwatch/watched handler
// calls the package-level authedClient() (reads config from disk/env) and
// resolveRepo() (shells `git`) before touching the HTTP client — there is no
// client-injection seam at this layer. What IS deterministically testable is
// the arg/usage guards that resolve BEFORE authedClient(), and that a
// well-formed invocation ROUTES past those guards into the handler (failing
// downstream at authedClient with the no-login config error).

// TestCmdWatchRequiresRef proves `watch`/`unwatch` with no <ref> is a usage
// error raised by the NArg guard, before authedClient() — so it surfaces
// regardless of login/git state.
func TestCmdWatchRequiresRef(t *testing.T) {
	err := cmdWatch(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "usage: rift watch <ref>") {
		t.Fatalf("`watch` with no ref must be a usage error, got %v", err)
	}
	err = cmdUnwatch(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "usage: rift unwatch <ref>") {
		t.Fatalf("`unwatch` with no ref must be a usage error, got %v", err)
	}
}

// TestCmdWatchRoutesToHandler proves watch/unwatch WITH a ref, and watched
// (which takes no positional arg), each route past the arg guards into their
// handler — failing at authedClient() with the no-login config error (distinct
// from a usage error). hermeticNoLogin (image_test.go) points config lookup at
// an empty dir so authedClient() fails deterministically.
func TestCmdWatchRoutesToHandler(t *testing.T) {
	hermeticNoLogin(t)
	cases := []struct {
		name string
		run  func(context.Context, []string) error
		args []string
	}{
		{"watch", cmdWatch, []string{"refs/heads/main"}},
		{"unwatch", cmdUnwatch, []string{"refs/heads/main"}},
		{"watched", cmdWatched, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.run(context.Background(), c.args)
			if err == nil {
				t.Fatalf("%s: expected a downstream (config) error, got nil", c.name)
			}
			if strings.Contains(err.Error(), "usage:") {
				t.Fatalf("%s: hit a usage guard instead of routing to the handler: %v", c.name, err)
			}
			// The handler was reached: it failed at authedClient() (no API URL).
			if !strings.Contains(err.Error(), "API URL") {
				t.Fatalf("%s: want the no-login config error, got %v", c.name, err)
			}
		})
	}
}
