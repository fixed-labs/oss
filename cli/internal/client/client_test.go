package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "dev-bearer")
}

func TestCreateSendsBodyAndAuth(t *testing.T) {
	var got map[string]any
	var auth string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspace_id":    "ws-new",
			"resolved_ref":    "refs/heads/main",
			"resolved_commit": "abcdef0123456789",
			"fallback":        true,
		})
	})
	// the repo wire value is the canonical forge-qualified string (what
	// cmd/rift's canonicalRepo produces and the api stores/returns)
	res, err := c.Create(context.Background(), "github:github.com/org/app", "shared-2x", "iad", "refs/heads/main", "", true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.WorkspaceID != "ws-new" {
		t.Fatalf("id: %q", res.WorkspaceID)
	}
	if res.ResolvedRef != "refs/heads/main" || res.ResolvedCommit != "abcdef0123456789" || !res.Fallback {
		t.Fatalf("resolved: %+v", res)
	}
	if auth != "Bearer dev-bearer" {
		t.Fatalf("auth: %q", auth)
	}
	if got["repo"] != "github:github.com/org/app" || got["size"] != "shared-2x" ||
		got["region"] != "iad" ||
		got["ref"] != "refs/heads/main" || got["fallback_to_default"] != true {
		t.Fatalf("body: %+v", got)
	}
	if _, present := got["context_id"]; present {
		t.Fatalf("Create body must not send context_id: %+v", got)
	}
}

func TestCreateOmitsBlankOptionals(t *testing.T) {
	var got map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"workspace_id": "x"})
	})
	_, _ = c.Create(context.Background(), "r", "", "", "", "", false)
	if _, ok := got["size"]; ok {
		t.Fatal("blank size should be omitted")
	}
	if _, ok := got["region"]; ok {
		t.Fatal("blank region should be omitted")
	}
	if _, ok := got["ref"]; ok {
		t.Fatal("blank ref should be omitted")
	}
	if _, ok := got["image"]; ok {
		t.Fatal("blank image should be omitted")
	}
	// fallback_to_default is always sent, even when false.
	if v, ok := got["fallback_to_default"]; !ok || v != false {
		t.Fatalf("fallback_to_default should always be present (false): %v", got["fallback_to_default"])
	}
}

func TestCreateTypedError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "image-not-ready"})
	})
	_, err := c.Create(context.Background(), "r", "", "", "", "", false)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("want 409 APIError, got %v", err)
	}
}

func TestListImages(t *testing.T) {
	const repo = "github:github.com/org/app"
	cases := []struct {
		name      string
		limit     int
		wantLimit string // "" → the limit param must be absent
	}{
		{"no-limit", 0, ""},
		// A nonzero limit rides as a SECOND query param: if the "&limit="
		// join ever regresses to "?limit=", the decoded repo below corrupts —
		// only a with-limit row catches that.
		{"with-limit", 5, "5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/repos/images" || r.Method != http.MethodGet {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				// The whole canonical string rides percent-encoded in ?repo=
				// (its ':' and '/' must not appear raw)…
				if !strings.Contains(r.URL.RawQuery, "repo=github%3Agithub.com%2Forg%2Fapp") {
					t.Errorf("raw query not percent-encoded: %q", r.URL.RawQuery)
				}
				// …and the DECODED values are the behavioral anchor.
				if got := r.URL.Query().Get("repo"); got != repo {
					t.Errorf("repo: %q, want %q", got, repo)
				}
				if got := r.URL.Query().Get("limit"); got != c.wantLimit {
					t.Errorf("limit: %q, want %q", got, c.wantLimit)
				}
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"commit": "abc123", "created_at": 100, "registry_ref": "reg@sha256:x",
						"pinned": true, "box_count": 2, "heads": []string{"refs/heads/main"}, "default": true},
				})
			})
			items, err := cl.ListImages(context.Background(), repo, c.limit)
			if err != nil {
				t.Fatalf("ListImages: %v", err)
			}
			if len(items) != 1 || items[0].Commit != "abc123" || items[0].BoxCount != 2 ||
				!items[0].Pinned || !items[0].Default || len(items[0].Heads) != 1 {
				t.Fatalf("items: %+v", items)
			}
		})
	}
}

func TestPinUnpinImage(t *testing.T) {
	const repo = "github:github.com/org/app"
	var paths, repos []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: %s", r.Method)
		}
		// Same wire form as ListImages: percent-encoded raw ?repo=, decoded
		// back to the canonical id (asserted below per captured request).
		if !strings.Contains(r.URL.RawQuery, "repo=github%3Agithub.com%2Forg%2Fapp") {
			t.Errorf("raw query not percent-encoded: %q", r.URL.RawQuery)
		}
		paths = append(paths, r.URL.Path)
		repos = append(repos, r.URL.Query().Get("repo"))
		_ = json.NewEncoder(w).Encode(map[string]any{"pinned": true})
	})
	if err := c.PinImage(context.Background(), repo, "deadbeef"); err != nil {
		t.Fatalf("PinImage: %v", err)
	}
	if err := c.UnpinImage(context.Background(), repo, "deadbeef"); err != nil {
		t.Fatalf("UnpinImage: %v", err)
	}
	want := []string{"/api/repos/images/deadbeef/pin", "/api/repos/images/deadbeef/unpin"}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("paths: %v", paths)
	}
	for i, got := range repos {
		if got != repo {
			t.Fatalf("repo query param on %s: %q, want %q", paths[i], got, repo)
		}
	}
}

func TestListWatched(t *testing.T) {
	const repo = "github:github.com/org/app"
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/repos/watched" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		// Same wire form as ListImages: the canonical id rides percent-encoded
		// in ?repo= (its ':' and '/' must not appear raw)…
		if !strings.Contains(r.URL.RawQuery, "repo=github%3Agithub.com%2Forg%2Fapp") {
			t.Errorf("raw query not percent-encoded: %q", r.URL.RawQuery)
		}
		// …and the DECODED value is the behavioral anchor.
		if got := r.URL.Query().Get("repo"); got != repo {
			t.Errorf("repo: %q, want %q", got, repo)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ref": "refs/heads/main", "status": "building", "added_by": "u-1", "added_at": 200},
			{"ref": "refs/heads/dev", "status": "idle", "added_by": "managed-builder", "added_at": 100},
		})
	})
	items, err := cl.ListWatched(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListWatched: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items: %+v", items)
	}
	if items[0].Ref != "refs/heads/main" || items[0].Status != "building" ||
		items[0].AddedBy != "u-1" || items[0].AddedAt != 200 {
		t.Fatalf("item0: %+v", items[0])
	}
	if items[1].Ref != "refs/heads/dev" || items[1].Status != "idle" {
		t.Fatalf("item1: %+v", items[1])
	}
}

func TestWatchUnwatch(t *testing.T) {
	const repo = "github:github.com/org/app"
	const ref = "refs/heads/feature"
	var paths, repos, bodyRefs []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: %s", r.Method)
		}
		// Same wire form as the image writes: percent-encoded raw ?repo=…
		if !strings.Contains(r.URL.RawQuery, "repo=github%3Agithub.com%2Forg%2Fapp") {
			t.Errorf("raw query not percent-encoded: %q", r.URL.RawQuery)
		}
		// …and the ref rides the JSON body, NOT the URL (it carries '/'s).
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		paths = append(paths, r.URL.Path)
		repos = append(repos, r.URL.Query().Get("repo"))
		bodyRefs = append(bodyRefs, body["ref"])
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": body["ref"], "watched": true})
	})
	if err := c.Watch(context.Background(), repo, ref); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if err := c.Unwatch(context.Background(), repo, ref); err != nil {
		t.Fatalf("Unwatch: %v", err)
	}
	want := []string{"/api/repos/watch", "/api/repos/unwatch"}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("paths: %v", paths)
	}
	for i := range paths {
		if repos[i] != repo {
			t.Fatalf("repo query param on %s: %q, want %q", paths[i], repos[i], repo)
		}
		if bodyRefs[i] != ref {
			t.Fatalf("body ref on %s: %q, want %q", paths[i], bodyRefs[i], ref)
		}
	}
}

func TestList(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspaces" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspaces": []map[string]any{
				{"workspace_id": "ws-1", "status": "running", "repo": "org/a", "size": "shared-1x", "created_at": 100},
			},
		})
	})
	items, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].WorkspaceID != "ws-1" || items[0].Status != "running" {
		t.Fatalf("items: %+v", items)
	}
}

// ListItem.Context decodes the `context` json field. A /api/workspaces row
// carrying "context":"company:c1" must populate ListItem.Context (the box's
// billed owning context, server-populated for display); a missing or renamed
// json tag would silently drop it.
func TestListDecodesContext(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspaces": []map[string]any{
				{"workspace_id": "ws-1", "status": "running", "repo": "org/a", "context": "company:c1"},
				{"workspace_id": "ws-2", "status": "running", "repo": "org/b"}, // no context → empty
			},
		})
	})
	items, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items: %+v", items)
	}
	if items[0].Context != "company:c1" {
		t.Fatalf("row 0 context: %q, want company:c1", items[0].Context)
	}
	if items[1].Context != "" {
		t.Fatalf("row 1 (no context) must decode to empty, got %q", items[1].Context)
	}
}

func TestSizes(t *testing.T) {
	def := "shared-2x"
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspaces/sizes" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer dev-bearer" {
			t.Errorf("sizes must carry the bearer: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"effective_default": def,
			"sizes": []map[string]any{
				{"id": "shared-2x", "display_name": "Medium · 2 vCPU · 1 GB",
					"description": "balanced", "cpu": 2, "memory_mb": 1024, "price": "$0.0042 / hr"},
			},
		})
	})
	cat, err := c.Sizes(context.Background())
	if err != nil {
		t.Fatalf("Sizes: %v", err)
	}
	if cat.EffectiveDefault == nil || *cat.EffectiveDefault != def {
		t.Fatalf("effective_default: %v", cat.EffectiveDefault)
	}
	if len(cat.Sizes) != 1 {
		t.Fatalf("sizes: %+v", cat.Sizes)
	}
	s := cat.Sizes[0]
	if s.ID != "shared-2x" || s.CPU != 2 || s.MemoryMB != 1024 || s.Price != "$0.0042 / hr" {
		t.Fatalf("size: %+v", s)
	}
}

// The handler returns effective_default:null + sizes:[] when no size is offered.
// EffectiveDefault must decode to nil, not "".
func TestSizesEmptyNullDefault(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"effective_default": nil,
			"sizes":             []map[string]any{},
		})
	})
	cat, err := c.Sizes(context.Background())
	if err != nil {
		t.Fatalf("Sizes: %v", err)
	}
	if cat.EffectiveDefault != nil {
		t.Fatalf("null effective_default must decode nil, got %q", *cat.EffectiveDefault)
	}
	if len(cat.Sizes) != 0 {
		t.Fatalf("expected no sizes, got %+v", cat.Sizes)
	}
}

func TestGetThreadsCursor(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != "h:prev" {
			t.Errorf("cursor not threaded: %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspace": map[string]any{"workspace_id": "ws-1", "status": "running", "wg_ip": "fd5e::1"},
			"cursor":    "h:next",
		})
	})
	ws, cursor, err := c.Get(context.Background(), "ws-1", "h:prev")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ws.WgIP != "fd5e::1" || cursor != "h:next" {
		t.Fatalf("ws=%+v cursor=%q", ws, cursor)
	}
}

// A long-poll hold that times out with no change returns 304. The watcher
// (connect.go waitRunning) must treat that as "no change → re-poll with the
// same cursor", NOT a fatal error — otherwise `devbox connect` dies with
// "HTTP 304:" the moment the box hasn't changed within a hold window.
func TestGetLongPoll304IsNoChangeNotError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified) // hold-timeout, no change
	})
	ws, cursor, err := c.Get(context.Background(), "ws-1", "h:prev")
	if err != nil {
		t.Fatalf("304 long-poll must not error: %v", err)
	}
	if ws.WorkspaceID != "" {
		t.Fatalf("304 must yield an empty (no-change) workspace, got %+v", ws)
	}
	if cursor != "h:prev" {
		t.Fatalf("304 must re-poll with the SAME cursor, got %q", cursor)
	}
}

// A 304 on a SNAPSHOT read (cursor=="") violates the server contract (it 200s
// on cursor=="") and must stay a hard error, not be silently swallowed.
func TestGetSnapshot304StaysError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	if _, _, err := c.Get(context.Background(), "ws-1", ""); err == nil {
		t.Fatal("304 on a snapshot read must be an error")
	}
}

func TestLifecycleVerbsHitRightPaths(t *testing.T) {
	var seen []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	ctx := context.Background()
	_ = c.Suspend(ctx, "ws-1")
	_ = c.Resume(ctx, "ws-1")
	_ = c.Resize(ctx, "ws-1", "shared-4x")
	_ = c.Keepalive(ctx, "ws-1", 60000)
	_ = c.Destroy(ctx, "ws-1")
	want := []string{
		"POST /api/workspaces/ws-1/suspend",
		"POST /api/workspaces/ws-1/resume",
		"POST /api/workspaces/ws-1/resize",
		"POST /api/workspaces/ws-1/keepalive",
		"DELETE /api/workspaces/ws-1",
	}
	if len(seen) != len(want) {
		t.Fatalf("seen %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen[%d]=%q want %q", i, seen[i], want[i])
		}
	}
}

func TestMachineVerbsHitAgentRoutes(t *testing.T) {
	var seen []string
	var auth string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	ctx := context.Background()
	_ = c.MachineSuspend(ctx, "ws-1")
	_ = c.MachineResize(ctx, "ws-1", "shared-4x")
	_ = c.MachineKeepalive(ctx, "ws-1", 60000)
	want := []string{
		"POST /api/rift/v1/ws-1/suspend",
		"POST /api/rift/v1/ws-1/resize",
		"POST /api/rift/v1/ws-1/keepalive",
	}
	if len(seen) != len(want) {
		t.Fatalf("seen %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen[%d]=%q want %q", i, seen[i], want[i])
		}
	}
	if auth != "Bearer dev-bearer" {
		t.Fatalf("machine routes must carry the bearer: %q", auth)
	}
}

func TestAttachBundle(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["laptop_wg_pubkey"] != "LP" || body["login_user"] != "dev" {
			t.Errorf("attach body: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"relay_public_endpoint": "1.2.3.4", "relay_port": 49152,
			"workspace_wg_pubkey": "WS", "workspace_wg_ip": "fd5e::9",
			"laptop_wg_ip": "fd5e::a", "ssh_host_pubkey": "ssh-ed25519 HK",
		})
	})
	b, err := c.Attach(context.Background(), "ws-1", "LP", "dev")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if b.RelayPublicEndpoint != "1.2.3.4" || b.RelayPort != 49152 ||
		b.WorkspaceWgPubkey != "WS" || b.LaptopWgIP != "fd5e::a" ||
		b.SSHHostPubkey != "ssh-ed25519 HK" {
		t.Fatalf("bundle: %+v", b)
	}
}

// TestPostSecretAccess asserts the audit POST hits the machine events endpoint
// with the pinned secret-access body — the workspace id is in the path, NOT the
// body, and the body carries exactly type/secret/command/exit_code/outcome (no
// reason).
func TestPostSecretAccess(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	exit := 0
	err := c.PostSecretAccess(context.Background(), "ws-7", SecretAccessEvent{
		Type:     "secret-access",
		Secret:   "aws",
		Command:  "aws s3 ls",
		ExitCode: &exit,
		Outcome:  "success",
	})
	if err != nil {
		t.Fatalf("PostSecretAccess: %v", err)
	}
	if gotPath != "/api/rift/v1/ws-7/events" || gotMethod != http.MethodPost {
		t.Fatalf("endpoint: %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer dev-bearer" {
		t.Fatalf("auth: %q", gotAuth)
	}
	if gotBody["type"] != "secret-access" || gotBody["secret"] != "aws" ||
		gotBody["command"] != "aws s3 ls" || gotBody["outcome"] != "success" {
		t.Fatalf("body: %+v", gotBody)
	}
	if ec, ok := gotBody["exit_code"].(float64); !ok || ec != 0 {
		t.Fatalf("exit_code: %v (%T)", gotBody["exit_code"], gotBody["exit_code"])
	}
	// workspace id must NOT be in the body (taken server-side from the token).
	for _, k := range []string{"workspace_id", "workspace-id", "reason"} {
		if _, present := gotBody[k]; present {
			t.Fatalf("body must not contain %q: %+v", k, gotBody)
		}
	}
}

// TestPostSecretAccessNilExitEncodesNull confirms a nil ExitCode (a --shell
// session or never-launched child) serializes as JSON null, not 0.
func TestPostSecretAccessNilExitEncodesNull(t *testing.T) {
	var gotBody map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	err := c.PostSecretAccess(context.Background(), "ws-7", SecretAccessEvent{
		Type: "secret-access", Secret: "aws", Command: "--shell session", ExitCode: nil, Outcome: "success",
	})
	if err != nil {
		t.Fatalf("PostSecretAccess: %v", err)
	}
	v, present := gotBody["exit_code"]
	if !present {
		t.Fatal("exit_code key must be present (encodes as null), not omitted")
	}
	if v != nil {
		t.Fatalf("exit_code should be null, got %v (%T)", v, v)
	}
}

// --- Regions (GET /api/regions) ---

// TestRegions decodes the full read-surface: the *string effective/pinned
// pointers (both present), and the regions slice with every field. It also
// pins the request path is /api/regions and that a non-empty repo rides
// as ?repo=<url-escaped> (a canonical id like "github:github.com/org/app" has
// ':' and '/' that must be percent-encoded).
func TestRegions(t *testing.T) {
	const repo = "github:github.com/org/app"
	eff, pin := "us-east", "eu-west"
	var gotPath, gotRawQuery, gotMethod, gotAuth string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRawQuery, gotMethod = r.URL.Path, r.URL.RawQuery, r.Method
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"effective_default": eff,
			"pinned_default":    pin,
			"regions": []map[string]any{
				{"slug": "us-east", "display_name": "US East", "description": "Virginia",
					"status": "available", "available_now": true},
				{"slug": "eu-west", "display_name": "EU West", "description": "Paris",
					"status": "deprecated", "available_now": false},
			},
		})
	})
	res, err := c.Regions(context.Background(), repo)
	if err != nil {
		t.Fatalf("Regions: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/regions" {
		t.Fatalf("endpoint: %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer dev-bearer" {
		t.Fatalf("regions must carry the bearer: %q", gotAuth)
	}
	// repo rides percent-encoded (":" and "/" must not appear raw).
	if gotRawQuery != "repo=github%3Agithub.com%2Forg%2Fapp" {
		t.Fatalf("repo query: %q, want repo=github%%3Agithub.com%%2Forg%%2Fapp", gotRawQuery)
	}
	if res.EffectiveDefault == nil || *res.EffectiveDefault != eff {
		t.Fatalf("effective_default: %v", res.EffectiveDefault)
	}
	if res.PinnedDefault == nil || *res.PinnedDefault != pin {
		t.Fatalf("pinned_default: %v", res.PinnedDefault)
	}
	if len(res.Regions) != 2 {
		t.Fatalf("regions: %+v", res.Regions)
	}
	r0 := res.Regions[0]
	if r0.Slug != "us-east" || r0.DisplayName != "US East" || r0.Description != "Virginia" ||
		r0.Status != "available" || !r0.AvailableNow {
		t.Fatalf("regions[0]: %+v", r0)
	}
	r1 := res.Regions[1]
	if r1.Slug != "eu-west" || r1.Status != "deprecated" || r1.AvailableNow {
		t.Fatalf("regions[1]: %+v", r1)
	}
}

// TestRegionsNullDefaultsAndEmptyRepo covers the null cases: the handler
// returns effective_default:null + pinned_default:null when nothing is
// selectable / the caller has no pin — both *string must decode to nil, not "".
// It also pins that an EMPTY repo omits the query entirely (no bare "?").
func TestRegionsNullDefaultsAndEmptyRepo(t *testing.T) {
	var gotRawQuery string
	sawQuery := false
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		sawQuery = r.URL.RawQuery != ""
		_ = json.NewEncoder(w).Encode(map[string]any{
			"effective_default": nil,
			"pinned_default":    nil,
			"regions":           []map[string]any{},
		})
	})
	res, err := c.Regions(context.Background(), "")
	if err != nil {
		t.Fatalf("Regions: %v", err)
	}
	if sawQuery {
		t.Fatalf("empty repo must omit the query entirely, got %q", gotRawQuery)
	}
	if res.EffectiveDefault != nil {
		t.Fatalf("null effective_default must decode nil, got %q", *res.EffectiveDefault)
	}
	if res.PinnedDefault != nil {
		t.Fatalf("null pinned_default must decode nil, got %q", *res.PinnedDefault)
	}
	if len(res.Regions) != 0 {
		t.Fatalf("expected no regions, got %+v", res.Regions)
	}
}

// --- Create decodes the per-dimension resolution echo ---

// The create success body echoes region/region_source/size/size_source (the
// old single `source` key is GONE — per-dimension resolution means region and
// size may come from different scopes). All four must decode; absent keys
// (an older server) decode empty.
func TestCreateDecodesResolutionEcho(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspace_id":  "ws-new",
			"region":        "iad",
			"region_source": "context-wide",
			"size":          "shared-4x",
			"size_source":   "repo",
		})
	})
	res, err := c.Create(context.Background(), "r", "", "", "", "", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Region != "iad" || res.RegionSource != "context-wide" {
		t.Fatalf("region echo: %+v", res)
	}
	if res.Size != "shared-4x" || res.SizeSource != "repo" {
		t.Fatalf("size echo: %+v", res)
	}

	// Older server: none of the echo keys → all four decode empty.
	c2 := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"workspace_id": "ws-old"})
	})
	res2, err := c2.Create(context.Background(), "r", "", "", "", "", false)
	if err != nil {
		t.Fatalf("Create (older server): %v", err)
	}
	if res2.Region != "" || res2.RegionSource != "" || res2.Size != "" || res2.SizeSource != "" {
		t.Fatalf("absent echo keys must decode empty: %+v", res2)
	}
}

// --- SetDevboxSetting (POST /api/devbox-settings) ---

// TestSetDevboxSettingRepoScoped pins the happy path: a POST to
// /api/devbox-settings carrying the bearer with body {repo, setting, value}
// — no context_id, and NO clear key on a plain set.
func TestSetDevboxSettingRepoScoped(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	err := c.SetDevboxSetting(context.Background(),
		"github:github.com/org/app", "default-region", "us-east", false)
	if err != nil {
		t.Fatalf("SetDevboxSetting: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/devbox-settings" {
		t.Fatalf("endpoint: %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer dev-bearer" {
		t.Fatalf("auth: %q", gotAuth)
	}
	if gotBody["repo"] != "github:github.com/org/app" ||
		gotBody["setting"] != "default-region" || gotBody["value"] != "us-east" {
		t.Fatalf("body: %+v", gotBody)
	}
	if _, present := gotBody["context_id"]; present {
		t.Fatalf("body must not send context_id: %+v", gotBody)
	}
	if _, present := gotBody["clear"]; present {
		t.Fatalf("a plain set must not send clear: %+v", gotBody)
	}
}

// Clear sends {clear:true} and omits value.
func TestSetDevboxSettingClear(t *testing.T) {
	var gotBody map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	err := c.SetDevboxSetting(context.Background(), "", "default-region", "", true)
	if err != nil {
		t.Fatalf("SetDevboxSetting(clear): %v", err)
	}
	if v, present := gotBody["clear"]; !present || v != true {
		t.Fatalf("clear must send clear:true, got %+v", gotBody)
	}
	if _, present := gotBody["value"]; present {
		t.Fatalf("clear must omit value: %+v", gotBody)
	}
}

// TestSetDevboxSettingErrorSurfacesDetail pins that a 4xx with the structured
// body {"error":"<code>","detail":"<human>","selectable":[…]} surfaces the
// HUMAN detail plus the selectable list — NOT the raw machine "error" code and
// NOT the raw "HTTP 4xx: {…}" APIError string.
func TestSetDevboxSettingErrorSurfacesDetail(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":      "region-not-available",
			"detail":     "region not available",
			"selectable": []string{"us-east", "eu-west"},
		})
	})
	err := c.SetDevboxSetting(context.Background(), "", "default-region", "atlantis", false)
	if err == nil {
		t.Fatal("a non-selectable slug must error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "region not available") {
		t.Fatalf("error must surface the human detail, got %q", msg)
	}
	// the raw machine code must NOT be what the user sees (detail is preferred).
	if strings.Contains(msg, "region-not-available") {
		t.Fatalf("error must prefer the human detail over the raw code, got %q", msg)
	}
	// the selectable list is appended so the user can pick a valid one.
	if !strings.Contains(msg, "us-east") || !strings.Contains(msg, "eu-west") {
		t.Fatalf("error must append the selectable list, got %q", msg)
	}
}

// TestSetDevboxSettingUnstructured4xxSurfacesBodyVerbatim pins the OTHER arm
// of the shared settingsPost error path (docs/plans/devbox-spawn-settings-
// tests.md §10): a 4xx whose body is NOT the SelectableError shape surfaces
// that body verbatim — never the raw "HTTP 4xx: …" APIError string. One
// write method suffices: both SetDevboxSetting and SetRepoBuilderSize share
// settingsPost, mirroring the plan's shared-helper reasoning.
func TestSetDevboxSettingUnstructured4xxSurfacesBodyVerbatim(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("plain nope"))
	})
	err := c.SetDevboxSetting(context.Background(), "", "default-region", "iad", false)
	if err == nil {
		t.Fatal("a 4xx must error")
	}
	msg := err.Error()
	if msg != "plain nope" {
		t.Fatalf("an unstructured 4xx body must surface verbatim, got %q", msg)
	}
	if strings.Contains(msg, "HTTP 4") {
		t.Fatalf("the raw APIError string must never reach the user, got %q", msg)
	}
}

// --- SetRepoBuilderSize (POST /api/repos/builder-size) ---

// The set path sends {repo, size} (no clear); the clear path sends
// {repo, clear:true} and omits size. Both carry the bearer.
func TestSetRepoBuilderSize(t *testing.T) {
	const repo = "github:github.com/org/app"
	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	if err := c.SetRepoBuilderSize(context.Background(), repo, "shared-8x-16g", false); err != nil {
		t.Fatalf("SetRepoBuilderSize: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/repos/builder-size" {
		t.Fatalf("endpoint: %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer dev-bearer" {
		t.Fatalf("auth: %q", gotAuth)
	}
	if gotBody["repo"] != repo || gotBody["size"] != "shared-8x-16g" {
		t.Fatalf("body: %+v", gotBody)
	}
	if _, present := gotBody["clear"]; present {
		t.Fatalf("a plain set must not send clear: %+v", gotBody)
	}

	if err := c.SetRepoBuilderSize(context.Background(), repo, "", true); err != nil {
		t.Fatalf("SetRepoBuilderSize(clear): %v", err)
	}
	if v, present := gotBody["clear"]; !present || v != true {
		t.Fatalf("clear must send clear:true, got %+v", gotBody)
	}
	if _, present := gotBody["size"]; present {
		t.Fatalf("clear must omit size: %+v", gotBody)
	}
	if gotBody["repo"] != repo {
		t.Fatalf("clear must still name the repo: %+v", gotBody)
	}
}

// A builder-size 4xx surfaces through the same structured decode.
func TestSetRepoBuilderSizeErrorSurfacesDetail(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":      "size-not-available",
			"detail":     "unknown size",
			"selectable": []string{"shared-8x", "shared-8x-16g"},
		})
	})
	err := c.SetRepoBuilderSize(context.Background(), "github:github.com/org/app", "mega-999x", false)
	if err == nil {
		t.Fatal("an unknown size must error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown size") || !strings.Contains(msg, "shared-8x-16g") {
		t.Fatalf("error must carry the detail + selectable list, got %q", msg)
	}
}

// --- DecodeSelectableError / Message (the shared structured-4xx decode) ---

func TestDecodeSelectableError(t *testing.T) {
	// Structured body → decoded, Message prefers detail + appends the list.
	se, ok := DecodeSelectableError(`{"error":"size-required","detail":"pick a size","selectable":["a","b"]}`)
	if !ok || se.Code != "size-required" || se.Detail != "pick a size" || len(se.Selectable) != 2 {
		t.Fatalf("decode: %+v ok=%v", se, ok)
	}
	if got := se.Message(); got != "pick a size (available: a, b)" {
		t.Fatalf("Message: %q", got)
	}
	// No detail → the code is the message.
	se2, ok := DecodeSelectableError(`{"error":"region-required"}`)
	if !ok || se2.Message() != "region-required" {
		t.Fatalf("code-only: %+v (%q)", se2, se2.Message())
	}
	// Undecodable / empty bodies → not ok (callers fall back to the raw body).
	if _, ok := DecodeSelectableError("not json"); ok {
		t.Fatal("garbage must not decode")
	}
	if _, ok := DecodeSelectableError(`{"unrelated":true}`); ok {
		t.Fatal("a body with neither code nor detail must not decode")
	}
}
