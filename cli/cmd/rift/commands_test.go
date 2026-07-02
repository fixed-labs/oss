package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/config"
)

func TestCanonicalRepo(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// the canonical is the bare lowercase owner/name pair, matching the
		// server and CI; "github:" is
		// accepted as input only.
		{"org/name", "org/name"},
		{"Org/Name.git", "org/name"},
		{"github:org/name", "org/name"},
		{"github:Acme-Corp/Widget", "acme-corp/widget"},
		{"GitHub:Org/Name", "org/name"},
		{"github:org/name.git", "org/name"},
		{"github:org/name/", "org/name"},
		{"github:org/name.git/", "org/name"},
		{"github:org/name//", "org/name"},
		{"  github:org/name  ", "org/name"},
	}
	for _, c := range cases {
		got, err := canonicalRepo(c.in)
		if err != nil {
			t.Errorf("canonicalRepo(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("canonicalRepo(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalRepoRejectsMalformed(t *testing.T) {
	for _, in := range []string{"", "github:", "github:org", "github:org/", "github:/name", "github:org/name/extra"} {
		if got, err := canonicalRepo(in); err == nil {
			t.Errorf("canonicalRepo(%q) = %q, want error", in, got)
		}
	}
}

func TestRepoFromRemote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"git@github.com:org/name.git", "org/name"},
		{"git@GitHub.com:Acme-Corp/Widget.git", "acme-corp/widget"},
		{"https://github.com/org/name", "org/name"},
		{"https://github.com/org/name.git", "org/name"},
		{"https://github.com/Org/Name/", "org/name"},
		{"https://GITHUB.COM/Org/Name", "org/name"},
	}
	for _, c := range cases {
		got, err := repoFromRemote(c.in)
		if err != nil {
			t.Errorf("repoFromRemote(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("repoFromRemote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRepoFromRemoteRejectsNonGitHub(t *testing.T) {
	if got, err := repoFromRemote("https://gitlab.com/org/name"); err == nil {
		t.Fatalf("want error for non-github remote, got %q", got)
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

	_ = captureStdout(t, func() {
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
}
