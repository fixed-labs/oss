package main

import (
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
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// contextsHandler answers GET /api/contexts with the given items (form_value +
// label), the wrapper shape client.Contexts decodes.
func contextsHandler(items []client.ContextItem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/contexts" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"contexts": items})
	}
}

var testContexts = []client.ContextItem{
	{FormValue: "personal:u1", Label: "Personal"},
	{FormValue: "company:c1", Label: "Acme"},
	{FormValue: "company:c2", Label: "Beta"},
}

// --- cmdSetDefaultContext <arg> validates against the live list & persists ---

func TestSetDefaultContextArgMatchPersists(t *testing.T) {
	srv := hermeticEnv(t, contextsHandler(testContexts))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	if err := cmdSetDefaultContext(context.Background(), []string{"company:c1"}); err != nil {
		t.Fatalf("exact match must succeed: %v", err)
	}
	if got := loadConfig(t).DefaultContext; got != "company:c1" {
		t.Fatalf("DefaultContext = %q, want company:c1", got)
	}
}

func TestSetDefaultContextArgNoMatchErrorsAndDoesNotWrite(t *testing.T) {
	srv := hermeticEnv(t, contextsHandler(testContexts))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	err := cmdSetDefaultContext(context.Background(), []string{"company:nope"})
	if err == nil {
		t.Fatal("a non-matching form-value must error")
	}
	// Surfaces the available contexts (loosely — not the exact "unknown context"
	// wording, which is not part of the stable output contract).
	if !strings.Contains(err.Error(), "company:c1") {
		t.Fatalf("error should surface the valid contexts, got %v", err)
	}
	if got := loadConfig(t).DefaultContext; got != "" {
		t.Fatalf("a rejected context must not be written, got %q", got)
	}
}

// --- cmdSetDefaultContext no-arg numbered picker ---

func TestSetDefaultContextPickerValidSelection(t *testing.T) {
	srv := hermeticEnv(t, contextsHandler(testContexts))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})
	withStdin(t, "2\n") // 1-based → the 2nd item (company:c1)

	var err error
	_ = captureStdout(t, func() { err = cmdSetDefaultContext(context.Background(), nil) })
	if err != nil {
		t.Fatalf("valid selection must succeed: %v", err)
	}
	// Selection 2 maps to items[1].FormValue — pins the index→form_value mapping,
	// not the picker glyphs.
	if got := loadConfig(t).DefaultContext; got != "company:c1" {
		t.Fatalf("DefaultContext = %q, want company:c1 (2nd item)", got)
	}
}

func TestSetDefaultContextPickerInvalidInputDoesNotWrite(t *testing.T) {
	cases := map[string]string{
		"non-integer":  "abc\n",
		"out-of-range": "99\n",
		"empty":        "",   // immediate EOF, nothing entered
		"eof-blank":    "\n", // just a newline → blank line
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			srv := hermeticEnv(t, contextsHandler(testContexts))
			seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})
			withStdin(t, input)

			var err error
			_ = captureStdout(t, func() { err = cmdSetDefaultContext(context.Background(), nil) })
			if err == nil {
				t.Fatalf("input %q must error (non-zero exit)", input)
			}
			if got := loadConfig(t).DefaultContext; got != "" {
				t.Fatalf("invalid input must not write, got %q", got)
			}
		})
	}
}

// --- cmdNew context fallback chain & error ---
//
// The happy path calls connect() after Create; the mock captures context_id on
// the Create request (which runs first), then answers the connect-time Get with a
// "failed" workspace so waitRunning returns promptly and cmdNew unwinds without a
// live box. We assert only the captured context_id (precedence), not the connect
// outcome. --repo is passed so no git remote is inferred; --ref avoids the branch
// inference shell-out.

func newCaptureHandler(createHit *bool, gotContextID *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/workspaces" {
			*createHit = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*gotContextID, _ = body["context_id"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"workspace_id": "ws-new"})
			return
		}
		// connect's waitRunning Get → a terminal "failed" status returns fast.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspace": map[string]any{"workspace_id": "ws-new", "status": "failed", "error_message": "test-stop"},
		})
	}
}

func TestCmdNewExplicitContextWins(t *testing.T) {
	var createHit bool
	var gotContextID string
	srv := hermeticEnv(t, newCaptureHandler(&createHit, &gotContextID))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok", DefaultContext: "company:seeded"})

	// --context beats the seeded default; connect error after Create is ignored.
	_ = captureStdout(t, func() {
		_ = cmdNew(context.Background(), []string{"--repo", "org/app", "--ref", "main", "--context", "company:explicit"})
	})
	if !createHit {
		t.Fatal("Create was not called")
	}
	if gotContextID != "company:explicit" {
		t.Fatalf("context_id = %q, want the explicit --context (company:explicit)", gotContextID)
	}
}

func TestCmdNewFallsBackToDefaultContext(t *testing.T) {
	var createHit bool
	var gotContextID string
	srv := hermeticEnv(t, newCaptureHandler(&createHit, &gotContextID))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok", DefaultContext: "company:seeded"})

	_ = captureStdout(t, func() {
		_ = cmdNew(context.Background(), []string{"--repo", "org/app", "--ref", "main"})
	})
	if !createHit {
		t.Fatal("Create was not called")
	}
	if gotContextID != "company:seeded" {
		t.Fatalf("context_id = %q, want the cfg.DefaultContext fallback (company:seeded)", gotContextID)
	}
}

func TestCmdNewNoContextErrorsBeforeCreate(t *testing.T) {
	var createHit bool
	var gotContextID string
	srv := hermeticEnv(t, newCaptureHandler(&createHit, &gotContextID))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"}) // no DefaultContext

	err := cmdNew(context.Background(), []string{"--repo", "org/app", "--ref", "main"})
	if err == nil {
		t.Fatal("no context set must error")
	}
	// The exact error message, verbatim.
	want := "no context set — run `rift set-default-context` or pass --context <ctx>"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if createHit {
		t.Fatal("Create must NOT be called when no context resolves")
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
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok", DefaultContext: "company:c1"})

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
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok", DefaultContext: "company:c1"})

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

// --- cmdList --context client-side filter; bare `ls` ignores the default ---

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

func TestCmdListContextFilterShowsOnlyMatches(t *testing.T) {
	srv := hermeticEnv(t, listHandler(listRows))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdList(context.Background(), []string{"--context", "company:c1"}); err != nil {
			t.Fatalf("cmdList: %v", err)
		}
	})
	if !strings.Contains(out, "ws-acme") {
		t.Fatalf("--context company:c1 must render its box, got:\n%s", out)
	}
	if strings.Contains(out, "ws-beta") {
		t.Fatalf("--context company:c1 must NOT render the other context's box, got:\n%s", out)
	}
}

func TestCmdListNoFlagNoDefaultShowsAll(t *testing.T) {
	srv := hermeticEnv(t, listHandler(listRows))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	out := captureStdout(t, func() {
		if err := cmdList(context.Background(), nil); err != nil {
			t.Fatalf("cmdList: %v", err)
		}
	})
	if !strings.Contains(out, "ws-acme") || !strings.Contains(out, "ws-beta") {
		t.Fatalf("bare ls must render all boxes, got:\n%s", out)
	}
}

// The key regression guard: a bare `ls` must IGNORE cfg.DefaultContext (the
// default is a create target, not an ls filter) — all boxes still render even
// though the seeded default matches only one.
func TestCmdListBareIgnoresDefaultContext(t *testing.T) {
	srv := hermeticEnv(t, listHandler(listRows))
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok", DefaultContext: "company:c1"})

	out := captureStdout(t, func() {
		if err := cmdList(context.Background(), nil); err != nil {
			t.Fatalf("cmdList: %v", err)
		}
	})
	if !strings.Contains(out, "ws-acme") || !strings.Contains(out, "ws-beta") {
		t.Fatalf("bare ls must ignore cfg.DefaultContext and render ALL boxes, got:\n%s", out)
	}
}

// --- cmdLogin preserves an existing DefaultContext ---
//
// Login persists only APIBaseURL + Token; a pre-existing per-device default must
// survive it (the poll no longer even carries a context to latch onto).

func TestCmdLoginPreservesDefaultContext(t *testing.T) {
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
	// Pre-seed a SENTINEL default the login path must not touch.
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "old", DefaultContext: "company:sentinel"})

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
	if got.DefaultContext != "company:sentinel" {
		t.Fatalf("login must PRESERVE DefaultContext, got %q", got.DefaultContext)
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
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "preseeded-tok", DefaultContext: "company:pre"})

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
	// No partial write: the pre-seeded token + DefaultContext survive untouched.
	got := loadConfig(t)
	if got.Token != "preseeded-tok" {
		t.Fatalf("failed login must NOT overwrite the token, got %q", got.Token)
	}
	if got.DefaultContext != "company:pre" {
		t.Fatalf("failed login must NOT touch DefaultContext, got %q", got.DefaultContext)
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
