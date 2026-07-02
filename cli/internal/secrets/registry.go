// Package secrets implements the devbox host→box secret push: a repo declares
// destination targets in .rift/secrets.json (an UNTRUSTED manifest — it may
// name dests, never sources), the user maps namespaced keys to sources in
// ~/.config/rift/secrets.json, and Reconcile pushes the changed ones over the
// box's SSH exec channel (no control-plane involvement).
package secrets

import (
	"fmt"
	"strings"
)

// Namespace says who owns a key's meaning and which config layer supplies its
// default source: std (the tool's built-in registry), org (the organization),
// local (this repo).
type Namespace string

const (
	NSStd   Namespace = "std"
	NSOrg   Namespace = "org"
	NSLocal Namespace = "local"
)

// Strategy is how a secret reaches the box.
type Strategy string

const (
	StrategyCopy    Strategy = "copy"    // write bytes to a dest file on the box
	StrategyForward Strategy = "forward" // ssh agent forwarding; no bytes copied
	// StrategyEnv writes the value to a box-resident ~/.config/rift/env.d/<NAME>
	// file the base image sources into every shell. DEPRECATED for std: keys (it
	// leaves the value resident on the box) — superseded by StrategyInject. It is
	// retained only for user-defined local:/org: env secrets, which still rely on
	// it.
	StrategyEnv Strategy = "env"
	// StrategyInject delivers env var(s) into the child of an explicit `devbox run`
	// (the broker, owned by another component) — never written to the box, never in
	// the agent's ambient environment. The push reconcile SKIPS inject entries
	// entirely; they carry an Extract describing how source bytes map to the named
	// Env values.
	StrategyInject Strategy = "inject"
)

// Extract says how an inject secret's opaque source bytes map onto its named Env
// values. Only meaningful for StrategyInject.
type Extract string

const (
	// ExtractPassthrough — the whole source (trailing whitespace trimmed) is one
	// value → the single Env[0]. Covers npm, the Claude token, any single token.
	ExtractPassthrough Extract = "passthrough"
	// ExtractAWSCredsFile — parse the standard AWS credentials INI ([default]
	// section) → AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN.
	ExtractAWSCredsFile Extract = "aws-creds-file"
)

// Key is a parsed namespace:name.
type Key struct {
	NS   Namespace
	Name string
}

func (k Key) String() string { return string(k.NS) + ":" + k.Name }

// ParseKey parses "ns:name" (default ns=local) and validates the name charset.
func ParseKey(s string) (Key, error) {
	ns := NSLocal
	name := s
	if i := strings.IndexByte(s, ':'); i >= 0 {
		switch Namespace(s[:i]) {
		case NSStd:
			ns = NSStd
		case NSOrg:
			ns = NSOrg
		case NSLocal:
			ns = NSLocal
		default:
			return Key{}, fmt.Errorf("unknown key namespace in %q (want std:/org:/local:)", s)
		}
		name = s[i+1:]
	}
	if !validName(name) {
		return Key{}, fmt.Errorf("invalid key name in %q (want [a-z0-9][a-z0-9-]*)", s)
	}
	return Key{NS: ns, Name: name}, nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// stdEntry is a built-in std: key definition. A copy entry carries a
// conventional dest+mode; an env/inject entry carries the variable name(s) (an
// env entry's dest is derived — see resolve.go); a forward entry carries
// neither. An inject entry additionally carries an Extract (how its source bytes
// map onto the named Env values) and has no Dest/Mode (it is never pushed).
type stdEntry struct {
	Dest     string
	Mode     string
	Env      []string // StrategyEnv/StrategyInject: the environment variable name(s)
	Strategy Strategy
	Extract  Extract // StrategyInject only: how source bytes map to the Env values
}

// stdRegistry is the curated set of std: keys the tool owns the meaning of.
// This is what makes a `{"key":"std:aws"}` manifest entry zero-config: the dest,
// mode, and strategy come from here, the source from the user's global defaults.
var stdRegistry = map[string]stdEntry{
	// aws: BROKERED (inject). The source is the standard AWS credentials INI; the
	// aws-creds-file extractor splits it into the three conventional env vars the
	// `aws` CLI reads. No box-resident ~/.aws/credentials.
	"aws": {
		Strategy: StrategyInject,
		Env:      []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"},
		Extract:  ExtractAWSCredsFile,
	},
	"gcp": {Dest: "~/.config/gcloud/application_default_credentials.json", Mode: "0600", Strategy: StrategyCopy},
	// npm: BROKERED (inject). The whole source is the token → NPM_TOKEN. No
	// box-resident ~/.npmrc.
	"npm":   {Strategy: StrategyInject, Env: []string{"NPM_TOKEN"}, Extract: ExtractPassthrough},
	"netrc": {Dest: "~/.netrc", Mode: "0600", Strategy: StrategyCopy},
	// gh reads its token from ~/.config/gh/hosts.yml (the `oauth_token` field).
	// Copying it authorizes `gh` on the box; with `gh auth setup-git`, git-over-HTTPS
	// too. Maps cleanly when the host stores the token in the file; if the host keeps
	// it in a keyring (hosts.yml has no oauth_token), synthesize one from `gh auth
	// token`. Today it stays `copy` because
	// git-over-HTTPS still wants the file (an operator call, deferred).
	"github": {Dest: "~/.config/gh/hosts.yml", Mode: "0600", Strategy: StrategyCopy},
	// claude: BROKERED (inject) — was `env`. Map this to a file holding a `claude
	// setup-token` OAuth token; the broker injects it as CLAUDE_CODE_OAUTH_TOKEN
	// into the child of an explicit `devbox run`. It moves off the resident `env`
	// strategy (which left the value in every shell's environment + an env.d file a
	// careless agent could read) onto inject, so it is never resident on the box.
	// The old ~/.claude/.credentials.json copy raced across machines (short-lived
	// access token + single-use refresh token) and self-clobbered its symlink.
	"claude": {Strategy: StrategyInject, Env: []string{"CLAUDE_CODE_OAUTH_TOKEN"}, Extract: ExtractPassthrough},
	"ssh":    {Strategy: StrategyForward},
	"gpg":    {Strategy: StrategyForward},
	// ssh-key: OPT-IN private-key PUSH. Unlike std:ssh
	// (forward-only — no key on box), this COPIES the mapped private key into the
	// box tmpfs so a FULLY-DETACHED batch job can authenticate with no live agent.
	// It trades the no-key-on-box property for detached availability, so it is
	// never a default: a manifest must explicitly declare std:ssh-key, and the
	// user must explicitly map it to a key file. tmpfs-backed like every pushed
	// secret, so it evaporates on stop (not persisted to the volume).
	"ssh-key": {Dest: "~/.ssh/id_rift", Mode: "0600", Strategy: StrategyCopy},
}

// IsStdName reports whether name is a built-in std: key — used by `secrets map`
// to catch a bare `aws` (which parses as local:aws) that almost certainly meant
// std:aws.
func IsStdName(name string) bool {
	_, ok := stdRegistry[name]
	return ok
}
