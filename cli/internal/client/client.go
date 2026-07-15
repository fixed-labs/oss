// Package client is the devbox CLI's HTTP client for the server's
// developer surface plus the device-flow login. All requests carry the
// developer bearer; the wire shapes mirror the server's JSON exactly.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// PollTimeout bounds a single device-flow poll request (see
	// DevicePoll). Zero means the production default (defaultPollTimeout);
	// tests inject a small value to exercise the per-request-timeout branch
	// without a real 35s wait.
	PollTimeout time.Duration
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		// Long enough to outlive the api's ≤25s long-poll hold (ls/connect
		// watchers); per-call contexts bound the rest.
		HTTP: &http.Client{Timeout: 35 * time.Second},
	}
}

// APIError carries a non-2xx response so callers can branch on status (e.g.
// 409 image-not-ready, 503 no-ready-relay/pool-full).
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// --- Workspace shapes (mirror the server's workspace / listing JSON) ---

type Workspace struct {
	WorkspaceID    string `json:"workspace_id"`
	Status         string `json:"status"`
	Repo           string `json:"repo"`
	ImageCommit    string `json:"image_commit"`
	Size           string `json:"size"`
	Region         string `json:"region"`
	PublicIP       string `json:"public_ip"`
	WgIP           string `json:"wg_ip"`
	WgPubkey       string `json:"wg_pubkey"`
	SSHHostPubkey  string `json:"ssh_host_pubkey"`
	RelayID        string `json:"relay_id"`
	RelayEndpoint  string `json:"relay_endpoint"`
	CreatedAt      int64  `json:"created_at"`
	StoppedAt      int64  `json:"stopped_at"`
	KeepaliveUntil int64  `json:"keepalive_until"`
	ErrorMessage   string `json:"error_message"`
}

type ListItem struct {
	WorkspaceID string `json:"workspace_id"`
	Status      string `json:"status"`
	Repo        string `json:"repo"`
	ImageCommit string `json:"image_commit"`
	Size        string `json:"size"`
	CreatedAt   int64  `json:"created_at"`
	// Context is the box's billed context as a form-value string (the owning
	// context the repo derived, per FIX-217). Server-populated and decoded here,
	// but not yet surfaced by `rift ls`; an empty string is a pre-backfill live row.
	Context string `json:"context"`
}

// Size is one offered VM size as returned by GET /api/workspaces/sizes — the
// developer read surface over the server's size catalog. cpu/memory_mb are
// the size's display spec; Price is a
// preformatted display string ("—" when the active rate-card version doesn't
// price the id) and is display-only — the rate card is the charge authority.
type Size struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	CPU         int    `json:"cpu"`
	MemoryMB    int    `json:"memory_mb"`
	Price       string `json:"price"`
}

// SizeCatalog is the GET /api/workspaces/sizes response: the offered set of
// sizes plus the size a blank `devbox new` resolves to. EffectiveDefault is JSON
// null (decoding to nil) when the offered set is empty.
type SizeCatalog struct {
	EffectiveDefault *string `json:"effective_default"`
	Sizes            []Size  `json:"sizes"`
}

// RegionItem is one selectable (or the caller's pinned-but-deprecated) region
// as returned by GET /api/regions. Slug is the stable logical id the CLI sends
// on `rift new` / `rift set-default-region` (never a cloud code). Status is one
// of "preview" | "available" | "deprecated" | "retired". AvailableNow is the
// advisory health signal — true iff the region's preferred placement has ready
// relay capacity right now (spawnability is decided authoritatively at spawn).
type RegionItem struct {
	Slug         string `json:"slug"`
	DisplayName  string `json:"display_name"`
	Description  string `json:"description"`
	Status       string `json:"status"`
	AvailableNow bool   `json:"available_now"`
}

// RegionsResult is the GET /api/regions response. EffectiveDefault is the slug
// a blank `rift new` resolves to for this caller (chain over user/org/system);
// JSON null (decoding to nil) when nothing is selectable. PinnedDefault is the
// caller's OWN stored user preference (null when unset); it may be
// non-selectable (e.g. since deprecated) and differ from EffectiveDefault —
// that difference is the "your pin is stale, migrate" signal.
type RegionsResult struct {
	EffectiveDefault *string      `json:"effective_default"`
	PinnedDefault    *string      `json:"pinned_default"`
	Regions          []RegionItem `json:"regions"`
}

// AttachBundle is the attach response relayed by the api — everything the
// CLI needs to bring up the tunnel and dial the box.
type AttachBundle struct {
	RelayPublicEndpoint string `json:"relay_public_endpoint"`
	RelayPort           int    `json:"relay_port"`
	WorkspaceWgPubkey   string `json:"workspace_wg_pubkey"`
	WorkspaceWgIP       string `json:"workspace_wg_ip"`
	LaptopWgIP          string `json:"laptop_wg_ip"`
	SSHHostPubkey       string `json:"ssh_host_pubkey"`
}

// --- Workspace operations ---

// CreateResult is the resolved-selection echo the api returns alongside the
// new workspace id: which ref/commit the boot-selection resolved to, and
// whether it fell back to the default branch (an inferred cwd branch with no
// built image). ResolvedRef is empty for an --image <sha> spawn (no ref).
//
// Region/Size echo the dimensions the server RESOLVED the spawn to (region is
// a SLUG, not a cloud code); RegionSource/SizeSource say how each was chosen,
// PER DIMENSION — "explicit" (the flag) | "repo" (the (context, repo)
// refinement) | "context-wide" (the context's account-wide default). Region
// and size resolve independently, so their sources may differ. A dimension an
// older server doesn't echo decodes empty, and cmdNew omits it from the
// resolved-defaults line.
type CreateResult struct {
	WorkspaceID    string `json:"workspace_id"`
	ResolvedRef    string `json:"resolved_ref"`
	ResolvedCommit string `json:"resolved_commit"`
	Fallback       bool   `json:"fallback"`
	Region         string `json:"region"`
	RegionSource   string `json:"region_source"`
	Size           string `json:"size"`
	SizeSource     string `json:"size_source"`
}

// Create spawns a workspace. repo is the canonical forge-qualified id
// ("github:github.com/owner/name"), carried verbatim in the JSON body.
// ref/image are the boot-selection overrides (mutually exclusive — the caller
// enforces that): a full "refs/heads/..." ref string, or an --image commit
// SHA (prefix). Either is omitted from the body when empty. fallbackToDefault
// is always sent; it tells the api to silently fall back to the default
// branch when an INFERRED cwd branch has no built image (false for an
// explicit --ref, so a typo fails loudly).
func (c *Client) Create(ctx context.Context, repo, size, region, ref, image string, fallbackToDefault bool) (*CreateResult, error) {
	var out CreateResult
	body := map[string]any{
		"repo":                repo,
		"fallback_to_default": fallbackToDefault,
	}
	if size != "" {
		body["size"] = size
	}
	if region != "" {
		body["region"] = region
	}
	if ref != "" {
		body["ref"] = ref
	}
	if image != "" {
		body["image"] = image
	}
	if err := c.do(ctx, http.MethodPost, "/api/workspaces", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Image operations (GET /api/repos/images?repo=… + pin/unpin) ---

// ImageItem mirrors one entry of the server's image-list response: a
// live (bootable) base image keyed by commit. Heads are the refs this commit
// heads (a commit may head several); Default marks the default-branch head;
// Pinned marks an explicit never-reap. CreatedAt is epoch-millis.
type ImageItem struct {
	Commit      string   `json:"commit"`
	CreatedAt   int64    `json:"created_at"`
	RegistryRef string   `json:"registry_ref"`
	Pinned      bool     `json:"pinned"`
	BoxCount    int64    `json:"box_count"`
	Heads       []string `json:"heads"`
	Default     bool     `json:"default"`
}

// ListImages reads the live base images for a repo, newest-first. repo is the
// canonical forge-qualified id ("github:github.com/owner/name"); it rides as
// a percent-encoded ?repo= query parameter — never path segments, because the
// canonical id contains ':' and '/' and an encoded %2F path segment is
// rejected by the server's URI compliance before routing.
func (c *Client) ListImages(ctx context.Context, repo string, limit int) ([]ImageItem, error) {
	path := "/api/repos/images?repo=" + url.QueryEscape(repo)
	if limit > 0 {
		path += "&limit=" + strconv.Itoa(limit)
	}
	var out []ImageItem
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PinImage / UnpinImage set/clear the never-reap pin on a (repo, commit).
func (c *Client) PinImage(ctx context.Context, repo, commit string) error {
	return c.imagePin(ctx, repo, commit, "pin")
}

func (c *Client) UnpinImage(ctx context.Context, repo, commit string) error {
	return c.imagePin(ctx, repo, commit, "unpin")
}

func (c *Client) imagePin(ctx context.Context, repo, commit, leaf string) error {
	path := "/api/repos/images/" + url.PathEscape(commit) + "/" + leaf +
		"?repo=" + url.QueryEscape(repo)
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// --- Watched refs (GET /api/repos/watched?repo=… + watch/unwatch, FIX-202) ---

// WatchedRef mirrors one entry of the server's watched-refs response: a git ref
// this repo builds images for. Status is the server-DERIVED lifecycle of the
// ref's build slot ("building" | "pending" | "idle"), never stored. AddedBy is
// the user-id that watched it (the managed-builder sentinel for the
// onboarding-seeded default branch); AddedAt is epoch-millis.
type WatchedRef struct {
	Ref     string `json:"ref"`
	Status  string `json:"status"`
	AddedBy string `json:"added_by"`
	AddedAt int64  `json:"added_at"`
}

// ListWatched reads the watched refs for a repo, newest-first. repo is the
// canonical forge-qualified id; it rides as a percent-encoded ?repo= query
// parameter (same reasoning as ListImages — the canonical id can't be a path
// segment).
func (c *Client) ListWatched(ctx context.Context, repo string) ([]WatchedRef, error) {
	path := "/api/repos/watched?repo=" + url.QueryEscape(repo)
	var out []WatchedRef
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Watch / Unwatch start / stop watching a git ref on a repo. The ref rides in
// the JSON body ({"ref": …}) — a ref carries '/'s, so it is a body field, never
// a path segment (the repo is still the percent-encoded ?repo= query param).
func (c *Client) Watch(ctx context.Context, repo, ref string) error {
	return c.setWatch(ctx, repo, ref, "watch")
}

func (c *Client) Unwatch(ctx context.Context, repo, ref string) error {
	return c.setWatch(ctx, repo, ref, "unwatch")
}

func (c *Client) setWatch(ctx context.Context, repo, ref, leaf string) error {
	path := "/api/repos/" + leaf + "?repo=" + url.QueryEscape(repo)
	return c.do(ctx, http.MethodPost, path, map[string]string{"ref": ref}, nil)
}

// List does a single absolute read (no cursor → snapshot).
func (c *Client) List(ctx context.Context) ([]ListItem, error) {
	var out struct {
		Workspaces []ListItem `json:"workspaces"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/workspaces", nil, &out); err != nil {
		return nil, err
	}
	return out.Workspaces, nil
}

// Sizes reads the offered VM size catalog from the developer surface
// (GET /api/workspaces/sizes — bearer-authenticated, same as List), returning
// the offered sizes plus the effective default a blank `devbox new` resolves to.
func (c *Client) Sizes(ctx context.Context) (*SizeCatalog, error) {
	var out SizeCatalog
	if err := c.do(ctx, http.MethodGet, "/api/workspaces/sizes", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Regions reads the selectable region catalog from the developer surface
// (GET /api/regions — bearer-authenticated, same as List). repo, when
// non-empty, rides as ?repo= so the defaults reflect the repo's OWNING context
// (FIX-217); absent ⇒ the server uses the bearer's Personal context.
func (c *Client) Regions(ctx context.Context, repo string) (*RegionsResult, error) {
	path := "/api/regions"
	if repo != "" {
		path += "?repo=" + url.QueryEscape(repo)
	}
	var out RegionsResult
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SelectableError is the decoded structured 4xx body the settings writes and
// the create edge share: {"error":"<code>","detail":"<human>",
// "selectable":[…]} — a machine code, a human message, and the values the
// caller could have picked (region slugs or size ids).
type SelectableError struct {
	Code       string   `json:"error"`
	Detail     string   `json:"detail"`
	Selectable []string `json:"selectable"`
}

// DecodeSelectableError decodes a 4xx body into a SelectableError. ok is
// false when the body isn't this shape (undecodable, or carries neither a
// code nor a detail) — the caller should then surface the raw body/error so
// no information is lost.
func DecodeSelectableError(body string) (SelectableError, bool) {
	var e SelectableError
	if json.Unmarshal([]byte(body), &e) == nil && (e.Code != "" || e.Detail != "") {
		return e, true
	}
	return SelectableError{}, false
}

// Message renders the human message: the `detail` (falling back to the
// machine code when the server sent none), with the selectable list appended
// so the user can pick a valid value.
func (e SelectableError) Message() string {
	msg := e.Detail
	if msg == "" {
		msg = e.Code
	}
	if len(e.Selectable) > 0 {
		msg += " (available: " + strings.Join(e.Selectable, ", ") + ")"
	}
	return msg
}

// settingsPost POSTs a settings write and surfaces a structured 4xx
// (SelectableError shape) as its human message rather than the raw
// "HTTP 4xx: {…json…}" APIError string; an unstructured 4xx body is surfaced
// verbatim.
func (c *Client) settingsPost(ctx context.Context, path string, body any) error {
	if err := c.do(ctx, http.MethodPost, path, body, nil); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
			if se, ok := DecodeSelectableError(ae.Body); ok {
				return errors.New(se.Message())
			}
			return errors.New(ae.Body)
		}
		return err
	}
	return nil
}

// SetDevboxSetting writes (or, with clear, clears) one spawn default via
// POST /api/devbox-settings. repo is REQUIRED (FIX-217): it names the repo
// whose OWNING context's defaults the write targets (owner/admin-gated
// server-side) — the server rejects a settings write with no repo. setting is
// "default-region" | "default-size". Validity is authoritative server-side (a
// structured 4xx surfaces its detail + selectable list; a repo write by a
// non-admin is a 403).
func (c *Client) SetDevboxSetting(ctx context.Context, repo, setting, value string, clear bool) error {
	body := map[string]any{"setting": setting}
	if repo != "" {
		body["repo"] = repo
	}
	if clear {
		body["clear"] = true
	} else {
		body["value"] = value
	}
	return c.settingsPost(ctx, "/api/devbox-settings", body)
}

// SetRepoBuilderSize sets (or, with clear, clears) the per-repo BUILDER size
// via POST /api/repos/builder-size — the guest managed image builds for the
// repo run on. Builds carry no context, so this is repo-scoped only; a
// cleared repo reverts to the server's global default. Validity is
// authoritative server-side (same structured-4xx surface as SetDevboxSetting).
func (c *Client) SetRepoBuilderSize(ctx context.Context, repo, size string, clear bool) error {
	body := map[string]any{"repo": repo}
	if clear {
		body["clear"] = true
	} else {
		body["size"] = size
	}
	return c.settingsPost(ctx, "/api/repos/builder-size", body)
}

// Get reads one workspace (snapshot when cursor is empty; long-poll otherwise,
// returning the new cursor — the connect watcher's primitive).
func (c *Client) Get(ctx context.Context, id, cursor string) (*Workspace, string, error) {
	path := "/api/workspaces/" + url.PathEscape(id)
	if cursor != "" {
		path += "?cursor=" + url.QueryEscape(cursor)
	}
	var out struct {
		Workspace Workspace `json:"workspace"`
		Cursor    string    `json:"cursor"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		// Long-poll hold timed out with no change → 304. The cursor is still
		// current, so signal "no change" by returning an empty workspace and the
		// SAME cursor (re-poll), not an error. Uses the same 304 no-change
		// convention as the long-poll endpoints. Guarded to long-poll
		// mode: a snapshot read (cursor=="") 200s by contract, so a 304 there is
		// a real fault and stays an error.
		var ae *APIError
		if cursor != "" && errors.As(err, &ae) && ae.Status == http.StatusNotModified {
			return &Workspace{}, cursor, nil
		}
		return nil, "", err
	}
	return &out.Workspace, out.Cursor, nil
}

func (c *Client) simplePost(ctx context.Context, id, leaf string, body any) error {
	return c.do(ctx, http.MethodPost, "/api/workspaces/"+url.PathEscape(id)+"/"+leaf, body, nil)
}

func (c *Client) Suspend(ctx context.Context, id string) error {
	return c.simplePost(ctx, id, "suspend", nil)
}
func (c *Client) Resume(ctx context.Context, id string) error {
	return c.simplePost(ctx, id, "resume", nil)
}

func (c *Client) Resize(ctx context.Context, id, size string) error {
	return c.simplePost(ctx, id, "resize", map[string]string{"size": size})
}

func (c *Client) Keepalive(ctx context.Context, id string, forMs int64) error {
	return c.simplePost(ctx, id, "keepalive", map[string]int64{"for_ms": forMs})
}

// Presence is the liveness ping the running `connect` sends on a short cadence:
// it bumps the workspace's last-interactive-liveness-at so a connected session is
// not idle-suspended. Stop sending → the liveness lapses and the box
// idle-suspends after the suspend window.
func (c *Client) Presence(ctx context.Context, id string) error {
	return c.simplePost(ctx, id, "presence", nil)
}

func (c *Client) Destroy(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/workspaces/"+url.PathEscape(id), nil, nil)
}

// --- Machine-token self-service routes (in-VM) ---
//
// In-VM, the env carries a MACHINE bearer (subject = this workspace-id), which
// the dev-token /api/workspaces routes reject. Self-service lifecycle goes
// through the machine-authenticated agent prefix (the server's in-VM
// self-service surface) instead, where the wrapper asserts token subject ==
// path workspace-id — a
// workspace may only act on itself.

func (c *Client) machinePost(ctx context.Context, id, leaf string, body any) error {
	return c.do(ctx, http.MethodPost, "/api/rift/v1/"+url.PathEscape(id)+"/"+leaf, body, nil)
}

func (c *Client) MachineSuspend(ctx context.Context, id string) error {
	return c.machinePost(ctx, id, "suspend", nil)
}

func (c *Client) MachineResize(ctx context.Context, id, size string) error {
	return c.machinePost(ctx, id, "resize", map[string]string{"size": size})
}

func (c *Client) MachineKeepalive(ctx context.Context, id string, forMs int64) error {
	return c.machinePost(ctx, id, "keepalive", map[string]int64{"for_ms": forMs})
}

// SecretAccessEvent is the audit body the box posts after a `devbox run`.
// The workspace identity is taken server-side from the machine token, so it is
// NOT in the body. ExitCode is a pointer so a nil (e.g. a --shell session or a
// never-launched child) encodes as JSON null. The contract is pinned to the
// server — do not add fields (no `reason` until a future escalation).
type SecretAccessEvent struct {
	Type     string `json:"type"`    // always "secret-access"
	Secret   string `json:"secret"`  // the secret name
	Command  string `json:"command"` // argv joined, or "--shell session" / "--materialize-to <path>"
	ExitCode *int   `json:"exit_code"`
	Outcome  string `json:"outcome"` // "success" | "failure"
}

// PostSecretAccess posts one secret-access audit event for the given workspace
// via the machine-token events endpoint. The api responds 204 on success. Audit
// is best-effort and non-fatal at the call site; this returns the error so the
// caller can decide (it must not fail the command).
func (c *Client) PostSecretAccess(ctx context.Context, id string, ev SecretAccessEvent) error {
	return c.machinePost(ctx, id, "events", ev)
}

// Attach opens an attachment for the laptop's WG pubkey and returns the
// transport bundle (or a typed *APIError: 409 not-attachable, 503 pool-full).
func (c *Client) Attach(ctx context.Context, id, laptopWgPubkey, loginUser string) (*AttachBundle, error) {
	body := map[string]string{"laptop_wg_pubkey": laptopWgPubkey}
	if loginUser != "" {
		body["login_user"] = loginUser
	}
	var b AttachBundle
	if err := c.simplePostOut(ctx, id, "attach", body, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (c *Client) Detach(ctx context.Context, id, laptopWgPubkey string) error {
	return c.simplePost(ctx, id, "detach", map[string]string{"laptop_wg_pubkey": laptopWgPubkey})
}

func (c *Client) simplePostOut(ctx context.Context, id, leaf string, body, out any) error {
	return c.do(ctx, http.MethodPost, "/api/workspaces/"+url.PathEscape(id)+"/"+leaf, body, out)
}
