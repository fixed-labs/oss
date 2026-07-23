package repoid

import (
	"strings"
	"testing"
)

// The fixtures-driven test (TestCanonicalRepoFixtures) lives in cmd/rift, where
// the shared cross-language vectors file is already pants-wired
// (oss/cli:repo-fixtures) — see cmd/rift/repoid_fixtures_test.go. The tests
// below are self-contained (no testdata file), so they run under both `go test`
// and the pants sandbox.

// The rejection message is pinned by the design (the CLI surfaces every
// canonicalizer reject with this exact wording).
func TestCanonicalRepoRejectionMessage(t *testing.T) {
	_, err := CanonicalRepo("widget", "github", "github.com")
	want := "invalid repo — use owner/repo or the full forge:host/owner/repo form"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

// --- flow-1 forge/host resolution (offline; INV-5) ---
//
// The forge comes from exactly one explicit source: (a) the built-in SaaS
// table (github.com only this phase) — a conflicting --forge is an error;
// (b) an explicit --forge; else (c) an error naming the host. Never a guess.
func TestResolveRepoIdentityFlow1(t *testing.T) {
	canonical := "github:github.com/acme/widget"
	cases := []struct {
		name    string
		in      string
		forge   string
		want    string // "" → expect error
		wantErr string // substring the error must carry
	}{
		// (a) SaaS-table hits, across decomposition forms, no --forge needed.
		{"saas-bare", "Acme/Widget", "", canonical, ""},
		{"saas-url", "https://github.com/Acme/Widget.git", "", canonical, ""},
		{"saas-scp", "git@github.com:Acme/Widget.git", "", canonical, ""},
		{"saas-already-canonical", "github:github.com/acme/widget", "", canonical, ""},
		// Case-skewed HOSTS still hit the SaaS table — the lookup happens on
		// the canonicalized host, not the raw authority.
		{"saas-scp-case-skewed-host", "git@GitHub.com:Acme/Widget.git", "", canonical, ""},
		{"saas-url-upper-host", "https://GITHUB.COM/Org/Name", "", "github:github.com/org/name", ""},
		// --forge agreeing with the SaaS table is not a conflict.
		{"forge-agrees", "acme/widget", "github", canonical, ""},
		{"forge-agrees-case", "acme/widget", "GitHub", canonical, ""},
		// (a) beats (b): a --forge conflicting with a known SaaS host errors.
		{"forge-conflict-url", "https://github.com/acme/widget", "gitlab", "", "conflicts"},
		{"forge-conflict-bare", "acme/widget", "gitlab", "", "conflicts"},
		// (c) unknown host, no --forge → the pinned register-the-instance error.
		{"unknown-host", "https://git.corp.net/o/r", "",
			"", "unknown/unsupported forge for host git.corp.net — pass --forge or register the instance"},
		{"unknown-host-gitlab-saas", "git@gitlab.com:group/proj", "",
			"", "unknown/unsupported forge for host gitlab.com"},
		// (b) --forge github on a non-github.com host resolves the forge but
		// still errors in CanonicalRepo (GHES is deferred).
		{"forge-github-nongithub-host", "https://ghes.corp.net/acme/widget", "github", "", "invalid repo"},
		// (b) an unsupported --forge ("this phase accepts only :github")
		// gets the pinned register-the-instance error, not a misleading
		// repo-shape error — in-enum and out-of-enum values alike.
		{"forge-unsupported-inenum", "https://gitlab.corp.net/g/p", "gitlab",
			"", "unknown/unsupported forge for host gitlab.corp.net — pass --forge or register the instance"},
		{"forge-unsupported-bogus", "https://git.corp.net/o/r", "bogus",
			"", "unknown/unsupported forge for host git.corp.net — pass --forge or register the instance"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveIdentity(c.in, c.forge)
			if c.want == "" {
				if err == nil {
					t.Fatalf("ResolveIdentity(%q, %q) = %q, want error", c.in, c.forge, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveIdentity(%q, %q): %v", c.in, c.forge, err)
			}
			if got != c.want {
				t.Fatalf("ResolveIdentity(%q, %q) = %q, want %q", c.in, c.forge, got, c.want)
			}
		})
	}
}
