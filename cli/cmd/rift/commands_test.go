package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/config"
)

// TestCanonicalRepoFixtures drives canonicalRepo through the checked-in
// fixtures shared with the server's Clojure canonicalizer — the executable
// contract for the grammar. Each row carries the raw input, the pre-resolved
// forge arg, the default-host arg (applied only to host-less bare pairs), and
// the expected canonical string (null → expected reject).
func TestCanonicalRepoFixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "canonical-repo-vectors.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var rows []struct {
		Input     string  `json:"input"`
		Forge     string  `json:"forge"`
		Host      string  `json:"host"`
		Canonical *string `json:"canonical"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("fixtures file decoded to zero rows")
	}
	for _, row := range rows {
		got, err := canonicalRepo(row.Input, row.Forge, row.Host)
		if row.Canonical == nil {
			if err == nil {
				t.Errorf("canonicalRepo(%q, %q, %q) = %q, want reject", row.Input, row.Forge, row.Host, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("canonicalRepo(%q, %q, %q): %v, want %q", row.Input, row.Forge, row.Host, err, *row.Canonical)
			continue
		}
		if got != *row.Canonical {
			t.Errorf("canonicalRepo(%q, %q, %q) = %q, want %q", row.Input, row.Forge, row.Host, got, *row.Canonical)
			continue
		}
		// Fixed point: every canonical string the CLI emits must survive a
		// round trip unchanged — a non-fixed-point output is exactly the
		// string the server ingress would 400.
		again, err := canonicalRepo(*row.Canonical, row.Forge, row.Host)
		if err != nil {
			t.Errorf("canonicalRepo(%q, %q, %q) (fixed point): %v", *row.Canonical, row.Forge, row.Host, err)
			continue
		}
		if again != *row.Canonical {
			t.Errorf("canonicalRepo(%q, %q, %q) = %q, want the fixed point %q", *row.Canonical, row.Forge, row.Host, again, *row.Canonical)
		}
	}
}

// The rejection message is pinned by the design (the CLI surfaces every
// canonicalizer reject with this exact wording).
func TestCanonicalRepoRejectionMessage(t *testing.T) {
	_, err := canonicalRepo("widget", "github", "github.com")
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
		// still errors in canonicalRepo (GHES is deferred).
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
			got, err := resolveRepoIdentity(c.in, c.forge)
			if c.want == "" {
				if err == nil {
					t.Fatalf("resolveRepoIdentity(%q, %q) = %q, want error", c.in, c.forge, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRepoIdentity(%q, %q): %v", c.in, c.forge, err)
			}
			if got != c.want {
				t.Fatalf("resolveRepoIdentity(%q, %q) = %q, want %q", c.in, c.forge, got, c.want)
			}
		})
	}
}

func TestMachineTarget(t *testing.T) {
	if id, err := machineTarget("ws-self", nil); err != nil || id != "ws-self" {
		t.Fatalf("no args: id=%q err=%v", id, err)
	}
	if id, err := machineTarget("ws-self", []string{"ws-self"}); err != nil || id != "ws-self" {
		t.Fatalf("own id: id=%q err=%v", id, err)
	}
	if _, err := machineTarget("ws-self", []string{"ws-other"}); err == nil {
		t.Fatal("foreign id must be rejected in-VM")
	}
}

func TestExplainCreateNamesCanonicalRepo(t *testing.T) {
	err := explainCreate(&client.APIError{Status: 409, Body: `{"error":"image-not-ready"}`}, "org/name")
	if err == nil || !strings.Contains(err.Error(), "org/name") {
		t.Fatalf("409 message must name the canonical repo it looked up: %v", err)
	}
	// 503 with a non-image error code falls through to the status-based message.
	err = explainCreate(&client.APIError{Status: 503, Body: `{"error":"no-ready-relay"}`}, "org/name")
	if err == nil || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("503 message: %v", err)
	}
}

func TestExplainCreateBranchesOnBodyCode(t *testing.T) {
	// image-not-ready-for-ref must list the available built refs.
	err := explainCreate(&client.APIError{
		Status: 409,
		Body:   `{"error":"image-not-ready-for-ref","available_refs":["refs/heads/main","refs/heads/dev"]}`,
	}, "org/name")
	if err == nil || !strings.Contains(err.Error(), "refs/heads/main") || !strings.Contains(err.Error(), "refs/heads/dev") {
		t.Fatalf("image-not-ready-for-ref must list available refs: %v", err)
	}

	// ambiguous-image must list the candidate commits.
	err = explainCreate(&client.APIError{
		Status: 409,
		Body:   `{"error":"ambiguous-image","candidates":["abc123def","abc123aaa"]}`,
	}, "org/name")
	if err == nil || !strings.Contains(err.Error(), "abc123def") || !strings.Contains(err.Error(), "abc123aaa") {
		t.Fatalf("ambiguous-image must list candidates: %v", err)
	}

	// image-not-found (404) → its own message.
	err = explainCreate(&client.APIError{Status: 404, Body: `{"error":"image-not-found"}`}, "org/name")
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("image-not-found message: %v", err)
	}

	// image-prefix-too-short (400) → its own message.
	err = explainCreate(&client.APIError{Status: 400, Body: `{"error":"image-prefix-too-short"}`}, "org/name")
	if err == nil || !strings.Contains(err.Error(), "7") {
		t.Fatalf("image-prefix-too-short message: %v", err)
	}
}

// --- describeResolved renders the `new` success line ---
//
// Arms covered:
//   - ref-present       → "<ref> @ <short12>"
//   - --image (no ref)  → bare short commit (resolved_ref empty)
//   - 12-char SHA truncation (a >12-char commit is cut to its first 12)
//   - fallback suffix   → "(fell back to default branch)" appended when set
func TestDescribeResolved(t *testing.T) {
	cases := []struct {
		name string
		in   client.CreateResult
		want string
	}{
		{
			name: "ref-present-short-commit",
			in:   client.CreateResult{ResolvedRef: "refs/heads/main", ResolvedCommit: "abc123"},
			want: "refs/heads/main @ abc123",
		},
		{
			name: "ref-present-truncates-long-commit",
			// 40-char SHA → only the first 12 chars are shown after the ref.
			in:   client.CreateResult{ResolvedRef: "refs/heads/dev", ResolvedCommit: "0123456789abcdef0123456789abcdef01234567"},
			want: "refs/heads/dev @ 0123456789ab",
		},
		{
			name: "image-spawn-bare-commit",
			// --image spawn: resolved_ref is empty, so just the (truncated) commit.
			in:   client.CreateResult{ResolvedRef: "", ResolvedCommit: "0123456789abcdef0123456789abcdef01234567"},
			want: "0123456789ab",
		},
		{
			name: "image-spawn-short-commit-not-truncated",
			in:   client.CreateResult{ResolvedRef: "", ResolvedCommit: "abc123"},
			want: "abc123",
		},
		{
			name: "fallback-suffix",
			in:   client.CreateResult{ResolvedRef: "refs/heads/main", ResolvedCommit: "abc123", Fallback: true},
			want: "refs/heads/main @ abc123 (fell back to default branch)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.in
			if got := describeResolved(&r); got != c.want {
				t.Fatalf("describeResolved(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// --- cmdNew rejects --ref and --image together (mutual-exclusion guard) ---
//
// The guard fires right after flag parsing, before authedClient()/git, so it's
// driven purely through the flag-parsing path. A nil ctx is safe: the guard
// returns before ctx is ever used.
func TestCmdNewRefAndImageMutuallyExclusive(t *testing.T) {
	err := cmdNew(context.Background(), []string{"--ref", "main", "--image", "deadbeef"})
	if err == nil {
		t.Fatal("--ref and --image together must error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutual-exclusion error, got %v", err)
	}
}

func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"main":                 "refs/heads/main",
		"feature-x":            "refs/heads/feature-x",
		"refs/heads/main":      "refs/heads/main",
		"refs/heads/feature-x": "refs/heads/feature-x",
		"  main  ":             "refs/heads/main",
	}
	for in, want := range cases {
		if got := normalizeRef(in); got != want {
			t.Errorf("normalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- helpers for the cmd* command tests ---
//
// The cmd* functions read config from disk (authedClient → config.FromEnvOrFile)
// and build a real client against cfg.APIBaseURL. hermeticEnv points config at a
// temp dir and returns a mock server so authedClient's client talks to it; the
// caller seeds whatever config it needs (APIBaseURL := the server) with
// seedConfig. Both XDG_CONFIG_HOME and HOME are set so os.UserConfigDir resolves
// to the temp dir on Linux and macOS alike — the tests need read/write
// consistency, not a specific path. RIFT_* env is cleared to force laptop mode
// (no machine-token override) and a deterministic --api default.

func hermeticEnv(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	d := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", d)
	t.Setenv("HOME", d)
	t.Setenv("RIFT_WORKSPACE_ID", "")
	t.Setenv("RIFT_API_URL", "")
	t.Setenv("RIFT_TOKEN", "")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func seedConfig(t *testing.T, c *config.Config) {
	t.Helper()
	if err := c.Save(); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return c
}

// withStdin points os.Stdin at a temp file holding `input` for the duration of
// the test (the no-arg picker reads a line from os.Stdin).
func withStdin(t *testing.T, input string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatalf("stdin temp: %v", err)
	}
	if _, err := f.WriteString(input); err != nil {
		t.Fatalf("stdin write: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("stdin seek: %v", err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; _ = f.Close() })
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns whatever
// fn printed (these callers observe rendered output rather than an internal value).
// captureStderr does the same for os.Stderr (advisory pre-flight warnings print
// there, so they never mix with parseable stdout).
func captureStdout(t *testing.T, fn func()) string { return captureFile(t, &os.Stdout, fn) }
func captureStderr(t *testing.T, fn func()) string { return captureFile(t, &os.Stderr, fn) }

func captureFile(t *testing.T, f **os.File, fn func()) string {
	t.Helper()
	old := *f
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	*f = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	*f = old
	return <-done
}

// --- cmdNew billing context is repo-derived (no context input) ---
//
// The happy path calls connect() after Create; the mock captures the Create
// body (which runs first), then answers the connect-time Get with a "failed"
// workspace so waitRunning returns promptly and cmdNew unwinds without a live
// box. We assert the create body carries NO context_id (the server derives it
// from the repo). --repo is passed so no git remote is inferred; --ref avoids
// the branch inference shell-out.

func newCaptureHandler(createHit *bool, gotBody *map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/workspaces" {
			*createHit = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*gotBody = body
			_ = json.NewEncoder(w).Encode(map[string]any{"workspace_id": "ws-new"})
			return
		}
		// connect's waitRunning Get → a terminal "failed" status returns fast.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspace": map[string]any{"workspace_id": "ws-new", "status": "failed", "error_message": "test-stop"},
		})
	}
}

// A plain `new` sends no context_id — the billing context is derived
// server-side from the repo.
func TestCmdNewSendsNoContextID(t *testing.T) {
	var createHit bool
	var gotBody map[string]any
	srv := hermeticEnv(t, newCaptureHandler(&createHit, &gotBody))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	_ = captureStdout(t, func() {
		_ = cmdNew(context.Background(), []string{"--repo", "org/app", "--ref", "main"})
	})
	if !createHit {
		t.Fatal("Create was not called")
	}
	if _, present := gotBody["context_id"]; present {
		t.Fatalf("create body must not carry a context_id (server derives it from repo): %+v", gotBody)
	}
	if gotBody["repo"] != "github:github.com/org/app" {
		t.Fatalf("create body must carry the canonical repo: %+v", gotBody)
	}
}

// --- cmdNew --forge plumbing (command level) ---
//
// Unit tables pin resolveRepoIdentity itself; these drive cmdNew's actual
// flagset so a --forge mis-registration ("flag provided but not defined") or
// a dropped pass-through into resolveRepo cannot hide. Same harness as the
// context tests: --repo avoids the git-remote inference, --ref the branch
// shell-out, and the connect-time Get answers "failed" so cmdNew unwinds.

func TestCmdNewForgeFlagResolvesCanonicalRepo(t *testing.T) {
	var createHit bool
	var gotRepo string
	srv := hermeticEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/workspaces" {
			createHit = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotRepo, _ = body["repo"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"workspace_id": "ws-new"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspace": map[string]any{"workspace_id": "ws-new", "status": "failed", "error_message": "test-stop"},
		})
	})
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	_ = captureStdout(t, func() {
		_ = cmdNew(context.Background(), []string{"--repo", "acme/widget", "--forge", "github", "--ref", "main"})
	})
	if !createHit {
		t.Fatal("Create was not called")
	}
	if gotRepo != "github:github.com/acme/widget" {
		t.Fatalf("body repo = %q, want the canonical github:github.com/acme/widget", gotRepo)
	}
}

func TestCmdNewForgeConflictSurfacesThroughCommand(t *testing.T) {
	var createHit bool
	srv := hermeticEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/workspaces" {
			createHit = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	// --forge gitlab against a github.com --repo → the flow-1 conflict error
	// must surface through the command, before any Create.
	err := cmdNew(context.Background(), []string{"--repo", "https://github.com/acme/widget", "--forge", "gitlab"})
	if err == nil {
		t.Fatal("a --forge conflicting with a SaaS host must error through the command")
	}
	if !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("want the flow-1 conflict error, got %v", err)
	}
	if createHit {
		t.Fatal("Create must NOT be called on a forge conflict")
	}
}

// --- cmdList is owner-scoped; there is no context filter ---

func listHandler(items []client.ListItem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspaces" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": items})
	}
}

var listRows = []client.ListItem{
	{WorkspaceID: "ws-acme", Status: "running", Repo: "org/a", Context: "company:c1"},
	{WorkspaceID: "ws-beta", Status: "running", Repo: "org/b", Context: "company:c2"},
}

// A bare `ls` renders every workspace the caller owns, across all contexts they
// can see — there is no context filter to narrow it.
func TestCmdListShowsAllOwnedWorkspaces(t *testing.T) {
	srv := hermeticEnv(t, listHandler(listRows))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdList(context.Background(), nil); err != nil {
			t.Fatalf("cmdList: %v", err)
		}
	})
	if !strings.Contains(out, "ws-acme") || !strings.Contains(out, "ws-beta") {
		t.Fatalf("ls must render all owned boxes across contexts, got:\n%s", out)
	}
}

// The retired --context flag is now an unknown flag: it errors at parse rather
// than silently filtering.
func TestCmdListContextFlagRetired(t *testing.T) {
	srv := hermeticEnv(t, listHandler(listRows))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	_ = captureStderr(t, func() { // fs.Parse prints its own usage on error
		if err := cmdList(context.Background(), []string{"--context", "company:c1"}); err == nil {
			t.Fatal("--context must be a retired/unknown flag now")
		}
	})
}

// --- cmdLogin: login persists only APIBaseURL + Token ---
//
// There is no per-device context to seed or preserve.

func TestCmdLoginPersistsOnlyURLAndToken(t *testing.T) {
	srv := hermeticEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login/device/start":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "DC", "user_code": "ABCD-1234",
				"verification_url": "https://api/activate", "interval": 1,
			})
		case "/api/login/device/poll":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted"})
		default:
			http.NotFound(w, r)
		}
	})
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "old"})

	out := captureStdout(t, func() {
		if err := cmdLogin(context.Background(), []string{"--api", srv.URL}); err != nil {
			t.Fatalf("cmdLogin: %v", err)
		}
	})
	got := loadConfig(t)
	if got.Token != "minted" {
		t.Fatalf("login must persist the minted token, got %q", got.Token)
	}
	if got.APIBaseURL != srv.URL {
		t.Fatalf("login must persist the API base URL, got %q", got.APIBaseURL)
	}

	// Printed-URL fallback: the always-present invariant. The captured stdout
	// must carry the server's ACTUAL verification_url (whatever the hermetic
	// handler returns above) plus the "Waiting for approval" line — the printed
	// URL is the guaranteed way to log in regardless of auto-open. Reference the
	// handler's value; do NOT hardcode a prod URL.
	const wantURL = "https://api/activate"
	if !strings.Contains(out, wantURL) {
		t.Fatalf("stdout must print the server's verification_url %q, got:\n%s", wantURL, out)
	}
	if !strings.Contains(out, "Waiting for approval") {
		t.Fatalf("stdout must print the Waiting-for-approval fallback line, got:\n%s", out)
	}
}

// loginHappyHandler is the happy-path device flow: start returns a fixed
// verification_url + user_code, poll immediately mints a token. Shared by the
// auto-open-suppression sibling below.
func loginHappyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login/device/start":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "DC", "user_code": "ABCD-1234",
				"verification_url": "https://api/activate", "interval": 1,
			})
		case "/api/login/device/poll":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted"})
		default:
			http.NotFound(w, r)
		}
	}
}

// --- cmdLogin never auto-opens the browser in a non-TTY ---
//
// The test captures stdout, so os.Stdout is a pipe → term.IsTerminal is false →
// cmdLogin's `interactive` is false, and shouldAutoOpen short-circuits at
// !interactive BEFORE it ever consults --no-browser. So the openBrowser var is
// guaranteed un-invoked by the interactive gate alone.
//
// Honest scope (per the plan): this exercises only the SUPPRESSED direction. Its
// real signal is a regression guard against someone hoisting the openBrowser
// call ABOVE the interactive gate; the --no-browser repeat's only added signal
// is that a known flag parses without error. It is NOT proof that --no-browser
// suppresses the browser — that decision is covered non-vacuously by
// TestShouldAutoOpen. No t.Parallel: this swaps the openBrowser package var.
func TestCmdLoginNoAutoOpenInNonTTY(t *testing.T) {
	// Swap the openBrowser package var for a recorder; restore on cleanup.
	var opened int
	orig := openBrowser
	openBrowser = func(string) error { opened++; return nil }
	t.Cleanup(func() { openBrowser = orig })

	run := func(name string, args []string) {
		opened = 0
		srv := hermeticEnv(t, loginHappyHandler())
		full := append([]string{"--api", srv.URL}, args...)
		_ = captureStdout(t, func() {
			if err := cmdLogin(context.Background(), full); err != nil {
				t.Fatalf("%s: cmdLogin: %v", name, err)
			}
		})
		if opened != 0 {
			t.Fatalf("%s: openBrowser must NOT fire in a non-TTY (called %d times)", name, opened)
		}
	}

	// Default (no flag) and with --no-browser: both parse and neither fires.
	run("default", nil)
	run("no-browser", []string{"--no-browser"})
}

// --- cmdLogin: a fatal poll error is wrapped and writes no config ---
//
// A 400 on /api/login/device/poll is terminal in DevicePoll (any non-2xx that
// isn't 204/202 → *APIError), surfaced through PollUntilToken to cmdLogin, which
// wraps it as fmt.Errorf("login: %w", err). cmdLogin only Load()s/Save()s config
// AFTER a successful poll, so on this error the pre-seeded config must be left
// byte-for-byte untouched — no partial write.
func TestCmdLoginFatalPollErrorNoConfigWrite(t *testing.T) {
	srv := hermeticEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login/device/start":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "DC", "user_code": "ABCD-1234",
				"verification_url": "https://api/activate", "interval": 1,
			})
		case "/api/login/device/poll":
			// Terminal: 400 with an error body → DevicePoll returns a terminal
			// *APIError (not ErrAuthPending), which is fatal for PollUntilToken.
			http.Error(w, `{"error":"expired_token"}`, http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	})
	// Pre-seed a full session the failed login must not touch.
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "preseeded-tok"})

	var err error
	_ = captureStdout(t, func() {
		err = cmdLogin(context.Background(), []string{"--api", srv.URL})
	})
	if err == nil {
		t.Fatal("a fatal poll error must surface as a non-nil error")
	}
	// The fmt.Errorf("login: %w", ...) wrap — a regression that drops it makes
	// main mis-render the failure.
	if !strings.Contains(err.Error(), "login:") {
		t.Fatalf("error must carry the login: wrap, got %v", err)
	}
	// No partial write: the pre-seeded token survives untouched.
	got := loadConfig(t)
	if got.Token != "preseeded-tok" {
		t.Fatalf("failed login must NOT overwrite the token, got %q", got.Token)
	}
}

// --- cmdLogin: an unknown flag is a parse error and mints nothing ---
//
// The flagset uses ContinueOnError, so an undefined flag fails at fs.Parse
// before any device call. The hermetic env isolates config; nothing may be
// written.
func TestCmdLoginUnknownFlagParseError(t *testing.T) {
	_ = hermeticEnv(t, func(w http.ResponseWriter, r *http.Request) {
		// Any request would be a bug: parsing fails before the device flow.
		t.Errorf("no request expected on a parse error, got %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	var err error
	_ = captureStdout(t, func() {
		err = cmdLogin(context.Background(), []string{"--nope"})
	})
	if err == nil {
		t.Fatal("an unknown flag must be a parse error")
	}
	// No token minted: config stays empty (no seed, no write).
	if got := loadConfig(t).Token; got != "" {
		t.Fatalf("a parse error must mint no token, got %q", got)
	}
}

// --- cmdNew force-select (region-required / size-required) ---
//
// The server never substitutes a missing region/size — it 400s
// {"error":"<dim>-required","selectable":[…]}. On a TTY cmdNew runs a
// numbered picker over the selectable list and re-issues the create; non-TTY
// it lists the values and exits non-zero.

// forceTTY forces cmdNew's interactive gate for the duration of the test. The
// harness's stdin/stdout are files/pipes — never real terminals — so the true
// term.IsTerminal gate cannot fire here; the gate's decision fn is the
// package var isTTY, and the picker itself is unit-tested over an injected
// reader (TestPickerPrompt*). No t.Parallel in tests using this: it swaps a
// package var.
func forceTTY(t *testing.T, v bool) {
	t.Helper()
	orig := isTTY
	isTTY = func() bool { return v }
	t.Cleanup(func() { isTTY = orig })
}

// forceSelectHandler 400s a create missing region (then size) with the
// force-select codes, succeeds once both are present (echoing them back as
// explicit), and answers the connect-time Get with a terminal "failed"
// workspace so cmdNew unwinds without a live box (the newCaptureHandler
// pattern).
func forceSelectHandler(bodies *[]map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/workspaces" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*bodies = append(*bodies, body)
			if _, ok := body["region"]; !ok {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "region-required", "detail": "no default region for this context",
					"selectable": []string{"iad", "ewr"}})
				return
			}
			if _, ok := body["size"]; !ok {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "size-required", "detail": "no default size for this context",
					"selectable": []string{"shared-2x", "shared-4x"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workspace_id": "ws-new",
				"region":       body["region"], "region_source": "explicit",
				"size": body["size"], "size_source": "explicit",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspace": map[string]any{"workspace_id": "ws-new", "status": "failed", "error_message": "test-stop"},
		})
	}
}

// Both dimensions missing → two picker rounds, each re-issuing the create
// with the pick filled in (3 attempts total), then the success line echoes
// both resolved dimensions. The stdin carries a garbage first entry to prove
// the picker re-prompts, and one shared reader must survive both rounds.
func TestCmdNewForceSelectPickerLoop(t *testing.T) {
	var bodies []map[string]any
	srv := hermeticEnv(t, forceSelectHandler(&bodies))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})
	forceTTY(t, true)
	// Region picker: "abc" (re-prompt) then "2" → ewr; size picker: "1" →
	// shared-2x.
	withStdin(t, "abc\n2\n1\n")

	out := captureStdout(t, func() {
		_ = cmdNew(context.Background(), []string{"--repo", "org/app", "--ref", "main"})
	})
	if len(bodies) != 3 {
		t.Fatalf("want 3 create attempts (blank, +region, +size), got %d", len(bodies))
	}
	if _, ok := bodies[0]["region"]; ok {
		t.Fatalf("attempt 1 must omit region: %+v", bodies[0])
	}
	if bodies[1]["region"] != "ewr" {
		t.Fatalf("attempt 2 must carry the picked region (ewr): %+v", bodies[1])
	}
	if _, ok := bodies[1]["size"]; ok {
		t.Fatalf("attempt 2 must still omit size: %+v", bodies[1])
	}
	if bodies[2]["region"] != "ewr" || bodies[2]["size"] != "shared-2x" {
		t.Fatalf("attempt 3 must carry BOTH picks: %+v", bodies[2])
	}
	// The picker rendered the numbered selectable list…
	if !strings.Contains(out, "1) iad") || !strings.Contains(out, "2) ewr") {
		t.Fatalf("picker must render the numbered selectable list, got:\n%s", out)
	}
	// …and a success line echoes both dimensions with their sources. Only
	// presence is asserted — the exact copy is pinned ONCE, in
	// TestDescribeSpawnDefaults. The picker lists above also print the bare
	// values (one per numbered line), so the assertion must find a SINGLE
	// line carrying both values plus a source wording for each.
	echoed := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "ewr") && strings.Contains(line, "shared-2x") &&
			strings.Count(line, "explicit") >= 2 {
			echoed = true
			break
		}
	}
	if !echoed {
		t.Fatalf("success output must echo both dimensions with their sources on one line, got:\n%s", out)
	}
}

// An empty line (or EOF) at the picker aborts without re-issuing the create.
func TestCmdNewForceSelectAbortsOnEmptyStdin(t *testing.T) {
	var bodies []map[string]any
	srv := hermeticEnv(t, forceSelectHandler(&bodies))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})
	forceTTY(t, true)
	withStdin(t, "\n")

	var err error
	_ = captureStdout(t, func() {
		err = cmdNew(context.Background(), []string{"--repo", "org/app", "--ref", "main"})
	})
	if err == nil || !strings.Contains(err.Error(), "no region selected") {
		t.Fatalf("empty picker input must abort, got %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("an aborted picker must not re-issue the create, got %d attempts", len(bodies))
	}
}

// Non-TTY (CI): no picker — the detail + selectable list surface in the error
// (main prints it to stderr, exit non-zero) and the create is never retried.
// The value is NEVER substituted.
func TestCmdNewForceSelectNonTTYListsAndFails(t *testing.T) {
	var bodies []map[string]any
	srv := hermeticEnv(t, forceSelectHandler(&bodies))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})
	forceTTY(t, false)

	err := cmdNew(context.Background(), []string{"--repo", "org/app", "--ref", "main"})
	if err == nil {
		t.Fatal("non-TTY force-select must fail (never substitute)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no default region for this context") ||
		!strings.Contains(msg, "iad") || !strings.Contains(msg, "ewr") {
		t.Fatalf("non-TTY error must carry the detail + selectable list, got %q", msg)
	}
	if !strings.Contains(msg, "--region") {
		t.Fatalf("non-TTY error should name the flag to re-run with, got %q", msg)
	}
	if len(bodies) != 1 {
		t.Fatalf("non-TTY must not retry the create, got %d attempts", len(bodies))
	}
}

// A <dim>-required 400 whose selectable list is EMPTY never opens a picker —
// there is nothing to pick — even on a TTY: the server's detail surfaces as
// the listing error (non-zero exit), the create is never re-issued, and the
// value is never substituted. The empty-list gate is dimension-agnostic
// (one code path for both); region stands in for both dimensions.
func TestCmdNewForceSelectEmptySelectableFailsEvenOnTTY(t *testing.T) {
	createCalls := 0
	srv := hermeticEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/workspaces" {
			createCalls++
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "region-required", "detail": "no default region for this context",
				"selectable": []string{}})
			return
		}
		http.NotFound(w, r)
	})
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})
	forceTTY(t, true)   // even interactively…
	withStdin(t, "1\n") // …an empty list must never reach a picker read

	var err error
	out := captureStdout(t, func() {
		err = cmdNew(context.Background(), []string{"--repo", "org/app", "--ref", "main"})
	})
	if err == nil {
		t.Fatal("an empty selectable list must fail even on a TTY (nothing to pick)")
	}
	if !strings.Contains(err.Error(), "no default region for this context") {
		t.Fatalf("error must carry the server's detail, got %v", err)
	}
	if !strings.Contains(err.Error(), "--region") {
		t.Fatalf("error should name the flag to re-run with, got %v", err)
	}
	if createCalls != 1 {
		t.Fatalf("an empty list must never re-issue the create, got %d attempts", createCalls)
	}
	// No numbered picker rendered — there was nothing to pick from.
	if strings.Contains(out, "1)") || strings.Contains(out, "Select a") {
		t.Fatalf("no picker may render on an empty selectable list, got:\n%s", out)
	}
}

// An EXPLICIT --size the server rejects (…-not-available) keeps the
// fail-with-list behavior — no picker even on a TTY, no re-issue, never a
// substitution.
func TestCmdNewExplicitNotAvailableFailsWithList(t *testing.T) {
	createCalls := 0
	srv := hermeticEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/workspaces" {
			createCalls++
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "size-not-available", "detail": "size not available",
				"selectable": []string{"shared-2x", "shared-4x"}})
			return
		}
		http.NotFound(w, r)
	})
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})
	forceTTY(t, true)   // even interactively…
	withStdin(t, "1\n") // …an explicit-invalid must NOT consume a picker entry

	var err error
	_ = captureStdout(t, func() {
		err = cmdNew(context.Background(), []string{"--repo", "org/app", "--ref", "main", "--size", "mega-999x"})
	})
	if err == nil {
		t.Fatal("an explicit invalid --size must fail")
	}
	if !strings.Contains(err.Error(), "size not available") || !strings.Contains(err.Error(), "shared-4x") {
		t.Fatalf("error must carry the detail + selectable list, got %v", err)
	}
	if createCalls != 1 {
		t.Fatalf("explicit-invalid must never re-issue, got %d attempts", createCalls)
	}
}

// The infinite-loop guard: a server that keeps 400ing <dim>-required for a
// dimension the picker already filled in is surfaced as an error after ONE
// re-issue, not looped forever.
func TestCmdNewForceSelectGuardsRepeatedRequired(t *testing.T) {
	createCalls := 0
	srv := hermeticEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/workspaces" {
			createCalls++
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "region-required", "detail": "no default region for this context",
				"selectable": []string{"iad"}})
			return
		}
		http.NotFound(w, r)
	})
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})
	forceTTY(t, true)
	withStdin(t, "1\n1\n1\n1\n") // more input than the guard should ever consume

	var err error
	_ = captureStdout(t, func() {
		err = cmdNew(context.Background(), []string{"--repo", "org/app", "--ref", "main"})
	})
	if err == nil {
		t.Fatal("a re-reported required dimension must surface an error")
	}
	if createCalls != 2 {
		t.Fatalf("guard must stop after one re-issue, got %d attempts", createCalls)
	}
}

// --- pickerPrompt (the numbered force-select picker, unit level) ---
//
// Driven over an injected bufio.Reader — no TTY needed (the TTY gate is the
// caller's isTTY check, exercised above via forceTTY). Garbage (a non-number
// or out-of-range entry) re-prompts; an empty line or EOF aborts.

func TestPickerPromptRepromptsOnGarbageThenPicks(t *testing.T) {
	se := client.SelectableError{Detail: "pick one", Selectable: []string{"iad", "ewr"}}
	in := bufio.NewReader(strings.NewReader("abc\n99\n0\n2\n"))
	var choice string
	var err error
	out := captureStdout(t, func() { choice, err = pickerPrompt(in, "region", se) })
	if err != nil || choice != "ewr" {
		t.Fatalf("choice=%q err=%v, want ewr", choice, err)
	}
	if !strings.Contains(out, "pick one") || !strings.Contains(out, "1) iad") || !strings.Contains(out, "2) ewr") {
		t.Fatalf("picker must render detail + numbered list, got:\n%s", out)
	}
	// Three invalid entries → three re-prompts after the initial prompt.
	if got := strings.Count(out, "Select a region"); got != 4 {
		t.Fatalf("want 4 prompts (1 + 3 re-prompts), got %d:\n%s", got, out)
	}
}

func TestPickerPromptAborts(t *testing.T) {
	cases := map[string]string{
		"empty-line":       "\n",
		"immediate-eof":    "",
		"garbage-then-eof": "abc", // no trailing newline; the re-prompt hits EOF
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			se := client.SelectableError{Detail: "pick one", Selectable: []string{"iad"}}
			in := bufio.NewReader(strings.NewReader(input))
			var err error
			_ = captureStdout(t, func() { _, err = pickerPrompt(in, "region", se) })
			if err == nil {
				t.Fatalf("input %q must abort the picker", input)
			}
		})
	}
}

// --- set-default-region / set-default-size / set-repo-builder-size ---

// settingsHits counts the advisory pre-flight catalog reads
// settingsCaptureHandler serves: GET /api/regions (the region dimension) and
// GET /api/workspaces/sizes (the size dimension).
type settingsHits struct {
	regions int
	sizes   int
}

// settingsCaptureHandler captures POST /api/devbox-settings and POST
// /api/repos/builder-size bodies into byPath, answers the two advisory
// pre-flight catalog reads (GET /api/regions with slug "iad", GET
// /api/workspaces/sizes with id "shared-4x"), and counts those reads in hits.
func settingsCaptureHandler(byPath map[string]map[string]any, hits *settingsHits) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost &&
			(r.URL.Path == "/api/devbox-settings" || r.URL.Path == "/api/repos/builder-size"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			byPath[r.URL.Path] = body
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/regions":
			hits.regions++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"effective_default": nil, "pinned_default": nil,
				"regions": []map[string]any{
					{"slug": "iad", "display_name": "Ashburn", "status": "available", "available_now": true},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/sizes":
			hits.sizes++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"effective_default": nil,
				"sizes": []map[string]any{
					{"id": "shared-4x", "display_name": "Shared 4x", "cpu": 4, "memory_mb": 8192, "price": "—"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}
}

// A --repo write canonicalizes the repo and sends it as the sole scope key —
// no context_id (the server derives + owner/admin-gates the owning context) —
// and keeps the region advisory pre-flight.
func TestCmdSetDefaultRegionRepoScopedBody(t *testing.T) {
	byPath := map[string]map[string]any{}
	var hits settingsHits
	srv := hermeticEnv(t, settingsCaptureHandler(byPath, &hits))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdSetDefaultRegion(context.Background(), []string{"--repo", "Acme/Widget", "iad"}); err != nil {
			t.Fatalf("cmdSetDefaultRegion: %v", err)
		}
	})
	body := byPath["/api/devbox-settings"]
	if body == nil {
		t.Fatal("no POST /api/devbox-settings captured")
	}
	if body["repo"] != "github:github.com/acme/widget" ||
		body["setting"] != "default-region" || body["value"] != "iad" {
		t.Fatalf("body: %+v", body)
	}
	if _, present := body["context_id"]; present {
		t.Fatalf("the write must carry no context_id (server derives it from repo): %+v", body)
	}
	if _, present := body["clear"]; present {
		t.Fatalf("a plain set must not send clear: %+v", body)
	}
	if hits.regions != 1 {
		t.Fatalf("region set keeps the advisory pre-flight (1 GET /api/regions), got %d", hits.regions)
	}
	if hits.sizes != 0 {
		t.Fatalf("region set must not read /api/workspaces/sizes, got %d", hits.sizes)
	}
	if !strings.Contains(out, "Default region set to iad") {
		t.Fatalf("confirmation output, got:\n%s", out)
	}
}

// A settings write REQUIRES a repo (the server 400s a no-repo write): with
// --repo absent the CLI fails fast, before any HTTP request.
func TestCmdSetDefaultRegionNoRepoRequired(t *testing.T) {
	byPath := map[string]map[string]any{}
	var hits settingsHits
	srv := hermeticEnv(t, settingsCaptureHandler(byPath, &hits))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	var err error
	_ = captureStdout(t, func() {
		err = cmdSetDefaultRegion(context.Background(), []string{"iad"})
	})
	if err == nil {
		t.Fatal("no --repo must error")
	}
	if !strings.Contains(err.Error(), "--repo is required") {
		t.Fatalf("error should name the missing --repo, got: %v", err)
	}
	if len(byPath) != 0 || hits.regions != 0 || hits.sizes != 0 {
		t.Fatalf("no request must be made before the --repo check: byPath=%+v hits=%+v", byPath, hits)
	}
}

// --clear (or no value argument) sends clear:true and omits value — but only
// with a repo, which a settings write requires.
func TestCmdSetDefaultRegionClear(t *testing.T) {
	for name, args := range map[string][]string{
		"explicit-clear": {"--repo", "Acme/Widget", "--clear"},
		"no-arg":         {"--repo", "Acme/Widget"},
	} {
		t.Run(name, func(t *testing.T) {
			byPath := map[string]map[string]any{}
			var hits settingsHits
			srv := hermeticEnv(t, settingsCaptureHandler(byPath, &hits))
			seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

			out := captureStdout(t, func() {
				if err := cmdSetDefaultRegion(context.Background(), args); err != nil {
					t.Fatalf("cmdSetDefaultRegion: %v", err)
				}
			})
			body := byPath["/api/devbox-settings"]
			if body["repo"] != "github:github.com/acme/widget" {
				t.Fatalf("clear must carry the repo scope: %+v", body)
			}
			if v, present := body["clear"]; !present || v != true {
				t.Fatalf("clear must send clear:true: %+v", body)
			}
			if _, present := body["value"]; present {
				t.Fatalf("clear must omit value: %+v", body)
			}
			if !strings.Contains(out, "cleared") {
				t.Fatalf("confirmation output, got:\n%s", out)
			}
		})
	}
}

// A no-repo clear (explicit --clear or bare no-arg) also fails fast with the
// --repo-required error, before any HTTP request — a no-repo settings write is
// unsupported.
func TestCmdSetDefaultRegionClearNoRepoRequired(t *testing.T) {
	for name, args := range map[string][]string{
		"explicit-clear": {"--clear"},
		"no-arg":         {},
	} {
		t.Run(name, func(t *testing.T) {
			byPath := map[string]map[string]any{}
			var hits settingsHits
			srv := hermeticEnv(t, settingsCaptureHandler(byPath, &hits))
			seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

			var err error
			_ = captureStdout(t, func() {
				err = cmdSetDefaultRegion(context.Background(), args)
			})
			if err == nil || !strings.Contains(err.Error(), "--repo is required") {
				t.Fatalf("no --repo must error with --repo required, got: %v", err)
			}
			if len(byPath) != 0 {
				t.Fatalf("no request must be made before the --repo check: %+v", byPath)
			}
		})
	}
}

// set-default-size posts setting "default-size" with its own advisory
// pre-flight (GET /api/workspaces/sizes) — and never the region one.
func TestCmdSetDefaultSizeBody(t *testing.T) {
	byPath := map[string]map[string]any{}
	var hits settingsHits
	srv := hermeticEnv(t, settingsCaptureHandler(byPath, &hits))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdSetDefaultSize(context.Background(), []string{"--repo", "Acme/Widget", "shared-4x"}); err != nil {
			t.Fatalf("cmdSetDefaultSize: %v", err)
		}
	})
	body := byPath["/api/devbox-settings"]
	if body["setting"] != "default-size" || body["value"] != "shared-4x" {
		t.Fatalf("body: %+v", body)
	}
	if body["repo"] != "github:github.com/acme/widget" {
		t.Fatalf("the write must carry the derived repo scope: %+v", body)
	}
	if _, present := body["context_id"]; present {
		t.Fatalf("the write must carry no context_id (server derives it): %+v", body)
	}
	if hits.sizes != 1 {
		t.Fatalf("size set keeps the advisory pre-flight (1 GET /api/workspaces/sizes), got %d", hits.sizes)
	}
	if hits.regions != 0 {
		t.Fatalf("size set must not read /api/regions, got %d", hits.regions)
	}
	if !strings.Contains(out, "Default size set to shared-4x") {
		t.Fatalf("confirmation output, got:\n%s", out)
	}
}

// The size pre-flight is ADVISORY: an id absent from the catalog warns on
// stderr but the POST is still sent — the server stays authoritative.
func TestCmdSetDefaultSizeUnknownIDWarnsButPosts(t *testing.T) {
	byPath := map[string]map[string]any{}
	var hits settingsHits
	srv := hermeticEnv(t, settingsCaptureHandler(byPath, &hits))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	var stderr string
	out := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := cmdSetDefaultSize(context.Background(), []string{"--repo", "Acme/Widget", "mega-999"}); err != nil {
				t.Errorf("cmdSetDefaultSize: %v", err)
			}
		})
	})
	if !strings.Contains(stderr, `"mega-999" is not in `+"`rift sizes`") {
		t.Fatalf("unknown id must warn on stderr, got:\n%s", stderr)
	}
	body := byPath["/api/devbox-settings"]
	if body == nil || body["value"] != "mega-999" {
		t.Fatalf("the warning must not block the POST: %+v", body)
	}
	if !strings.Contains(out, "Default size set to mega-999") {
		t.Fatalf("confirmation output, got:\n%s", out)
	}
}

// A failed pre-flight catalog read is silently skipped — it never blocks the
// POST (same error tolerance as the region dimension).
func TestCmdSetDefaultSizePreflightFailureDoesNotBlockPost(t *testing.T) {
	byPath := map[string]map[string]any{}
	srv := hermeticEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/devbox-settings":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			byPath[r.URL.Path] = body
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/sizes":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdSetDefaultSize(context.Background(), []string{"--repo", "Acme/Widget", "shared-4x"}); err != nil {
			t.Errorf("a failed pre-flight must not fail the command: %v", err)
		}
	})
	body := byPath["/api/devbox-settings"]
	if body == nil || body["value"] != "shared-4x" || body["setting"] != "default-size" {
		t.Fatalf("the POST must still be sent: %+v", body)
	}
	if !strings.Contains(out, "Default size set to shared-4x") {
		t.Fatalf("confirmation output, got:\n%s", out)
	}
}

// set-repo-builder-size posts {repo, size} to /api/repos/builder-size (no
// context — builds carry none), canonicalizing the repo argument.
func TestCmdSetRepoBuilderSizeBody(t *testing.T) {
	byPath := map[string]map[string]any{}
	var hits settingsHits
	srv := hermeticEnv(t, settingsCaptureHandler(byPath, &hits))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdSetRepoBuilderSize(context.Background(), []string{"Acme/Widget", "shared-8x-16g"}); err != nil {
			t.Fatalf("cmdSetRepoBuilderSize: %v", err)
		}
	})
	body := byPath["/api/repos/builder-size"]
	if body == nil {
		t.Fatal("no POST /api/repos/builder-size captured")
	}
	if body["repo"] != "github:github.com/acme/widget" || body["size"] != "shared-8x-16g" {
		t.Fatalf("body: %+v", body)
	}
	if _, present := body["clear"]; present {
		t.Fatalf("a plain set must not send clear: %+v", body)
	}
	if _, present := body["context_id"]; present {
		t.Fatalf("builder-size is repo-scoped only (no context_id): %+v", body)
	}
	if !strings.Contains(out, "set to shared-8x-16g") {
		t.Fatalf("confirmation output, got:\n%s", out)
	}
}

// --clear (or a missing SIZE) reverts to the global default: {repo, clear:true},
// no size key.
func TestCmdSetRepoBuilderSizeClear(t *testing.T) {
	for name, args := range map[string][]string{
		"explicit-clear": {"--clear", "acme/widget"},
		"no-size":        {"acme/widget"},
	} {
		t.Run(name, func(t *testing.T) {
			byPath := map[string]map[string]any{}
			var hits settingsHits
			srv := hermeticEnv(t, settingsCaptureHandler(byPath, &hits))
			seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

			out := captureStdout(t, func() {
				if err := cmdSetRepoBuilderSize(context.Background(), args); err != nil {
					t.Fatalf("cmdSetRepoBuilderSize: %v", err)
				}
			})
			body := byPath["/api/repos/builder-size"]
			if body["repo"] != "github:github.com/acme/widget" {
				t.Fatalf("body: %+v", body)
			}
			if v, present := body["clear"]; !present || v != true {
				t.Fatalf("clear must send clear:true: %+v", body)
			}
			if _, present := body["size"]; present {
				t.Fatalf("clear must omit size: %+v", body)
			}
			if !strings.Contains(out, "cleared") {
				t.Fatalf("confirmation output, got:\n%s", out)
			}
		})
	}
}

// No repo argument → usage error before any POST.
func TestCmdSetRepoBuilderSizeUsage(t *testing.T) {
	byPath := map[string]map[string]any{}
	var hits settingsHits
	_ = hermeticEnv(t, settingsCaptureHandler(byPath, &hits))

	err := cmdSetRepoBuilderSize(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("missing repo must be a usage error, got %v", err)
	}
	if len(byPath) != 0 {
		t.Fatalf("a usage error must not POST: %+v", byPath)
	}
}

// Flags parse wherever they appear — before, between, or after positionals —
// matching the documented grammar (`rift set-repo-builder-size <REPO>
// [--clear] <SIZE>`, `rift set-default-region [--repo R] [--clear] <VALUE>`).
// Each spelling must land the same POST as its flags-first equivalent.
func TestSetCommandsAcceptInterleavedFlags(t *testing.T) {
	type tc struct {
		run      func(context.Context, []string) error
		args     []string
		path     string // captured POST path
		wantBody map[string]any
		absent   []string // keys the body must NOT contain
		wantOut  string
	}
	cases := map[string]tc{
		"builder-size trailing --clear (docs verbatim)": {
			run:      cmdSetRepoBuilderSize,
			args:     []string{"acme/widget", "--clear"},
			path:     "/api/repos/builder-size",
			wantBody: map[string]any{"repo": "github:github.com/acme/widget", "clear": true},
			absent:   []string{"size"},
			wantOut:  "cleared",
		},
		"builder-size repo then size": {
			run:      cmdSetRepoBuilderSize,
			args:     []string{"acme/widget", "shared-8x-16g"},
			path:     "/api/repos/builder-size",
			wantBody: map[string]any{"repo": "github:github.com/acme/widget", "size": "shared-8x-16g"},
			absent:   []string{"clear"},
			wantOut:  "set to shared-8x-16g",
		},
		"builder-size flags-first still works": {
			run:      cmdSetRepoBuilderSize,
			args:     []string{"--clear", "acme/widget"},
			path:     "/api/repos/builder-size",
			wantBody: map[string]any{"repo": "github:github.com/acme/widget", "clear": true},
			absent:   []string{"size"},
			wantOut:  "cleared",
		},
		"region value then trailing --repo": {
			run:  cmdSetDefaultRegion,
			args: []string{"iad", "--repo", "acme/widget"},
			path: "/api/devbox-settings",
			wantBody: map[string]any{
				"repo":    "github:github.com/acme/widget",
				"setting": "default-region", "value": "iad",
			},
			absent:  []string{"context_id", "clear"},
			wantOut: "Default region set to iad",
		},
		"region --repo then trailing --clear": {
			run:  cmdSetDefaultRegion,
			args: []string{"--repo", "acme/widget", "--clear"},
			path: "/api/devbox-settings",
			wantBody: map[string]any{
				"repo":    "github:github.com/acme/widget",
				"setting": "default-region", "clear": true,
			},
			absent:  []string{"context_id", "value"},
			wantOut: "cleared",
		},
		"size value then trailing --repo": {
			run:  cmdSetDefaultSize,
			args: []string{"shared-4x", "--repo", "acme/widget"},
			path: "/api/devbox-settings",
			wantBody: map[string]any{
				"repo":    "github:github.com/acme/widget",
				"setting": "default-size", "value": "shared-4x",
			},
			absent:  []string{"context_id", "clear"},
			wantOut: "Default size set to shared-4x",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			byPath := map[string]map[string]any{}
			var hits settingsHits
			srv := hermeticEnv(t, settingsCaptureHandler(byPath, &hits))
			seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

			out := captureStdout(t, func() {
				if err := c.run(context.Background(), c.args); err != nil {
					t.Fatalf("%v: %v", c.args, err)
				}
			})
			body := byPath[c.path]
			if body == nil {
				t.Fatalf("no POST %s captured (all: %+v)", c.path, byPath)
			}
			for k, want := range c.wantBody {
				if body[k] != want {
					t.Fatalf("body[%q] = %v, want %v (body: %+v)", k, body[k], want, body)
				}
			}
			for _, k := range c.absent {
				if _, present := body[k]; present {
					t.Fatalf("body must omit %q: %+v", k, body)
				}
			}
			if !strings.Contains(out, c.wantOut) {
				t.Fatalf("output must contain %q, got:\n%s", c.wantOut, out)
			}
		})
	}
}

// An UNKNOWN flag still errors loudly wherever it sits — the stdlib Parse
// rejects it on the pass that reaches it — and nothing is POSTed.
func TestSetCommandsUnknownFlagAnywhereErrors(t *testing.T) {
	byPath := map[string]map[string]any{}
	var hits settingsHits
	srv := hermeticEnv(t, settingsCaptureHandler(byPath, &hits))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	_ = captureStderr(t, func() { // fs.Parse prints its own usage on error
		if err := cmdSetDefaultRegion(context.Background(), []string{"iad", "--bogus"}); err == nil ||
			!strings.Contains(err.Error(), "-bogus") {
			t.Errorf("trailing unknown flag must error loudly, got %v", err)
		}
		if err := cmdSetRepoBuilderSize(context.Background(), []string{"acme/widget", "--bogus"}); err == nil ||
			!strings.Contains(err.Error(), "-bogus") {
			t.Errorf("trailing unknown flag must error loudly, got %v", err)
		}
		if err := cmdSetRepoBuilderSize(context.Background(), []string{"--bogus", "acme/widget"}); err == nil ||
			!strings.Contains(err.Error(), "-bogus") {
			t.Errorf("leading unknown flag must error loudly, got %v", err)
		}
	})
	if len(byPath) != 0 {
		t.Fatalf("an unknown flag must not POST: %+v", byPath)
	}
}

// resolveLoginAPIURL's fallback chain: explicit flag/env > saved config >
// hosted production default. hermeticEnv clears RIFT_API_URL and points config
// at an empty temp dir, so the no-flag/no-config case must land on the default.
func TestResolveLoginAPIURL(t *testing.T) {
	t.Run("explicit flag wins", func(t *testing.T) {
		_ = hermeticEnv(t, func(http.ResponseWriter, *http.Request) {})
		seedConfig(t, &config.Config{APIBaseURL: "https://saved.example", Token: "t"})
		if got := resolveLoginAPIURL("https://flag.example"); got != "https://flag.example" {
			t.Fatalf("flag must win, got %q", got)
		}
	})
	t.Run("saved config used when flag empty", func(t *testing.T) {
		_ = hermeticEnv(t, func(http.ResponseWriter, *http.Request) {})
		seedConfig(t, &config.Config{APIBaseURL: "https://saved.example", Token: "t"})
		if got := resolveLoginAPIURL(""); got != "https://saved.example" {
			t.Fatalf("saved config must be reused, got %q", got)
		}
	})
	t.Run("prod default when neither flag nor config", func(t *testing.T) {
		_ = hermeticEnv(t, func(http.ResponseWriter, *http.Request) {})
		if got := resolveLoginAPIURL(""); got != config.DefaultAPIBaseURL {
			t.Fatalf("first login must default to %q, got %q", config.DefaultAPIBaseURL, got)
		}
	})
}
