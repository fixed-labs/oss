package secrets

import "testing"

func ucWith(defaults map[string]Source, repos map[string]RepoEntry) *UserConfig {
	if defaults == nil {
		defaults = map[string]Source{}
	}
	if repos == nil {
		repos = map[string]RepoEntry{}
	}
	return &UserConfig{Defaults: defaults, Repos: repos, Trusted: map[string][]string{}}
}

func manifest(secs ...RepoSecret) *RepoManifest { return &RepoManifest{Secrets: secs} }

// std:aws is now a BROKERED (inject) secret: the source is the AWS credentials
// INI, the aws-creds-file extractor splits it into the three conventional env
// vars, and NOTHING is written to the box (no dest/mode).
func TestResolveStdAwsInject(t *testing.T) {
	uc := ucWith(map[string]Source{"aws": {Path: "/home/u/.aws/credentials"}}, nil)
	r, unmapped, errs := Resolve(manifest(RepoSecret{Key: "std:aws"}), uc, "acme/widget")
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if len(unmapped) != 0 {
		t.Fatalf("unexpected unmapped: %v", unmapped)
	}
	if len(r) != 1 {
		t.Fatalf("want 1 resolved, got %d", len(r))
	}
	got := r[0]
	if got.Strategy != StrategyInject || got.Extract != ExtractAWSCredsFile {
		t.Errorf("std:aws should be inject/aws-creds-file: %+v", got)
	}
	if got.Dest != "" || got.Mode != "" {
		t.Errorf("std:aws inject must carry no dest/mode (never written to the box): %+v", got)
	}
	want := []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}
	if len(got.EnvNames) != 3 || got.EnvNames[0] != want[0] || got.EnvNames[1] != want[1] || got.EnvNames[2] != want[2] {
		t.Errorf("std:aws env names = %v, want %v", got.EnvNames, want)
	}
	if got.Source.Path != "/home/u/.aws/credentials" {
		t.Errorf("source = %+v", got.Source)
	}
}

// std:npm is now a BROKERED (inject) passthrough secret → NPM_TOKEN; nothing on
// the box.
func TestResolveStdNpmInject(t *testing.T) {
	uc := ucWith(map[string]Source{"npm": {Cmd: "op read op://v/npm/token"}}, nil)
	r, _, errs := Resolve(manifest(RepoSecret{Key: "std:npm"}), uc, "acme/widget")
	if len(errs) != 0 || len(r) != 1 {
		t.Fatalf("resolved=%d errs=%v", len(r), errs)
	}
	got := r[0]
	if got.Strategy != StrategyInject || got.Extract != ExtractPassthrough ||
		len(got.EnvNames) != 1 || got.EnvNames[0] != "NPM_TOKEN" || got.Dest != "" {
		t.Errorf("std:npm resolved wrong: %+v", got)
	}
}

// std:claude carries the Claude Code subscription OAuth token (`claude
// setup-token`) as a BROKERED (inject) passthrough secret: the broker injects
// CLAUDE_CODE_OAUTH_TOKEN into a `devbox run` child on use. It moved off the
// resident `env` strategy (which left the token in every shell + a readable env.d
// file) onto inject — never resident on the box. NOT a ~/.claude/.credentials.json
// copy (that rotating file raced across machines and self-clobbered its symlink).
func TestResolveStdClaude(t *testing.T) {
	uc := ucWith(map[string]Source{"claude": {Path: "/home/u/.config/op/claude-token"}}, nil)
	r, unmapped, errs := Resolve(manifest(RepoSecret{Key: "std:claude"}), uc, "acme/widget")
	if len(errs) != 0 || len(unmapped) != 0 || len(r) != 1 {
		t.Fatalf("want 1 resolved, got resolved=%d unmapped=%d errs=%v", len(r), len(unmapped), errs)
	}
	got := r[0]
	if got.Strategy != StrategyInject || got.Extract != ExtractPassthrough ||
		len(got.EnvNames) != 1 || got.EnvNames[0] != "CLAUDE_CODE_OAUTH_TOKEN" || got.Dest != "" {
		t.Errorf("std:claude resolved wrong: %+v", got)
	}
	if got.Source.Path != "/home/u/.config/op/claude-token" {
		t.Errorf("source = %+v", got.Source)
	}
}

// A local:/org: manifest can declare an env-var secret with `env`; the dest is
// derived from the validated variable name (never free-form), strategy is env.
func TestResolveEnvFromManifest(t *testing.T) {
	uc := ucWith(nil, map[string]RepoEntry{
		"github.com/acme/widget": {Map: map[string]Source{"local:tok": {Cmd: "op read op://v/i/c"}}},
	})
	r, _, errs := Resolve(manifest(RepoSecret{Key: "local:tok", Env: "MY_API_TOKEN"}), uc, "acme/widget")
	if len(errs) != 0 || len(r) != 1 {
		t.Fatalf("resolved=%d errs=%v", len(r), errs)
	}
	got := r[0]
	if got.Strategy != StrategyEnv || len(got.EnvNames) != 1 || got.EnvNames[0] != "MY_API_TOKEN" ||
		got.Dest != ".config/rift/env.d/MY_API_TOKEN" || !got.Tmpfs {
		t.Errorf("env resolved wrong: %+v", got)
	}
}

// `env` and `dest` on one entry is a declaration error.
func TestResolveEnvAndDestMutuallyExclusive(t *testing.T) {
	uc := ucWith(nil, map[string]RepoEntry{
		"github.com/acme/widget": {Map: map[string]Source{"local:x": {Path: "/x"}}},
	})
	_, _, errs := Resolve(manifest(RepoSecret{Key: "local:x", Env: "FOO", Dest: "~/foo"}), uc, "acme/widget")
	if len(errs) != 1 {
		t.Fatalf("want 1 error for env+dest, got %v", errs)
	}
}

// Manifest-declared env names are validated as identifiers and denylisted for
// shell/loader-control variables (the env analog of deniedDest).
func TestResolveEnvNameRejected(t *testing.T) {
	uc := ucWith(nil, map[string]RepoEntry{
		"github.com/acme/widget": {Map: map[string]Source{"local:x": {Path: "/x"}}},
	})
	for _, bad := range []string{
		"", "1FOO", "FOO-BAR", "FOO BAR", "foo.bar", // invalid identifiers
		"PATH", "HOME", "IFS", "BASH_ENV", "ENV", "PROMPT_COMMAND", "PS1", "CDPATH", // control vars
		"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "BASH_FUNC_foo", // denied prefixes
		"ld_preload", // case-insensitive denylist
	} {
		_, _, errs := Resolve(manifest(RepoSecret{Key: "local:x", Env: bad}), uc, "acme/widget")
		if len(errs) == 0 {
			t.Errorf("env name %q should be rejected", bad)
		}
	}
	for _, ok := range []string{"FOO", "MY_API_TOKEN", "_X", "claude_token", "A1_B2"} {
		_, _, errs := Resolve(manifest(RepoSecret{Key: "local:x", Env: ok}), uc, "acme/widget")
		if len(errs) != 0 {
			t.Errorf("env name %q should pass: %v", ok, errs)
		}
	}
}

func TestResolveStdUnmappedIsUnmapped(t *testing.T) {
	uc := ucWith(nil, nil) // no defaults
	// std:gcp is a copy secret → an unmapped one carries its conventional dest.
	r, unmapped, errs := Resolve(manifest(RepoSecret{Key: "std:gcp"}), uc, "acme/widget")
	if len(errs) != 0 || len(r) != 0 || len(unmapped) != 1 {
		t.Fatalf("want 1 unmapped, got resolved=%d unmapped=%d errs=%v", len(r), len(unmapped), errs)
	}
	if unmapped[0].Dest != ".config/gcloud/application_default_credentials.json" {
		t.Errorf("unmapped dest = %q", unmapped[0].Dest)
	}
}

// An unmapped inject secret (std:aws) has no on-box dest; it surfaces by its env
// var name(s) instead.
func TestResolveStdInjectUnmappedIsUnmapped(t *testing.T) {
	uc := ucWith(nil, nil) // no defaults
	r, unmapped, errs := Resolve(manifest(RepoSecret{Key: "std:aws"}), uc, "acme/widget")
	if len(errs) != 0 || len(r) != 0 || len(unmapped) != 1 {
		t.Fatalf("want 1 unmapped, got resolved=%d unmapped=%d errs=%v", len(r), len(unmapped), errs)
	}
	u := unmapped[0]
	if u.Dest != "" || len(u.EnvNames) != 3 || u.EnvNames[0] != "AWS_ACCESS_KEY_ID" {
		t.Errorf("unmapped inject = %+v (want empty dest, 3 env names)", u)
	}
	if u.Label() != "$AWS_ACCESS_KEY_ID $AWS_SECRET_ACCESS_KEY $AWS_SESSION_TOKEN" {
		t.Errorf("unmapped inject label = %q", u.Label())
	}
}

func TestResolveLocalNeedsDest(t *testing.T) {
	uc := ucWith(nil, map[string]RepoEntry{
		"github.com/acme/widget": {Map: map[string]Source{"local:env": {Path: "/x"}}},
	})
	_, _, errs := Resolve(manifest(RepoSecret{Key: "local:env"}), uc, "acme/widget") // no dest
	if len(errs) != 1 {
		t.Fatalf("want 1 error for missing dest, got %v", errs)
	}
}

func TestResolveLocalFromRepoMap(t *testing.T) {
	uc := ucWith(nil, map[string]RepoEntry{
		"github.com/acme/widget": {Map: map[string]Source{"local:env": {Cmd: "printf v"}}},
	})
	r, _, errs := Resolve(manifest(RepoSecret{Key: "local:env", Dest: "~/app/.env"}), uc, "acme/widget")
	if len(errs) != 0 || len(r) != 1 {
		t.Fatalf("resolved=%d errs=%v", len(r), errs)
	}
	if r[0].Dest != "app/.env" || r[0].Source.Cmd != "printf v" {
		t.Errorf("resolved = %+v", r[0])
	}
}

func TestResolveLocalNoGlobalFallback(t *testing.T) {
	// A local: key must NOT pick up a std-style global default by bare name.
	uc := ucWith(map[string]Source{"env": {Path: "/should/not/be/used"}}, nil)
	_, unmapped, errs := Resolve(manifest(RepoSecret{Key: "local:env", Dest: "~/app/.env"}), uc, "acme/widget")
	if len(errs) != 0 || len(unmapped) != 1 {
		t.Fatalf("want unmapped (no fallback), got unmapped=%d errs=%v", len(unmapped), errs)
	}
}

func TestResolveForwardStrategy(t *testing.T) {
	uc := ucWith(nil, nil)
	r, _, errs := Resolve(manifest(RepoSecret{Key: "std:ssh"}), uc, "acme/widget")
	if len(errs) != 0 || len(r) != 1 || r[0].Strategy != StrategyForward {
		t.Fatalf("std:ssh should be forward: r=%+v errs=%v", r, errs)
	}
}

// std:ssh-key is the OPT-IN key-PUSH variant: a copy
// strategy that lands the mapped private key in the box tmpfs (~/.ssh/id_rift,
// 0600, tmpfs), so a fully-detached job can authenticate. Distinct from std:ssh
// (forward-only, no key on box).
func TestResolveStdSSHKeyPush(t *testing.T) {
	uc := ucWith(map[string]Source{"ssh-key": {Path: "/home/u/.ssh/id_ed25519"}}, nil)
	r, unmapped, errs := Resolve(manifest(RepoSecret{Key: "std:ssh-key"}), uc, "acme/widget")
	if len(errs) != 0 || len(unmapped) != 0 || len(r) != 1 {
		t.Fatalf("want 1 resolved, got resolved=%d unmapped=%d errs=%v", len(r), len(unmapped), errs)
	}
	got := r[0]
	if got.Strategy != StrategyCopy || got.Dest != ".ssh/id_rift" || got.Mode != "0600" || !got.Tmpfs {
		t.Errorf("std:ssh-key resolved wrong: %+v", got)
	}
	if got.Source.Path != "/home/u/.ssh/id_ed25519" {
		t.Errorf("source = %+v", got.Source)
	}
}

// std:ssh-key is unmapped (no source) by default → it is NEVER pushed unless the
// user explicitly maps it (opt-in property).
func TestResolveStdSSHKeyUnmappedByDefault(t *testing.T) {
	uc := ucWith(nil, nil)
	r, unmapped, errs := Resolve(manifest(RepoSecret{Key: "std:ssh-key"}), uc, "acme/widget")
	if len(errs) != 0 || len(r) != 0 || len(unmapped) != 1 {
		t.Fatalf("std:ssh-key with no mapping should be unmapped, not pushed: r=%+v unmapped=%+v errs=%v", r, unmapped, errs)
	}
}

func TestResolveDestTraversalRejected(t *testing.T) {
	uc := ucWith(nil, map[string]RepoEntry{
		"github.com/acme/widget": {Map: map[string]Source{"local:x": {Path: "/x"}}},
	})
	for _, bad := range []string{"~/../etc/passwd", "/etc/passwd", "~/a/../b", "~/a//b", "~/"} {
		_, _, errs := Resolve(manifest(RepoSecret{Key: "local:x", Dest: bad}), uc, "acme/widget")
		if len(errs) == 0 {
			t.Errorf("dest %q should be rejected", bad)
		}
	}
}

func TestResolveLocalTmpfsExplicitFalse(t *testing.T) {
	no := false
	uc := ucWith(nil, map[string]RepoEntry{
		"github.com/acme/widget": {Map: map[string]Source{"local:env": {Path: "/x"}}},
	})
	r, _, _ := Resolve(manifest(RepoSecret{Key: "local:env", Dest: "~/app/.env", Tmpfs: &no}), uc, "acme/widget")
	if len(r) != 1 || r[0].Tmpfs {
		t.Errorf("local: explicit tmpfs:false not honored: %+v", r)
	}
}

func TestResolveStdDestLocked(t *testing.T) {
	// A manifest cannot redirect a std: copy key's dest, widen its mode, or disable
	// tmpfs (std:gcp — a copy secret; aws is now inject and has no dest at all).
	no := false
	uc := ucWith(map[string]Source{"gcp": {Path: "/x"}}, nil)
	r, _, errs := Resolve(manifest(RepoSecret{Key: "std:gcp", Dest: "~/.bashrc", Mode: "0666", Tmpfs: &no}), uc, "acme/widget")
	if len(errs) != 0 || len(r) != 1 {
		t.Fatalf("resolved=%d errs=%v", len(r), errs)
	}
	if r[0].Dest != ".config/gcloud/application_default_credentials.json" || r[0].Mode != "0600" || !r[0].Tmpfs {
		t.Errorf("std: dest/mode/tmpfs not locked to registry: %+v", r[0])
	}
}

func TestResolveUnknownStdKeyErrors(t *testing.T) {
	uc := ucWith(map[string]Source{"bogus": {Path: "/x"}}, nil)
	_, _, errs := Resolve(manifest(RepoSecret{Key: "std:bogus"}), uc, "acme/widget")
	if len(errs) != 1 {
		t.Fatalf("unknown std: key should error, got %v", errs)
	}
}

func TestResolveLocalModeClamp(t *testing.T) {
	uc := ucWith(nil, map[string]RepoEntry{
		"github.com/acme/widget": {Map: map[string]Source{"local:x": {Path: "/x"}}},
	})
	for _, bad := range []string{"0666", "0777", "0660", "4755", "0750", "0606"} {
		_, _, errs := Resolve(manifest(RepoSecret{Key: "local:x", Dest: "~/x", Mode: bad}), uc, "acme/widget")
		if len(errs) == 0 {
			t.Errorf("mode %q should be rejected", bad)
		}
	}
	for _, good := range []string{"0600", "0640", "0644", "0400", "600", "0700"} {
		r, _, errs := Resolve(manifest(RepoSecret{Key: "local:x", Dest: "~/x", Mode: good}), uc, "acme/widget")
		if len(errs) != 0 || len(r) != 1 {
			t.Errorf("mode %q should be accepted: errs=%v", good, errs)
		}
	}
}

func TestResolveDeniedDest(t *testing.T) {
	uc := ucWith(nil, map[string]RepoEntry{
		"github.com/acme/widget": {Map: map[string]Source{"local:x": {Path: "/x"}}},
	})
	for _, bad := range []string{"~/.bashrc", "~/.profile", "~/.zshrc", "~/.bash_aliases",
		"~/.ssh/authorized_keys", "~/.ssh/config", "~/.config/systemd/user/x.service",
		"~/repo/.git/hooks/pre-commit", "~/.gitconfig", "~/.config/git/config",
		"~/myrepo/.git/config", "~/myrepo/.git/hooks/pre-push",
		"~/myrepo/.git/info/attributes", "~/myrepo/.git/modules/sub/config",
		"~/myrepo/.git/worktrees/wt/config.worktree", // every git-internal path
		"~/.config/fish/config.fish", "~/.envrc", "~/.inputrc", "~/.xprofile",
		"~/bin/foo", "~/.local/bin/foo", "~/.bashrc.d/x.sh",
		"~/.vimrc", "~/.config/nvim/init.lua", "~/.gitattributes", "~/.gnupg/gpg.conf",
		"~/.tmux.conf", "~/.config/direnv/direnvrc", "~/.config/wezterm/wezterm.lua",
		"~/.BASHRC", "~/.SSH/authorized_keys"} { // case variants too
		_, _, errs := Resolve(manifest(RepoSecret{Key: "local:x", Dest: bad}), uc, "acme/widget")
		if len(errs) == 0 {
			t.Errorf("denied dest %q should be rejected", bad)
		}
	}
	for _, ok := range []string{"~/.aws/credentials", "~/.npmrc", "~/.netrc", "~/.ssh/id_ed25519",
		"~/.config/gcloud/adc.json", "~/.pgpass", "~/.docker/config.json", "~/app/.env"} {
		_, _, errs := Resolve(manifest(RepoSecret{Key: "local:x", Dest: ok}), uc, "acme/widget")
		if len(errs) != 0 {
			t.Errorf("legit dest %q should pass: %v", ok, errs)
		}
	}
}

func TestResolveDedupsDuplicateKeys(t *testing.T) {
	uc := ucWith(map[string]Source{"aws": {Path: "/x"}}, nil)
	r, _, errs := Resolve(manifest(
		RepoSecret{Key: "std:aws"},
		RepoSecret{Key: "std:aws"},
		RepoSecret{Key: "std:aws"},
	), uc, "acme/widget")
	if len(errs) != 0 || len(r) != 1 {
		t.Fatalf("duplicate keys should collapse to 1, got %d (errs=%v)", len(r), errs)
	}
}

func TestResolveDuplicatePaddingKeepsRealSecret(t *testing.T) {
	// Padding the manifest with many duplicates must NOT suppress a real secret.
	uc := ucWith(map[string]Source{"aws": {Path: "/a"}},
		map[string]RepoEntry{"github.com/acme/widget": {Map: map[string]Source{"local:env": {Path: "/e"}}}})
	secs := make([]RepoSecret, 0, 300)
	for i := 0; i < 300; i++ {
		secs = append(secs, RepoSecret{Key: "std:aws"}) // all duplicates → 1 distinct
	}
	secs = append(secs, RepoSecret{Key: "local:env", Dest: "~/app/.env"})
	r, _, errs := Resolve(&RepoManifest{Secrets: secs}, uc, "acme/widget")
	if len(errs) != 0 {
		t.Fatalf("unexpected errs: %v", errs)
	}
	keys := map[string]bool{}
	for _, x := range r {
		keys[x.Key.String()] = true
	}
	if !keys["std:aws"] || !keys["local:env"] {
		t.Errorf("duplicate padding suppressed a real secret: resolved keys=%v", keys)
	}
}

func TestResolvePerRepoOverridesDefault(t *testing.T) {
	uc := ucWith(
		map[string]Source{"aws": {Path: "/global/aws"}},
		map[string]RepoEntry{"github.com/acme/widget": {Map: map[string]Source{"std:aws": {Path: "/repo/aws"}}}},
	)
	r, _, _ := Resolve(manifest(RepoSecret{Key: "std:aws"}), uc, "acme/widget")
	if len(r) != 1 || r[0].Source.Path != "/repo/aws" {
		t.Errorf("per-repo override should win: %+v", r)
	}
}
