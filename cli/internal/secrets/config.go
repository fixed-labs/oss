package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// --- Repo manifest (.rift/secrets.json — UNTRUSTED, declares dests only) ---

// RepoSecret is one declared target. Note there is deliberately NO source field:
// the repo may name a destination, never a source (the core invariant). Unknown
// JSON fields are ignored, so a malicious `source` key is simply not read.
type RepoSecret struct {
	Key         string `json:"key"`
	Dest        string `json:"dest"`
	Env         string `json:"env"` // env-var name; mutually exclusive with dest
	Description string `json:"description"`
	Mode        string `json:"mode"`
	Tmpfs       *bool  `json:"tmpfs"` // nil → default true
}

type RepoManifest struct {
	Secrets []RepoSecret `json:"secrets"`
}

// ParseRepoManifest parses the manifest bytes fetched from the box.
func ParseRepoManifest(b []byte) (*RepoManifest, error) {
	var m RepoManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadRepoManifestFile reads a manifest from a local path (the offline `secrets
// status` preview). A missing file yields a nil manifest, not an error.
func LoadRepoManifestFile(path string) (*RepoManifest, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseRepoManifest(b)
}

// --- User config (~/.config/rift/secrets.json — TRUSTED, supplies sources) ---

// Source is a user-supplied secret source: a path / the literal "forward"
// sentinel (string form), or a command whose stdout is the secret (object form
// {"cmd":"…"}). Only the user (never the repo) supplies a Source.
type Source struct {
	Path string // file path (~-expanded) or the literal "forward"
	Cmd  string // command whose stdout is the secret
}

func (s Source) isForward() bool { return s.Path == "forward" && s.Cmd == "" }
func (s Source) empty() bool     { return s.Path == "" && s.Cmd == "" }

func (s *Source) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		s.Path = str
		return nil
	}
	var obj struct {
		Cmd  string `json:"cmd"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	if obj.Cmd != "" && obj.Path != "" {
		return fmt.Errorf("secret source must be a path OR a cmd, not both")
	}
	s.Cmd, s.Path = obj.Cmd, obj.Path
	return nil
}

func (s Source) MarshalJSON() ([]byte, error) {
	if s.Cmd != "" {
		return json.Marshal(struct {
			Cmd string `json:"cmd"`
		}{s.Cmd})
	}
	return json.Marshal(s.Path)
}

// RepoEntry is the per-repo user config: a push policy and key→source mappings.
type RepoEntry struct {
	Policy string            `json:"policy,omitempty"` // ask (default) | auto-push | off
	Map    map[string]Source `json:"map,omitempty"`
}

type UserConfig struct {
	// Defaults map a std: key's bare name to a source, shared across all repos.
	Defaults map[string]Source `json:"defaults,omitempty"`
	// Repos maps a repo pattern (qualified/bare, glob-able) to its entry.
	Repos map[string]RepoEntry `json:"repos,omitempty"`
	// Trusted is the TOFU memory: qualified repo id → approved dests ("~/…").
	Trusted map[string][]string `json:"trusted,omitempty"`

	path string // where this was loaded from (for Save)
}

func defaultUserConfigPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "rift", "secrets.json"), nil
}

// LoadUserConfig loads the user config from path (or the default location when
// path==""). A missing file yields an empty config, not an error.
func LoadUserConfig(path string) (*UserConfig, error) {
	if path == "" {
		p, err := defaultUserConfigPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	uc := &UserConfig{
		Defaults: map[string]Source{},
		Repos:    map[string]RepoEntry{},
		Trusted:  map[string][]string{},
		path:     path,
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return uc, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, uc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if uc.Defaults == nil {
		uc.Defaults = map[string]Source{}
	}
	if uc.Repos == nil {
		uc.Repos = map[string]RepoEntry{}
	}
	if uc.Trusted == nil {
		uc.Trusted = map[string][]string{}
	}
	uc.path = path
	return uc, nil
}

// Save writes the user config 0600 (it may carry source-command strings).
func (uc *UserConfig) Save() error {
	if uc.path == "" {
		p, err := defaultUserConfigPath()
		if err != nil {
			return err
		}
		uc.path = p
	}
	if err := os.MkdirAll(filepath.Dir(uc.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(uc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(uc.path, b, 0o600)
}

// SetMapping records key→source for a repo (used by `devbox secrets map`).
func (uc *UserConfig) SetMapping(repoQualified, key string, src Source) {
	if uc.Repos == nil {
		uc.Repos = map[string]RepoEntry{}
	}
	e := uc.Repos[repoQualified]
	if e.Map == nil {
		e.Map = map[string]Source{}
	}
	e.Map[key] = src
	uc.Repos[repoQualified] = e
}

func (uc *UserConfig) trustedHas(repoID, dest string) bool {
	q, _ := normalizeRepoID(repoID)
	for _, d := range uc.Trusted[q] {
		if d == dest {
			return true
		}
	}
	return false
}

func (uc *UserConfig) addTrusted(repoID, dest string) {
	q, _ := normalizeRepoID(repoID)
	if uc.trustedHas(repoID, dest) {
		return
	}
	if uc.Trusted == nil {
		uc.Trusted = map[string][]string{}
	}
	uc.Trusted[q] = append(uc.Trusted[q], dest)
}
