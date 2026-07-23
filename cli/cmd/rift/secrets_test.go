package main

import (
	"strings"
	"testing"
)

// --- secretsRepoID: the one-seam canonical→secrets-qualified reduction ---
//
// The workspace repo the API returns is the canonical forge-qualified string;
// the secrets layer's grammar (host/owner/name, forge-less) is unchanged. The
// seam drops the "<forge>:" prefix and passes everything else through.
func TestSecretsRepoID(t *testing.T) {
	cases := map[string]string{
		"github:github.com/acme/widget": "github.com/acme/widget",
		// any forge-enum prefix is dropped (the secrets grammar knows no forge)
		"gitlab:gitlab.com/group/proj": "gitlab.com/group/proj",
		// already secrets-qualified / bare forms pass through
		"github.com/acme/widget": "github.com/acme/widget",
		"acme/widget":            "acme/widget",
		// a non-enum prefix is not a forge label — untouched
		"mercurial:github.com/acme/widget": "mercurial:github.com/acme/widget",
		"":                                 "",
	}
	for in, want := range cases {
		if got := secretsRepoID(in); got != want {
			t.Errorf("secretsRepoID(%q) = %q, want %q", in, got, want)
		}
	}
}

// resolveRepoArg resolves an explicit --repo through the same flow-1 +
// canonicalizer as every other ingress, then reduces to the secrets form.
// ONE accept row proves the composition (resolve → canonical → forge-prefix
// strip); flow-1's own accept/reject axes are pinned in
// TestResolveRepoIdentityFlow1, the grammar in the fixture sweep.
func TestResolveRepoArgCanonicalizesAndReduces(t *testing.T) {
	got, err := resolveRepoArg("https://github.com/Acme/Widget.git", "")
	if err != nil {
		t.Fatalf("resolveRepoArg: %v", err)
	}
	if want := "github.com/acme/widget"; got != want {
		t.Fatalf("resolveRepoArg = %q, want %q", got, want)
	}
}

// The rejects this seam newly owns: retired secrets-grammar spellings that
// must fail loudly rather than silently un-match a user's secrets config.
func TestResolveRepoArgRejects(t *testing.T) {
	cases := []struct {
		in      string
		wantErr string
	}{
		// the old bare host/owner/name secrets input form classifies as a bare
		// pair (host defaults to github.com) and fails github depth validation
		{"gitlab.example.com/o/n", "invalid repo"},
		{"owner/repo/extra", "invalid repo"},
	}
	for _, c := range cases {
		got, err := resolveRepoArg(c.in, "")
		if err == nil {
			t.Errorf("resolveRepoArg(%q) = %q, want error", c.in, got)
			continue
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("resolveRepoArg(%q) error = %v, want it to contain %q", c.in, err, c.wantErr)
		}
	}
}
