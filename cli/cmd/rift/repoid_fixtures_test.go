package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fixed-labs/oss/cli/internal/repoid"
)

// TestCanonicalRepoFixtures drives repoid.CanonicalRepo through the checked-in
// vectors shared with the server's Clojure canonicalizer (the executable
// grammar contract). It lives in cmd/rift, NOT internal/repoid: the vectors
// file at cmd/rift/testdata/canonical-repo-vectors.json is the single
// cross-language source of truth, and cmd/rift/BUILD already wires it into the
// pants sandbox (oss/cli:repo-fixtures). A package-relative copy under
// internal/repoid would need its own pants wiring and could drift from the
// Clojure reader — so the resolver moved to internal/repoid but its fixture
// test stays here, exercising the exported repoid.CanonicalRepo.
func TestCanonicalRepoFixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "canonical-repo-vectors.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var rows []struct {
		Input     string  `json:"input"`
		Forge     string  `json:"forge"`
		Host      string  `json:"host"`
		Canonical *string `json:"canonical"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("fixtures file decoded to zero rows")
	}
	for _, row := range rows {
		got, err := repoid.CanonicalRepo(row.Input, row.Forge, row.Host)
		if row.Canonical == nil {
			if err == nil {
				t.Errorf("CanonicalRepo(%q, %q, %q) = %q, want reject", row.Input, row.Forge, row.Host, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("CanonicalRepo(%q, %q, %q): %v, want %q", row.Input, row.Forge, row.Host, err, *row.Canonical)
			continue
		}
		if got != *row.Canonical {
			t.Errorf("CanonicalRepo(%q, %q, %q) = %q, want %q", row.Input, row.Forge, row.Host, got, *row.Canonical)
			continue
		}
		// Fixed point: every canonical string the CLI emits must survive a round
		// trip unchanged — a non-fixed-point output is exactly the string the
		// server ingress would 400.
		again, err := repoid.CanonicalRepo(*row.Canonical, row.Forge, row.Host)
		if err != nil {
			t.Errorf("CanonicalRepo(%q, %q, %q) (fixed point): %v", *row.Canonical, row.Forge, row.Host, err)
			continue
		}
		if again != *row.Canonical {
			t.Errorf("CanonicalRepo(%q, %q, %q) = %q, want the fixed point %q", *row.Canonical, row.Forge, row.Host, again, *row.Canonical)
		}
	}
}
