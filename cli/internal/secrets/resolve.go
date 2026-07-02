package secrets

import (
	"fmt"
	"strings"
)

// Resolved is a fully-resolved secret ready to act on.
type Resolved struct {
	Key      Key
	Dest     string // home-relative (no leading "~/", no ".."); empty for forward/inject
	Mode     string // octal, validated [0-7]{3,4}; empty for forward/inject
	Tmpfs    bool   // for env: the derived env.d file is a tmpfs symlink (inert for inject)
	Strategy Strategy
	EnvNames []string // StrategyEnv/StrategyInject: the env var name(s); nil otherwise
	Extract  Extract  // StrategyInject: how Source bytes map to EnvNames; "" otherwise
	Source   Source   // the resolved source (empty only for forward)
	Desc     string
}

// Unmapped is a declared key with no source — surfaced so the user can map it.
type Unmapped struct {
	Key      Key
	Dest     string   // home-relative
	EnvNames []string // StrategyEnv/StrategyInject: the env var name(s); nil otherwise
	Desc     string
}

// Label is the human-facing target: `$NAME` (joined for several) for an
// env-var/inject secret, else `~/dest`.
func (u Unmapped) Label() string {
	if len(u.EnvNames) > 0 {
		return envLabel(u.EnvNames)
	}
	return "~/" + u.Dest
}

// envLabel renders one or more env var names as `$A` / `$A $B $C`.
func envLabel(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = "$" + n
	}
	return strings.Join(parts, " ")
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// Resolve turns a repo manifest + user config into the action list for repoID.
// Per-entry problems are returned as errs (and that entry dropped); the rest
// resolve normally.
//
// Trust split: std: keys are tool-owned, so their dest/mode/tmpfs come ONLY
// from the registry — a (untrusted) manifest's dest/mode/tmpfs on a std: key is
// ignored, so a repo can't redirect a globally-mapped secret (e.g. real AWS
// creds) to ~/.bashrc or widen its mode. local:/org: keys declare their own
// dest+mode (the dest is confined + denylisted, the mode clamped).
func Resolve(m *RepoManifest, uc *UserConfig, repoID string) (resolved []Resolved, unmapped []Unmapped, errs []error) {
	matchKey, ok := matchRepo(sortedKeys(uc.Repos), repoID)
	var repoMap map[string]Source
	if ok {
		repoMap = uc.Repos[matchKey].Map
	}

	// Dedup duplicate keys so an untrusted manifest can't amplify work or
	// double-push (e.g. declaring std:aws ×N). First declaration wins. No count
	// cap: the manifest byte cap bounds the entry count, the I/O budget bounds the
	// actual work, and an untrusted manifest can't make a key actionable anyway
	// (sources come only from the user's config) — so a count cap would only risk
	// junk-key padding starving a real secret without adding protection.
	seen := map[string]bool{}
	for _, sec := range m.Secrets {
		k, err := ParseKey(sec.Key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if seen[k.String()] {
			continue // first declaration wins
		}
		seen[k.String()] = true

		strat := StrategyCopy
		var reg stdEntry
		isStd := false
		if k.NS == NSStd {
			e, found := stdRegistry[k.Name]
			if !found {
				errs = append(errs, fmt.Errorf("%s: unknown std: key (not in the built-in registry)", k))
				continue
			}
			reg, isStd, strat = e, true, e.Strategy
		}

		// Source: explicit per-repo mapping first; std: falls back to global
		// defaults; org:/local: have no global fallback.
		var src Source
		if repoMap != nil {
			src = repoMap[k.String()]
		}
		if src.empty() && k.NS == NSStd {
			src = uc.Defaults[k.Name]
		}
		if src.isForward() {
			strat = StrategyForward
		}

		if strat == StrategyForward {
			resolved = append(resolved, Resolved{Key: k, Strategy: StrategyForward, Desc: sec.Description})
			continue
		}

		var dest, mode string
		var envNames []string
		var extract Extract
		var tmpfs bool
		switch {
		case isStd && reg.Strategy == StrategyInject:
			// Tool-owned brokered secret: env var name(s) + extractor from the
			// registry (trusted). NEVER written to the box — no dest/mode is set and
			// reconcile skips it; the broker injects EnvNames into a `devbox run`
			// child. The env.d dest is intentionally NOT derived (it would be inert
			// anyway).
			for _, ev := range reg.Env {
				if !validEnvName(ev) {
					errs = append(errs, fmt.Errorf("%s: registry env name %q is not a valid identifier", k, ev))
					envNames = nil
					break
				}
				envNames = append(envNames, ev)
			}
			if envNames == nil {
				continue
			}
			extract = reg.Extract
			strat = StrategyInject
		case isStd && reg.Strategy == StrategyEnv:
			// Tool-owned env var (DEPRECATED for std: keys — see StrategyEnv doc):
			// name from the registry (trusted), dest derived.
			if len(reg.Env) != 1 || !validEnvName(reg.Env[0]) {
				errs = append(errs, fmt.Errorf("%s: registry env name %v is not a single valid identifier", k, reg.Env))
				continue
			}
			envNames, dest, mode, tmpfs = []string{reg.Env[0]}, envDest(reg.Env[0]), "0600", true
		case isStd:
			// Tool-owned copy: registry only; manifest dest/mode/tmpfs ignored.
			dest, mode, tmpfs = reg.Dest, reg.Mode, true
		case sec.Env != "":
			// local:/org: env var: the manifest names the variable (untrusted —
			// validate the identifier AND denylist shell/loader-control names, the
			// env analog of deniedDest), but the dest is derived, never free-form.
			if sec.Dest != "" {
				errs = append(errs, fmt.Errorf("%s: declare `env` OR `dest`, not both", k))
				continue
			}
			if !validEnvName(sec.Env) {
				errs = append(errs, fmt.Errorf("%s: env %q is not a valid identifier ([A-Za-z_][A-Za-z0-9_]*)", k, sec.Env))
				continue
			}
			if deniedEnvName(sec.Env) {
				errs = append(errs, fmt.Errorf("%s: env %q is not allowed (shell/loader-control variable)", k, sec.Env))
				continue
			}
			mode = sec.Mode
			if mode == "" {
				mode = "0600"
			}
			if !validSafeMode(mode) {
				errs = append(errs, fmt.Errorf("%s: mode %q not allowed (octal; no group/other write or exec, no setuid — e.g. 0600/0640)", k, mode))
				continue
			}
			envNames, dest, tmpfs, strat = []string{sec.Env}, envDest(sec.Env), boolOr(sec.Tmpfs, true), StrategyEnv
		default:
			// local:/org: copy: declare their own dest (required) + mode.
			dest = sec.Dest
			if dest == "" {
				errs = append(errs, fmt.Errorf("%s: no dest (local:/org: keys must declare a dest or env)", k))
				continue
			}
			mode = sec.Mode
			if mode == "" {
				mode = "0600"
			}
			if !validSafeMode(mode) {
				errs = append(errs, fmt.Errorf("%s: mode %q not allowed (octal; no group/other write or exec, no setuid — e.g. 0600/0640)", k, mode))
				continue
			}
			tmpfs = boolOr(sec.Tmpfs, true)
		}

		// Inject secrets are never written to the box, so they have no dest to
		// validate; every other strategy here does.
		var rel string
		if strat != StrategyInject {
			rel, err = validateDest(dest)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", k, err))
				continue
			}
		}

		if src.empty() {
			unmapped = append(unmapped, Unmapped{Key: k, Dest: rel, EnvNames: envNames, Desc: sec.Description})
			continue
		}
		resolved = append(resolved, Resolved{
			Key:      k,
			Dest:     rel,
			Mode:     mode,
			Tmpfs:    tmpfs,
			Strategy: strat,
			EnvNames: envNames,
			Extract:  extract,
			Source:   src,
			Desc:     sec.Description,
		})
	}
	return resolved, unmapped, errs
}

// ResolveInjectSecret is the broker handler's entry point (the broker itself is
// owned by another component): given the loaded user config, the repo's directory
// name (its canonical id, used to pick the per-repo source mapping), and a secret
// name as the agent passed it to `devbox run --secret`, it returns the resolved
// inject secret — its EnvNames, Extract, and resolved Source — ready for
// ReadSource → ExtractCredential.
//
// The name resolves like a bare `--secret`: a name with no namespace is treated
// as the std: built-in (e.g. `aws` → std:aws). It errors if the secret is
// unknown, is not an inject secret (copy/forward/env are never brokered this way),
// or has no source mapped.
func ResolveInjectSecret(uc *UserConfig, repoDir, name string) (Resolved, error) {
	k, err := ParseKey(name)
	if err != nil {
		return Resolved{}, err
	}
	// A bare name (no namespace) means the std: built-in on the `devbox run` path.
	if !strings.Contains(name, ":") {
		if _, ok := stdRegistry[k.Name]; ok {
			k.NS = NSStd
		}
	}
	if k.NS != NSStd {
		return Resolved{}, fmt.Errorf("%s: only std: secrets are brokered for injection", k)
	}
	reg, found := stdRegistry[k.Name]
	if !found {
		return Resolved{}, fmt.Errorf("%s: unknown std: key (not in the built-in registry)", k)
	}
	if reg.Strategy != StrategyInject {
		return Resolved{}, fmt.Errorf("%s: secret is %q, not an injected secret", k, reg.Strategy)
	}

	src := sourceForKey(uc, repoDir, k)
	if src.empty() {
		return Resolved{}, fmt.Errorf("%s: no source mapped (set one with `rift secrets map`)", k)
	}

	var envNames []string
	for _, ev := range reg.Env {
		if !validEnvName(ev) {
			return Resolved{}, fmt.Errorf("%s: registry env name %q is not a valid identifier", k, ev)
		}
		envNames = append(envNames, ev)
	}
	return Resolved{
		Key:      k,
		Strategy: StrategyInject,
		EnvNames: envNames,
		Extract:  reg.Extract,
		Source:   src,
	}, nil
}

// sourceForKey resolves a key's source the same way Resolve does: an explicit
// per-repo mapping first, then (for std: keys) the global defaults.
func sourceForKey(uc *UserConfig, repoDir string, k Key) Source {
	var src Source
	if matchKey, ok := matchRepo(sortedKeys(uc.Repos), repoDir); ok {
		if m := uc.Repos[matchKey].Map; m != nil {
			src = m[k.String()]
		}
	}
	if src.empty() && k.NS == NSStd {
		src = uc.Defaults[k.Name]
	}
	return src
}

// validateDest enforces the confinement invariant: a dest must be home-relative
// (start with "~/"), use a conservative charset, and contain no "."/".."/empty
// segments. Returns the home-relative remainder (no leading "~/").
func validateDest(dest string) (rel string, err error) {
	d := strings.TrimSpace(dest)
	if !strings.HasPrefix(d, "~/") {
		return "", fmt.Errorf("dest %q must be home-relative (start with ~/)", dest)
	}
	rel = strings.TrimPrefix(d, "~/")
	if rel == "" {
		return "", fmt.Errorf("dest %q is empty", dest)
	}
	if !destCharset(rel) {
		return "", fmt.Errorf("dest %q has unsupported characters (allowed: A-Za-z0-9._-/)", dest)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("dest %q has a bad path segment %q", dest, seg)
		}
	}
	if deniedDest(rel) {
		return "", fmt.Errorf("dest %q targets a path secrets may not be written to (shell rc / ssh authorized_keys / systemd unit / autostart / git hook)", dest)
	}
	return rel, nil
}

// envDirRel is the box-side directory the base image sources to export env-var
// secrets (see nix/devboxes-base/module.nix): one file per variable, named for
// the variable, a tmpfs symlink like every other secret.
const envDirRel = ".config/rift/env.d/"

// envDest is the "~/"-prefixed dest for an env-var secret named v. v must have
// passed validEnvName, so the derived path is charset-safe, traversal-free, and
// off the deniedDest denylist (validateDest re-checks it anyway).
func envDest(v string) string { return "~/" + envDirRel + v }

// validEnvName reports whether s is a POSIX-shell-safe environment variable
// identifier ([A-Za-z_][A-Za-z0-9_]*), bounded in length. Keeps the on-box
// `export "$name=…"` and the per-variable filename injection-safe.
func validEnvName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// deniedEnvName blocks shell- and loader-control variables — the env analog of
// deniedDest. An untrusted manifest chooses the variable NAME (never the value:
// sources are always user-supplied), so routing a user's own secret into PATH /
// LD_PRELOAD / PROMPT_COMMAND / a BASH_FUNC_* slot is low-yield, but it's a
// code-exec / shell-break surface with no legitimate use as a secret target, so
// reject it. std: env names come from the trusted registry and skip this check.
// Matched case-insensitively — a mis-cased LD_PRELOAD is inert anyway, and the
// cost of over-blocking lowercase variants is nil.
func deniedEnvName(s string) bool {
	up := strings.ToUpper(s)
	switch up {
	case "PATH", "HOME", "SHELL", "USER", "LOGNAME", "PWD", "OLDPWD", "TMPDIR",
		"IFS", "ENV", "BASH_ENV", "CDPATH", "FPATH", "SHELLOPTS", "BASHOPTS",
		"GLIBC_TUNABLES", "HOSTALIASES", "NIS_PATH",
		"PS1", "PS2", "PS3", "PS4", "PROMPT_COMMAND", "PROMPT_DIRTRIM":
		return true
	}
	for _, p := range []string{"LD_", "DYLD_", "BASH_FUNC_"} {
		if strings.HasPrefix(up, p) {
			return true
		}
	}
	return false
}

// deniedDest blocks dests that execute code, shadow commands, or grant access on
// login/use — writing there (even the user's own secret bytes, mis-aimed by an
// untrusted manifest the user might approve without scrutiny) is a persistence /
// command-exec / credential-misdirection vector, never a legit secret target.
// Best-effort, not a complete boundary: the real protections are the approval
// prompt (which shows the dest) and that the bytes come from the user, not the
// repo. Matched case-insensitively (cheap, and safe on a case-insensitive home);
// the on-box realpath guard backstops symlinked escapes. rel has no leading "~/".
func deniedDest(rel string) bool {
	low := strings.ToLower(rel)
	base := low
	if i := strings.LastIndexByte(low, '/'); i >= 0 {
		base = low[i+1:]
	}
	switch base { // shell/login startup + tool config-exec files, in any directory
	case ".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".bash_aliases",
		".profile", ".zshrc", ".zprofile", ".zshenv", ".zlogin", ".zlogout",
		".kshrc", ".cshrc", ".tcshrc", ".login", ".logout",
		".pam_environment", ".forward", ".inputrc", ".envrc",
		".xprofile", ".xinitrc", ".xsession", ".xsessionrc",
		".gitconfig", ".gitattributes", ".vimrc", ".gvimrc", ".selected_editor", ".tmux.conf":
		return true
	}
	switch low { // specific access/exec files at a fixed path
	case ".ssh/authorized_keys", ".ssh/authorized_keys2", ".ssh/config", ".ssh/rc", ".ssh/environment",
		".config/git/config", ".config/git/attributes", ".gnupg/gpg.conf", ".gnupg/gpg-agent.conf":
		return true
	}
	for _, p := range []string{ // exec / shadow / unit directories
		".config/systemd/", ".config/autostart/", ".config/environment.d/",
		".local/share/systemd/", ".config/fish/", ".config/nvim/", ".vim/",
		".config/direnv/", ".config/wezterm/",
		".bashrc.d/", ".zshrc.d/", "bin/", ".local/bin/",
	} {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	// Anything inside a .git directory is git-internal (config, hooks/*,
	// info/attributes, modules/*/config, worktrees/*/config.worktree, …) and
	// code-exec on a routine git op — and none of it is a legit secret dest. Block
	// the whole tree rather than play whack-a-mole with individual git files.
	return strings.Contains(low, ".git/")
}

func destCharset(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-', c == '/':
		default:
			return false
		}
	}
	return true
}

func validMode(s string) bool {
	if len(s) < 3 || len(s) > 4 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

// validSafeMode clamps a manifest-supplied (local:) mode: octal, no special
// bits (setuid/setgid/sticky), and group/other limited to read-only — a secret
// must never be group/other writable or executable. Allows e.g. 0600, 0640,
// 0644, 0400; rejects 0666, 0777, 0660, 4755, 0750.
func validSafeMode(s string) bool {
	if !validMode(s) {
		return false
	}
	if len(s) == 4 {
		if s[0] != '0' { // leading digit carries setuid/setgid/sticky
			return false
		}
		s = s[1:]
	}
	// s is now owner,group,other; group and other must be 0 (none) or 4 (read).
	return (s[1] == '0' || s[1] == '4') && (s[2] == '0' || s[2] == '4')
}
