package secrets

import "testing"

func TestNormalizeRepoID(t *testing.T) {
	cases := []struct{ in, q, b string }{
		{"acme/widget", "github.com/acme/widget", "acme/widget"},
		{"github.com/acme/widget", "github.com/acme/widget", "acme/widget"},
		{"ACME/Widget", "github.com/acme/widget", "acme/widget"},
		{"gitlab.com/acme/widget", "gitlab.com/acme/widget", "acme/widget"},
	}
	for _, c := range cases {
		q, b := normalizeRepoID(c.in)
		if q != c.q || b != c.b {
			t.Errorf("normalizeRepoID(%q) = (%q,%q), want (%q,%q)", c.in, q, b, c.q, c.b)
		}
	}
}

func TestMatchRepo(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		repo     string
		want     string
		ok       bool
	}{
		{"exact qualified beats bare exact", []string{"acme/widget", "github.com/acme/widget"}, "acme/widget", "github.com/acme/widget", true},
		{"bare exact beats qualified owner-glob", []string{"acme/widget", "github.com/acme/*"}, "acme/widget", "acme/widget", true},
		{"qualified owner-glob matches", []string{"github.com/acme/*"}, "acme/widget", "github.com/acme/*", true},
		{"bare owner-glob matches", []string{"acme/*"}, "acme/widget", "acme/*", true},
		{"no match", []string{"other/repo", "github.com/x/*"}, "acme/widget", "", false},
		{"qualified owner-glob beats bare owner-glob", []string{"acme/*", "github.com/acme/*"}, "acme/widget", "github.com/acme/*", true},
		{"bare exact beats wildcard-host exact", []string{"acme/widget", "*/acme/widget"}, "acme/widget", "acme/widget", true},
		{"different forge does not match bare-derived", []string{"gitlab.com/acme/widget"}, "acme/widget", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := matchRepo(c.patterns, c.repo)
			if got != c.want || ok != c.ok {
				t.Errorf("matchRepo(%v, %q) = (%q,%v), want (%q,%v)", c.patterns, c.repo, got, ok, c.want, c.ok)
			}
		})
	}
}
