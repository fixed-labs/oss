// Package repoid resolves a raw repo input to the canonical forge-qualified
// identity the whole pipeline keys on.
//
// A repo is identified by the canonical forge-qualified string
// "<forge>:<host>/<owner>/<name>" (e.g. "github:github.com/acme/widget") — the
// identical grammar the server validates at ingress. The checked-in fixtures
// (testdata/canonical-repo-vectors.json, shared with the server's Clojure
// tests) are the executable contract. Only forge "github" on host github.com
// is serviced this phase; everything else is rejected, never guessed.
package repoid

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// --- Repo identity: canonical grammar + offline forge resolution (flow 1) ---

// forgeEnum is the closed set of forge-type tokens the grammar recognizes
// (pinned identically in the server implementation). Membership drives the
// already-canonical (form-2) input classification; it does NOT mean the forge
// is serviceable — only "github" validates this phase.
var forgeEnum = map[string]bool{
	"github":       true,
	"gitlab":       true,
	"bitbucket":    true,
	"gitea":        true,
	"forgejo":      true,
	"azure-devops": true,
	"sourcehut":    true,
}

// saasForges is the closed built-in table of well-known SaaS hosts → forge
// type: one of the two explicit forge sources (the other is --forge). An
// unrecognized host is an error, never a guess. github.com only this phase.
var saasForges = map[string]string{"github.com": "github"}

// implementedForges is the set --forge accepts this phase: only "github" is
// serviced end to end. Distinct from forgeEnum (grammar-recognized tokens) —
// an in-enum-but-unimplemented --forge (e.g. gitlab) is rejected with the
// unknown/unsupported-forge error, never passed through to canonicalization
// (which would mis-report a shape problem the input doesn't have).
var implementedForges = map[string]bool{"github": true}

// defaultRepoHost is the host a host-less bare "owner/name" pair resolves to.
const defaultRepoHost = "github.com"

// ErrRepoInvalid is the pinned rejection for any input CanonicalRepo cannot
// parse or validate.
var ErrRepoInvalid = errors.New("invalid repo — use owner/repo or the full forge:host/owner/repo form")

// GitHub segment character rules, applied after lowercasing: owner has no
// leading/trailing/consecutive hyphens and is ≤39 chars; name is ≤100 chars of
// [a-z0-9._-] and not "." or "..".
var (
	githubOwnerRe = regexp.MustCompile(`^[a-z0-9](-?[a-z0-9])*$`)
	githubNameRe  = regexp.MustCompile(`^[a-z0-9._-]+$`)
)

// IsForgeToken reports whether tok is a grammar-recognized forge-type token
// (the closed forgeEnum). The secrets seam uses it to detect and strip a
// "<forge>:" prefix without exposing the table.
func IsForgeToken(tok string) bool {
	return forgeEnum[strings.ToLower(tok)]
}

// decomposeRepo classifies a raw repo input into one of the grammar's four
// forms and splits it into a host authority and a repo path. The order is
// load-bearing (the forms collide on ':'):
//
//  1. contains "://"             → URL: authority up to the first '/', path after.
//  2. leading "<forge-enum>:"    → already-canonical: strip the prefix (the
//     caller's resolved forge is authoritative); authority up to the first '/'.
//  3. a ':' before the first '/' → scp "[user@]host:path" — the colon is a path
//     separator, scp carries NO port ("git@host:2021/repo" is path "2021/repo").
//  4. else                       → bare "owner/name" pair (no host).
//
// hasHost is false only for form 4.
func decomposeRepo(in string) (authority string, hasHost bool, path string) {
	// splitAuthority: authority up to the first '/', path after it.
	splitAuthority := func(rest string) (string, bool, string) {
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[:j], true, rest[j+1:]
		}
		return rest, true, ""
	}
	if i := strings.Index(in, "://"); i >= 0 {
		return splitAuthority(in[i+3:])
	}
	if i := strings.IndexByte(in, ':'); i >= 0 && forgeEnum[strings.ToLower(in[:i])] {
		return splitAuthority(in[i+1:])
	}
	ci := strings.IndexByte(in, ':')
	si := strings.IndexByte(in, '/')
	if ci >= 0 && (si < 0 || ci < si) {
		return in[:ci], true, in[ci+1:]
	}
	return "", false, in
}

// canonicalHost normalizes a host authority — the single host canonicalizer
// used at every parse/compare site: strip an embedded "user[:pass]@",
// lowercase, drop a trailing "/", drop a ":443".
func canonicalHost(authority string) string {
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		authority = authority[i+1:]
	}
	h := strings.ToLower(authority)
	h = strings.TrimSuffix(h, "/")
	h = strings.TrimSuffix(h, ":443")
	return h
}

// CanonicalRepo normalizes a repo input (URL, scp, bare pair, or an
// already-canonical string) to the canonical "<forge>:<host>/<owner>/<name>"
// the whole pipeline keys on. forge is the pre-resolved forge type (see
// ResolveIdentity); defaultHost applies only when the input carries no
// host (the bare-pair case). Idempotent on canonical "github" input. Only
// forge "github" on host github.com validates this phase — anything else is
// ErrRepoInvalid.
func CanonicalRepo(input, forge, defaultHost string) (string, error) {
	authority, hasHost, path := decomposeRepo(strings.TrimSpace(input))
	host := canonicalHost(defaultHost)
	if hasHost {
		host = canonicalHost(authority)
	}
	f := strings.ToLower(strings.TrimSpace(forge))
	if f != "github" || host != "github.com" {
		return "", ErrRepoInvalid
	}
	path = strings.TrimRight(path, "/")
	path = strings.TrimSuffix(path, ".git")
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if s == "" {
			return "", ErrRepoInvalid
		}
		segs[i] = strings.ToLower(s)
	}
	if len(segs) != 2 { // github namespace depth is exactly 1 (owner + name)
		return "", ErrRepoInvalid
	}
	owner, name := segs[0], segs[1]
	if len(owner) > 39 || !githubOwnerRe.MatchString(owner) {
		return "", ErrRepoInvalid
	}
	if len(name) > 100 || name == "." || name == ".." || !githubNameRe.MatchString(name) {
		return "", ErrRepoInvalid
	}
	return f + ":" + host + "/" + owner + "/" + name, nil
}

// ResolveIdentity is the CLI's offline flow-1: decompose the input just
// enough to learn the host (a bare pair defaults to defaultRepoHost), resolve
// the forge from exactly one explicit source — (a) the built-in SaaS table,
// (b) an explicit --forge — and then canonicalize. A --forge that conflicts
// with a known SaaS host is an error; an unrecognized host with no --forge is
// an error. Never a guess, never a network call.
func ResolveIdentity(input, forgeFlag string) (string, error) {
	in := strings.TrimSpace(input)
	authority, hasHost, _ := decomposeRepo(in)
	host := defaultRepoHost
	if hasHost {
		host = canonicalHost(authority)
	}
	flag := strings.ToLower(strings.TrimSpace(forgeFlag))
	forge, known := saasForges[host]
	switch {
	case known:
		if flag != "" && flag != forge {
			return "", fmt.Errorf("--forge %s conflicts with %s (a %s host)", flag, host, forge)
		}
	case flag != "" && implementedForges[flag]:
		forge = flag
	default:
		// No forge source, or a --forge this phase doesn't service ("this
		// phase accepts only :github") — same pinned error either way.
		return "", fmt.Errorf("unknown/unsupported forge for host %s — pass --forge or register the instance", host)
	}
	return CanonicalRepo(in, forge, defaultRepoHost)
}

// Resolve returns the canonical repo id: the --repo flag if set, else
// inferred from the cwd git remote — both through the same flow-1 resolution.
func Resolve(flagRepo, forgeFlag string) (string, error) {
	if flagRepo != "" {
		return ResolveIdentity(flagRepo, forgeFlag)
	}
	return inferRepo(forgeFlag)
}

// inferRepo derives the canonical repo from the cwd git remote origin. Any
// remote shape decomposes (https/ssh URL, scp, already-canonical); whether
// the host is serviceable is flow-1's call — e.g. a gitlab.com remote fails
// with the unknown-forge error, not a URL-shape error.
func inferRepo(forgeFlag string) (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("no git remote (run in a repo, or pass --repo): %w", err)
	}
	return ResolveIdentity(strings.TrimSpace(string(out)), forgeFlag)
}
