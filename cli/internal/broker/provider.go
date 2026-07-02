package broker

import (
	"context"
	"time"

	"github.com/fixed-labs/oss/cli/internal/secrets"
)

// CredKind is the shape of a credential's Material. Today these are only ever
// produced from a static source, but the kind is
// carried so the inject path can shape it into env pairs and the materialize path
// can hand back the raw bytes.
type CredKind string

const (
	// KeyTriple — an AWS-style key triple (the aws-creds-file Extract).
	KeyTriple CredKind = "key-triple"
	// BearerToken — a single opaque token (the passthrough Extract).
	BearerToken CredKind = "bearer-token"
	// SecretData — arbitrary bytes with no env shaping (materialize-only).
	SecretData CredKind = "secret-data"
)

// Credential is what a provider's Mint returns. Material is opaque and NEVER
// logged; today it carries both the
// typed env pairs (for inject) and the raw source bytes (for --materialize-to).
// Expiry is the provider's authoritative grant; the static provider does not
// expire on its own (zero time). Revoke is best-effort and a no-op for static.
type Credential struct {
	Kind   CredKind
	Env    []secrets.EnvPair // the typed env pairs (inject delivery)
	Raw    []byte            // the raw source bytes (materialize delivery)
	Expiry time.Time         // zero = no self-expiry (static)
	Revoke func()            // best-effort early revocation; no-op for static
}

// MintRequest is the provider input. Today minting takes only the secret name;
// the scope-down / identity-ref / audience / requested-TTL fields are inert for
// the static provider and exist so the interface is the one future AWS/GitHub
// providers implement unchanged.
type MintRequest struct {
	Secret       string
	IdentityRef  string         // unused by static
	ScopeDown    map[string]any // unused by static (narrowing only, never widening)
	Audience     string         // unused by static
	RequestedTTL time.Duration  // unused by static
}

// Provider produces one kind of credential. It is the only thing that mints.
// Today there is exactly one implementation, the static provider.
type Provider interface {
	Mint(ctx context.Context, req MintRequest) (Credential, error)
}

// StaticProvider is today's sole provider: it resolves a secret to its
// file/cmd: source on the customer side and applies the Extract to produce
// the typed credential. There is nothing to scope down, no real expiry, and
// Revoke is a no-op.
//
// It closes over the loaded user config and the repo dir, which together pick
// the source mapping the same way the connect-time reconcile does.
type StaticProvider struct {
	uc      *secrets.UserConfig
	repoDir string
}

// NewStaticProvider builds the static provider over a loaded user config and the
// repo's canonical id (its directory name).
func NewStaticProvider(uc *secrets.UserConfig, repoDir string) *StaticProvider {
	return &StaticProvider{uc: uc, repoDir: repoDir}
}

// Mint resolves req.Secret to its inject entry, reads the source on the laptop,
// and shapes both the typed env pairs and the raw bytes into a Credential. A
// failure to resolve (unknown / non-inject / unmapped) is returned as an error
// the handler maps to ErrUnknownSecret; a read/extract failure is ErrInternal.
func (p *StaticProvider) Mint(ctx context.Context, req MintRequest) (Credential, error) {
	r, err := secrets.ResolveInjectSecret(p.uc, p.repoDir, req.Secret)
	if err != nil {
		return Credential{}, &resolveError{err}
	}
	raw, err := secrets.ReadSource(ctx, r.Source)
	if err != nil {
		return Credential{}, err
	}
	pairs, err := secrets.ExtractCredential(r.Extract, r.EnvNames, raw)
	if err != nil {
		return Credential{}, err
	}
	kind := BearerToken
	if r.Extract == secrets.ExtractAWSCredsFile {
		kind = KeyTriple
	}
	return Credential{
		Kind:   kind,
		Env:    pairs,
		Raw:    raw,
		Revoke: func() {}, // static: no-op
	}, nil
}

// resolveError marks a Mint failure that originates from ResolveInjectSecret —
// an unknown, non-inject, or unmapped secret — so the handler returns
// ErrUnknownSecret rather than ErrInternal. The wrapped error's message is
// value-free (it names only the secret), so it is safe to surface as Detail.
type resolveError struct{ err error }

func (e *resolveError) Error() string { return e.err.Error() }
func (e *resolveError) Unwrap() error { return e.err }
