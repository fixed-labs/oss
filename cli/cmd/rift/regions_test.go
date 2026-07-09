package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/config"
)

// regionsHandler answers GET /api/regions with the given result shape (the
// effective/pinned *string defaults + the selectable rows). It mirrors the
// server's read-surface JSON so cmdRegions can be driven end-to-end through
// authedClient against a served response.
func regionsHandler(effective, pinned *string, rows []client.RegionItem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/regions" {
			http.NotFound(w, r)
			return
		}
		body := map[string]any{
			"effective_default": effective,
			"pinned_default":    pinned,
			"regions":           rows,
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}

func strptr(s string) *string { return &s }

// --- cmdRegions rendering (driven end-to-end via the served catalog) ---

// The effective_default row is marked with a trailing "*" and echoed on the
// closing "default: <slug>" line; other rows are unmarked.
func TestCmdRegionsMarksEffectiveDefault(t *testing.T) {
	rows := []client.RegionItem{
		{Slug: "us-east", DisplayName: "US East", Status: "available", AvailableNow: true},
		{Slug: "eu-west", DisplayName: "EU West", Status: "available", AvailableNow: false},
	}
	srv := hermeticEnv(t, regionsHandler(strptr("us-east"), nil, rows))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdRegions(context.Background(), nil); err != nil {
			t.Fatalf("cmdRegions: %v", err)
		}
	})
	if !strings.Contains(out, "us-east*") {
		t.Fatalf("effective default row must be marked with '*', got:\n%s", out)
	}
	if strings.Contains(out, "eu-west*") {
		t.Fatalf("non-default row must NOT be marked, got:\n%s", out)
	}
	if !strings.Contains(out, "default: us-east") {
		t.Fatalf("closing default line missing, got:\n%s", out)
	}
	// the AvailableNow signal renders as yes/no.
	if !strings.Contains(out, "yes") || !strings.Contains(out, "no") {
		t.Fatalf("available column must render yes/no, got:\n%s", out)
	}
}

// A pinned_default that differs from the effective default AND whose row is
// deprecated prints the migrate hint (the stale-pin signal).
func TestCmdRegionsPrintsMigrateHintForDeprecatedPin(t *testing.T) {
	rows := []client.RegionItem{
		{Slug: "us-east", DisplayName: "US East", Status: "available", AvailableNow: true},
		{Slug: "eu-west", DisplayName: "EU West", Status: "deprecated", AvailableNow: false},
	}
	// effective=us-east, pinned=eu-west (deprecated, differs) → hint.
	srv := hermeticEnv(t, regionsHandler(strptr("us-east"), strptr("eu-west"), rows))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdRegions(context.Background(), nil); err != nil {
			t.Fatalf("cmdRegions: %v", err)
		}
	})
	if !strings.Contains(out, "deprecated") {
		t.Fatalf("expected the deprecated status to render, got:\n%s", out)
	}
	if !strings.Contains(out, "eu-west") || !strings.Contains(out, "is deprecated") {
		t.Fatalf("expected the migrate hint naming the stale pin, got:\n%s", out)
	}
	if !strings.Contains(out, "set-default-region") {
		t.Fatalf("migrate hint must point at `rift set-default-region`, got:\n%s", out)
	}
}

// No migrate hint when the pinned default equals the effective default (nothing
// stale), even though a deprecated row exists in the catalog.
func TestCmdRegionsNoHintWhenPinIsEffective(t *testing.T) {
	rows := []client.RegionItem{
		{Slug: "us-east", DisplayName: "US East", Status: "available", AvailableNow: true},
		{Slug: "eu-west", DisplayName: "EU West", Status: "deprecated", AvailableNow: false},
	}
	// pinned == effective == us-east → no hint (the pin is current).
	srv := hermeticEnv(t, regionsHandler(strptr("us-east"), strptr("us-east"), rows))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdRegions(context.Background(), nil); err != nil {
			t.Fatalf("cmdRegions: %v", err)
		}
	})
	if strings.Contains(out, "is deprecated") {
		t.Fatalf("no migrate hint expected when pin == effective, got:\n%s", out)
	}
}

// No migrate hint when the differing pin is NOT deprecated (still-current pin;
// a divergence alone must not trip the hint — only a deprecated pin does).
func TestCmdRegionsNoHintWhenDifferingPinNotDeprecated(t *testing.T) {
	rows := []client.RegionItem{
		{Slug: "us-east", DisplayName: "US East", Status: "available", AvailableNow: true},
		{Slug: "eu-west", DisplayName: "EU West", Status: "available", AvailableNow: true},
	}
	// pinned=eu-west differs from effective=us-east, but eu-west is available.
	srv := hermeticEnv(t, regionsHandler(strptr("us-east"), strptr("eu-west"), rows))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdRegions(context.Background(), nil); err != nil {
			t.Fatalf("cmdRegions: %v", err)
		}
	})
	if strings.Contains(out, "is deprecated") {
		t.Fatalf("a differing but non-deprecated pin must NOT trip the hint, got:\n%s", out)
	}
}

// The empty-list path prints a "No regions available." message and no table.
func TestCmdRegionsEmptyList(t *testing.T) {
	srv := hermeticEnv(t, regionsHandler(nil, nil, []client.RegionItem{}))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdRegions(context.Background(), nil); err != nil {
			t.Fatalf("cmdRegions: %v", err)
		}
	})
	if !strings.Contains(out, "No regions available.") {
		t.Fatalf("empty catalog must print the no-regions message, got:\n%s", out)
	}
	if strings.Contains(out, "SLUG") {
		t.Fatalf("empty catalog must not render the table header, got:\n%s", out)
	}
}

// --- describeRegion pure-helper mapping (the `new` resolved-region echo) ---
//
// describeRegion maps the server's (region, source) echo to the friendly
// "Using region <slug> (<how>)" line: explicit → "you chose it", user →
// "your default", org → "your org default", system → "the system default";
// an unknown source falls through verbatim; an empty region → no output.
func TestDescribeRegion(t *testing.T) {
	cases := []struct {
		name string
		in   client.CreateResult
		want string
	}{
		{
			name: "empty-region-no-output",
			in:   client.CreateResult{Region: "", Source: "explicit"},
			want: "",
		},
		{
			name: "explicit",
			in:   client.CreateResult{Region: "us-east", Source: "explicit"},
			want: "Using region us-east (you chose it)",
		},
		{
			name: "user",
			in:   client.CreateResult{Region: "us-east", Source: "user"},
			want: "Using region us-east (your default)",
		},
		{
			name: "org",
			in:   client.CreateResult{Region: "us-east", Source: "org"},
			want: "Using region us-east (your org default)",
		},
		{
			name: "system",
			in:   client.CreateResult{Region: "us-east", Source: "system"},
			want: "Using region us-east (the system default)",
		},
		{
			name: "unknown-source-falls-through",
			in:   client.CreateResult{Region: "us-east", Source: "weird"},
			want: "Using region us-east (weird)",
		},
		{
			name: "empty-source-no-parenthetical",
			in:   client.CreateResult{Region: "us-east", Source: ""},
			want: "Using region us-east",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.in
			if got := describeRegion(&r); got != c.want {
				t.Fatalf("describeRegion(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
