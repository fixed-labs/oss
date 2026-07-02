package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gcpFP is the trust fingerprint of std:gcp → its conventional dest, mode 0600,
// tmpfs (the registry values; tmpfs intent is always true for std keys). std:gcp
// is the canonical std COPY secret these push tests exercise — std:aws is now an
// inject secret (never pushed), tested separately below.
const gcpDest = ".config/gcloud/application_default_credentials.json"
const gcpFP = "std:gcp|~/" + gcpDest + "|0600|t"

type recordedPush struct {
	tmpfs bool
	data  []byte
}

type fakeExecer struct {
	store      bool
	manifest   string
	hashes     map[string]string // rel -> sha256 ("" = absent)
	onStore    map[string]bool   // rel -> is a symlink into the store
	pushes     []recordedPush
	failHashes bool // inject a box-I/O error on the hash-read round trip
}

func (f *fakeExecer) Run(ctx context.Context, script string, stdin []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case strings.Contains(script, "base64 -w0"): // read config + store presence
		var b strings.Builder
		if f.store {
			b.WriteString("STORE 1\n")
		} else {
			b.WriteString("STORE 0\n")
		}
		if f.manifest != "" {
			b.WriteString("CONFIG " + base64.StdEncoding.EncodeToString([]byte(f.manifest)) + "\n")
		}
		return []byte(b.String()), nil
	case strings.Contains(script, "sha256sum"): // read current hashes + location
		if f.failHashes {
			return nil, fmt.Errorf("fakeExecer: injected box-I/O failure on hash read")
		}
		var b strings.Builder
		for _, rel := range strings.Split(strings.TrimRight(string(stdin), "\n"), "\n") {
			if rel == "" {
				continue
			}
			h := f.hashes[rel]
			if h == "" {
				h = "-"
			}
			loc := "0"
			if f.onStore[rel] {
				loc = "1"
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\n", rel, h, loc)
		}
		return []byte(b.String()), nil
	case strings.Contains(script, "umask 077"): // push
		f.pushes = append(f.pushes, recordedPush{
			tmpfs: strings.Contains(script, "ln -sfn"), // only the tmpfs branch symlinks
			data:  append([]byte(nil), stdin...),
		})
		return nil, nil
	default:
		return nil, fmt.Errorf("fakeExecer: unexpected script:\n%s", script)
	}
}

func writeUserConfig(t *testing.T, uc map[string]any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secrets.json")
	b, err := json.Marshal(uc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func secretFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const testManifest = `{"secrets":[
  {"key":"std:gcp"},
  {"key":"local:env","dest":"~/app/.env"},
  {"key":"std:ssh"},
  {"key":"local:orphan","dest":"~/orphan"}
]}`

func TestReconcileAutoPushTrusted(t *testing.T) {
	gcp := secretFile(t, "GCPDATA")
	ucPath := writeUserConfig(t, map[string]any{
		"defaults": map[string]any{"gcp": gcp},
		"repos":    map[string]any{"github.com/acme/widget": map[string]any{"policy": "auto-push"}},
		"trusted":  map[string]any{"github.com/acme/widget": []string{gcpFP}},
	})
	fe := &fakeExecer{store: true, manifest: testManifest}
	var out bytes.Buffer
	res, err := Reconcile(context.Background(), fe, "acme/widget", Options{
		UserConfigPath: ucPath, Out: &out, Interactive: false,
	})
	if err != nil {
		t.Fatalf("reconcile: %v\n%s", err, out.String())
	}
	if !res.ForwardAgent {
		t.Errorf("std:ssh should set ForwardAgent")
	}
	// std:gcp trusted+auto-push → pushed (tmpfs); local:env untrusted first-sight,
	// non-interactive → skipped; local:orphan unmapped.
	if res.Pushed != 1 || len(fe.pushes) != 1 || !fe.pushes[0].tmpfs || string(fe.pushes[0].data) != "GCPDATA" {
		t.Fatalf("push wrong: pushed=%d pushes=%+v\n%s", res.Pushed, fe.pushes, out.String())
	}
	if !strings.Contains(out.String(), "local:orphan") {
		t.Errorf("expected unmapped report for local:orphan:\n%s", out.String())
	}
}

// TestReconcileForwardAgentSurvivesLaterIOError locks in that a box-I/O failure
// AFTER the manifest is parsed (here: the hash-read round trip) still returns
// ForwardAgent=true. The std:ssh forwarding decision is made in the first pass
// from the parsed manifest, before any per-secret box I/O, and forwarding pushes
// no bytes to the box — so a mid-sync hiccup must not silently disable it. The
// connect path (reconcileSecrets) relies on this: it now honors res.ForwardAgent
// even when Reconcile returns an error. Regression guard for the "agent
// forwarding silently off after a flaky reconnect" class.
func TestReconcileForwardAgentSurvivesLaterIOError(t *testing.T) {
	gcp := secretFile(t, "GCPDATA")
	ucPath := writeUserConfig(t, map[string]any{
		"defaults": map[string]any{"gcp": gcp},
		"repos":    map[string]any{"github.com/acme/widget": map[string]any{"policy": "auto-push"}},
		"trusted":  map[string]any{"github.com/acme/widget": []string{gcpFP}},
	})
	// testManifest has both std:ssh (forward) and std:gcp (a trusted, actionable
	// copy item) — so the first pass sets ForwardAgent, then the hash read for gcp
	// runs and fails.
	fe := &fakeExecer{store: true, manifest: testManifest, failHashes: true}
	var out bytes.Buffer
	res, err := Reconcile(context.Background(), fe, "acme/widget", Options{
		UserConfigPath: ucPath, Out: &out, Interactive: false,
	})
	if err == nil {
		t.Fatalf("expected an error from the injected hash-read failure\n%s", out.String())
	}
	if !res.ForwardAgent {
		t.Errorf("ForwardAgent must survive a post-manifest box-I/O error (got false)\n%s", out.String())
	}
	if len(fe.pushes) != 0 {
		t.Errorf("no secret should have been pushed after the hash read failed, got %d", len(fe.pushes))
	}
}

func TestReconcileSkipsUnchanged(t *testing.T) {
	gcp := secretFile(t, "GCPDATA")
	ucPath := writeUserConfig(t, map[string]any{
		"defaults": map[string]any{"gcp": gcp},
		"repos":    map[string]any{"github.com/acme/widget": map[string]any{"policy": "auto-push"}},
		"trusted":  map[string]any{"github.com/acme/widget": []string{gcpFP}},
	})
	fe := &fakeExecer{
		store:    true,
		manifest: `{"secrets":[{"key":"std:gcp"}]}`,
		hashes:   map[string]string{gcpDest: sha256hex([]byte("GCPDATA"))},
		onStore:  map[string]bool{gcpDest: true}, // already a store symlink
	}
	res, err := Reconcile(context.Background(), fe, "acme/widget", Options{UserConfigPath: ucPath, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed != 0 || res.Skipped != 1 || len(fe.pushes) != 0 {
		t.Errorf("want skip-unchanged: pushed=%d skipped=%d pushes=%d", res.Pushed, res.Skipped, len(fe.pushes))
	}
}

func TestReconcileMigratesPersistentToTmpfs(t *testing.T) {
	// Content matches but the dest is a plain persistent file (onStore=false)
	// while the store now exists → must re-push to migrate onto tmpfs.
	gcp := secretFile(t, "GCPDATA")
	ucPath := writeUserConfig(t, map[string]any{
		"defaults": map[string]any{"gcp": gcp},
		"repos":    map[string]any{"github.com/acme/widget": map[string]any{"policy": "auto-push"}},
		"trusted":  map[string]any{"github.com/acme/widget": []string{gcpFP}},
	})
	fe := &fakeExecer{
		store:    true,
		manifest: `{"secrets":[{"key":"std:gcp"}]}`,
		hashes:   map[string]string{gcpDest: sha256hex([]byte("GCPDATA"))},
		onStore:  map[string]bool{gcpDest: false},
	}
	res, err := Reconcile(context.Background(), fe, "acme/widget", Options{UserConfigPath: ucPath, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed != 1 || len(fe.pushes) != 1 || !fe.pushes[0].tmpfs {
		t.Errorf("want migration push to tmpfs: pushed=%d pushes=%+v", res.Pushed, fe.pushes)
	}
}

func TestReconcileTmpfsFallback(t *testing.T) {
	gcp := secretFile(t, "GCPDATA")
	ucPath := writeUserConfig(t, map[string]any{
		"defaults": map[string]any{"gcp": gcp},
		"repos":    map[string]any{"github.com/acme/widget": map[string]any{"policy": "auto-push"}},
		"trusted":  map[string]any{"github.com/acme/widget": []string{gcpFP}},
	})
	fe := &fakeExecer{store: false, manifest: `{"secrets":[{"key":"std:gcp"}]}`}
	var out bytes.Buffer
	_, err := Reconcile(context.Background(), fe, "acme/widget", Options{UserConfigPath: ucPath, Out: &out, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(fe.pushes) != 1 || fe.pushes[0].tmpfs {
		t.Errorf("store absent → push should be persistent, got %+v", fe.pushes)
	}
	if !strings.Contains(out.String(), "persistent volume") {
		t.Errorf("expected fallback warning:\n%s", out.String())
	}
}

func TestReconcileInteractiveAlwaysPersistsTrust(t *testing.T) {
	gcp := secretFile(t, "GCPDATA")
	ucPath := writeUserConfig(t, map[string]any{
		"defaults": map[string]any{"gcp": gcp},
		"repos":    map[string]any{"github.com/acme/widget": map[string]any{"policy": "ask"}},
	})
	fe := &fakeExecer{store: true, manifest: `{"secrets":[{"key":"std:gcp"}]}`}
	res, err := Reconcile(context.Background(), fe, "acme/widget", Options{
		UserConfigPath: ucPath, In: strings.NewReader("a\n"), Out: &bytes.Buffer{}, Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed != 1 {
		t.Fatalf("want 1 push after 'always', got %d", res.Pushed)
	}
	uc, _ := LoadUserConfig(ucPath)
	if !uc.trustedHas("acme/widget", gcpFP) {
		t.Errorf("'always' should persist the fingerprint; trusted=%v", uc.Trusted)
	}
}

func TestReconcileAutoPushYesRecordsTrust(t *testing.T) {
	// Under auto-push, a plain "y" on first sight should record trust (so it's
	// silent next time) — the bug where auto-push behaved like ask forever.
	gcp := secretFile(t, "GCPDATA")
	ucPath := writeUserConfig(t, map[string]any{
		"defaults": map[string]any{"gcp": gcp},
		"repos":    map[string]any{"github.com/acme/widget": map[string]any{"policy": "auto-push"}},
	})
	fe := &fakeExecer{store: true, manifest: `{"secrets":[{"key":"std:gcp"}]}`}
	res, err := Reconcile(context.Background(), fe, "acme/widget", Options{
		UserConfigPath: ucPath, In: strings.NewReader("y\n"), Out: &bytes.Buffer{}, Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed != 1 {
		t.Fatalf("want 1 push, got %d", res.Pushed)
	}
	uc, _ := LoadUserConfig(ucPath)
	if !uc.trustedHas("acme/widget", gcpFP) {
		t.Errorf("auto-push 'y' should record trust; trusted=%v", uc.Trusted)
	}
}

func TestReconcileAskNonInteractiveSkips(t *testing.T) {
	gcp := secretFile(t, "GCPDATA")
	ucPath := writeUserConfig(t, map[string]any{"defaults": map[string]any{"gcp": gcp}})
	fe := &fakeExecer{store: true, manifest: `{"secrets":[{"key":"std:gcp"}]}`}
	res, err := Reconcile(context.Background(), fe, "acme/widget", Options{UserConfigPath: ucPath, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed != 0 || len(fe.pushes) != 0 {
		t.Errorf("ask + non-interactive should skip, got pushed=%d", res.Pushed)
	}
}

// TestReconcileSkipsInject is the load-bearing invariant: an inject secret
// is NEVER written to the box — no copy dest-write, no env.d file. std:aws,
// std:npm, and std:claude are all inject; even fully mapped + trusted +
// auto-push, the push loop must skip them (zero pushes), while a sibling copy
// secret (std:gcp) in the same manifest still pushes.
func TestReconcileSkipsInject(t *testing.T) {
	awsCreds := secretFile(t, "[default]\naws_access_key_id=AKIA\naws_secret_access_key=SEKRIT\n")
	npmTok := secretFile(t, "npm_token_value\n")
	claudeTok := secretFile(t, "claude_oauth_token\n")
	gcp := secretFile(t, "GCPDATA")
	ucPath := writeUserConfig(t, map[string]any{
		"defaults": map[string]any{
			"aws":    awsCreds,
			"npm":    npmTok,
			"claude": claudeTok,
			"gcp":    gcp,
		},
		"repos": map[string]any{"github.com/acme/widget": map[string]any{"policy": "auto-push"}},
		// Trust every dest the push loop could touch, so the ONLY reason an inject
		// secret isn't pushed is the inject skip — not a missing TOFU approval.
		"trusted": map[string]any{"github.com/acme/widget": []string{gcpFP}},
	})
	fe := &fakeExecer{store: true, manifest: `{"secrets":[
		{"key":"std:aws"},
		{"key":"std:npm"},
		{"key":"std:claude"},
		{"key":"std:gcp"}
	]}`}
	var out bytes.Buffer
	res, err := Reconcile(context.Background(), fe, "acme/widget", Options{UserConfigPath: ucPath, Out: &out, Interactive: false})
	if err != nil {
		t.Fatalf("reconcile: %v\n%s", err, out.String())
	}
	// Exactly one push (gcp); the three inject secrets are skipped entirely.
	if res.Pushed != 1 || len(fe.pushes) != 1 || string(fe.pushes[0].data) != "GCPDATA" {
		t.Fatalf("inject secrets must not be pushed: pushed=%d pushes=%+v\n%s", res.Pushed, fe.pushes, out.String())
	}
	// And no inject secret's bytes appear in ANY push (no env.d file, no dest).
	for _, p := range fe.pushes {
		s := string(p.data)
		for _, leaked := range []string{"AKIA", "SEKRIT", "npm_token_value", "claude_oauth_token", "aws_access_key_id"} {
			if strings.Contains(s, leaked) {
				t.Errorf("inject secret bytes %q were pushed to the box: %q", leaked, s)
			}
		}
	}
}

// slowReader sleeps once before delivering its bytes — a stand-in for a human
// who deliberates at the prompt.
type slowReader struct {
	delay time.Duration
	data  []byte
	slept bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	if !s.slept {
		time.Sleep(s.delay)
		s.slept = true
	}
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	return n, nil
}

// TestReconcileSlowPromptDoesNotPoisonBudget: a human taking longer than the I/O
// budget to answer must NOT cause the approved push to fail — only real box-I/O
// time counts toward the budget, not prompt-wait time. (The fakeExecer now
// returns ctx.Err(), so a regression to a wall-clock budget would fail here.)
func TestReconcileSlowPromptDoesNotPoisonBudget(t *testing.T) {
	old := ioBudget
	ioBudget = 50 * time.Millisecond
	defer func() { ioBudget = old }()

	gcp := secretFile(t, "GCPDATA")
	ucPath := writeUserConfig(t, map[string]any{
		"defaults": map[string]any{"gcp": gcp},
		"repos":    map[string]any{"github.com/acme/widget": map[string]any{"policy": "ask"}},
	})
	fe := &fakeExecer{store: true, manifest: `{"secrets":[{"key":"std:gcp"}]}`}
	res, err := Reconcile(context.Background(), fe, "acme/widget", Options{
		UserConfigPath: ucPath,
		In:             &slowReader{delay: 120 * time.Millisecond, data: []byte("y\n")},
		Out:            &bytes.Buffer{},
		Interactive:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed != 1 {
		t.Fatalf("slow prompt poisoned the I/O budget: pushed=%d (want 1)", res.Pushed)
	}
}

func TestReconcileNoManifestNoop(t *testing.T) {
	ucPath := writeUserConfig(t, map[string]any{})
	fe := &fakeExecer{store: true, manifest: ""}
	res, err := Reconcile(context.Background(), fe, "acme/widget", Options{UserConfigPath: ucPath, Interactive: false})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed != 0 || res.ForwardAgent || len(fe.pushes) != 0 {
		t.Errorf("no manifest → noop, got %+v", res)
	}
}
