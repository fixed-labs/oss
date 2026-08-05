package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/fixed-labs/oss/cli/clikit/deviceflow"
	"github.com/fixed-labs/oss/cli/clikit/httpx"
	"github.com/fixed-labs/oss/cli/clikit/kongx"
	"github.com/fixed-labs/oss/cli/clikit/login"
	"github.com/fixed-labs/oss/cli/clikit/table"
	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/config"
	"github.com/fixed-labs/oss/cli/internal/repoid"
)

// cmdLogin runs the device flow and persists the minted bearer. The session
// proves identity only; every command derives the owning/billing context from
// the repo it acts on, so there is no per-device context to select or persist.
func cmdLogin(ctx context.Context, args []string) error {
	// Resolve the active session name first: an invalid RIFT_ENV must fail
	// before the device flow, not after a browser round-trip.
	env, err := config.EnvName()
	if err != nil {
		return err
	}
	// --api defaults to empty (NOT RIFT_API_URL) so a typed flag and the ambient
	// override var stay distinguishable; RIFT_API_URL is read explicitly below.
	var c struct {
		API       string `name:"api" help:"rift API base URL"`
		NoBrowser bool   `name:"no-browser" help:"do not auto-open the verification URL in a browser"`
	}
	if err := kongx.Parse("login", &c, args); err != nil {
		return err
	}
	envURL := os.Getenv("RIFT_API_URL")
	// Source 3 of the FIX-246 precedence: the active env's previously-saved URL.
	savedURL := ""
	if prev, _ := config.Load(); prev != nil {
		savedURL = prev.APIBaseURL
	}
	apiURL, fromOverrideVar, err := login.ResolveURL(c.API, envURL, savedURL, env, config.DefaultAPIBaseURL)
	if err != nil {
		if errors.Is(err, login.ErrNoURLForEnv) {
			// Render rift's own guard wording (the shared sentinel carries none).
			return fmt.Errorf("no API URL saved for env %q — run rift login --api <url>", env)
		}
		return err
	}

	hc := httpx.New(apiURL, "")
	startCtx, cancelStart := ctxTimeout(ctx, 30*time.Second)
	defer cancelStart()
	start, err := deviceflow.Start(startCtx, hc)
	if err != nil {
		return fmt.Errorf("starting login: %w", err)
	}
	url := start.VerificationURL

	// Printed before raw mode (ordinary \n); the always-present fallback.
	fmt.Printf("To log in, open:\n\n    %s\n\nand enter the code:  %s\n\nWaiting for approval…\n",
		url, start.UserCode)

	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	if login.ShouldAutoOpen(os.Getenv, interactive, c.NoBrowser) {
		if err := login.OpenBrowser(url); err != nil {
			slog.Warn("rift login: could not open browser", "err", err) // → diag logfile
		}
	}

	pollCtx, cancelPoll := ctxTimeout(ctx, 10*time.Minute)
	defer cancelPoll()

	var tok *deviceflow.DeviceToken
	if interactive {
		tok, err = login.PollInteractive(pollCtx, cancelPoll, hc, start, url)
	} else {
		tok, err = deviceflow.PollUntilToken(pollCtx, hc, start) // today's behavior exactly
	}
	if err != nil {
		if errors.Is(err, login.ErrLoginCanceled) {
			return err // main renders "rift: login canceled", exit 1
		}
		return fmt.Errorf("login: %w", err) // keep today's wrap for timeout/network errors
	}
	cfg, _ := config.Load()
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.APIBaseURL = apiURL
	cfg.Token = tok.Token
	if err := cfg.Save(); err != nil {
		return err
	}
	// Prod keeps today's exact line. A non-prod login discloses the env, and —
	// when the URL came from the ambient override var rather than a typed flag —
	// names that var, surfacing the one moment a wrong-plane credential could be
	// mis-seeded under a non-prod profile.
	switch {
	case env == "prod":
		fmt.Println("Logged in.")
	case fromOverrideVar:
		fmt.Printf("Logged in to %s (env %s; URL from RIFT_API_URL).\n", apiURL, env)
	default:
		fmt.Printf("Logged in to %s (env %s).\n", apiURL, env)
	}
	return nil
}

// cmdList lists the workspaces the caller owns, across every context they can
// see (owner-scoped, server-side). There is no context filter: context is
// derived from the repo, never a user-facing selector.
func cmdList(ctx context.Context, args []string) error {
	var flags struct{}
	if err := kongx.Parse("ls", &flags, args); err != nil {
		return err
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	items, err := c.List(rctx)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println("No workspaces.")
		return nil
	}
	t := table.New(os.Stdout, "WORKSPACE", "STATUS", "SIZE", "REPO")
	for _, it := range items {
		t.Row(it.WorkspaceID, it.Status, it.Size, it.Repo)
	}
	return t.Flush()
}

func cmdNew(ctx context.Context, args []string) error {
	var flags struct {
		Size   string `help:"guest size (e.g. shared-2x)"`
		Region string `help:"Region (see 'rift regions')"`
		Repo   string `help:"repo (owner/name, a clone URL, or forge:host/owner/name); inferred from cwd if absent"`
		Forge  string `help:"forge type of the repo's host (only github this phase); required when the host isn't a known SaaS forge"`
		Ref    string `help:"boot the head image of this branch (e.g. main); mutually exclusive with --image"`
		Image  string `help:"boot this exact commit's image (full SHA or ≥7-char prefix); mutually exclusive with --ref"`
	}
	if err := kongx.Parse("new", &flags, args); err != nil {
		return err
	}
	if flags.Ref != "" && flags.Image != "" {
		return fmt.Errorf("--ref and --image are mutually exclusive")
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	if flags.Repo, err = repoid.Resolve(flags.Repo, flags.Forge); err != nil {
		return err
	}
	// The billing context is derived server-side from the repo's owning GitHub
	// account — the caller never names one.

	// Boot selection:
	//   --image <sha>  → send image, no ref, fallback irrelevant.
	//   --ref <branch> → normalize to refs/heads/<branch>, fallback=false
	//                    (an explicit ref typo must fail loudly, not boot default).
	//   plain new      → infer the cwd branch's ref, fallback=true (an inferred
	//                    feature branch with no built image quietly uses default).
	//                    Detached HEAD / no checkout → omit ref (sendRef stays "").
	var sendRef string
	var fallback bool
	switch {
	case flags.Image != "":
		// no ref; fallback inert
	case flags.Ref != "":
		sendRef = normalizeRef(flags.Ref)
		fallback = false
	default:
		sendRef = inferBranchRef()
		fallback = true
	}

	// Force-select loop (INV-3: the server never substitutes a missing
	// region/size — it 400s {"error":"<dim>-required","selectable":[…]}).
	// On a TTY, resolve each required dimension with a numbered picker over
	// the server's list and RE-ISSUE the create with the pick filled in; both
	// dimensions may be missing, so this runs at most one picker round per
	// dimension (a dimension re-reported after its pick means the server
	// rejected the re-issue — surface it rather than loop). Non-TTY (CI):
	// list the selectable values and exit non-zero, never substituting.
	pickedRegion, pickedSize := flags.Region, flags.Size
	picked := map[string]bool{}
	var stdin *bufio.Reader
	var res *client.CreateResult
	for {
		rctx, cancel := ctxTimeout(ctx, 30*time.Second)
		res, err = c.Create(rctx, flags.Repo, pickedSize, pickedRegion, sendRef, flags.Image, fallback)
		cancel()
		if err == nil {
			break
		}
		dim, se, ok := requiredCreateDimension(err)
		if !ok {
			return explainCreate(err, flags.Repo)
		}
		if !isTTY() || len(se.Selectable) == 0 || picked[dim] {
			return requiredDimensionErr(dim, se)
		}
		picked[dim] = true
		if stdin == nil {
			// One reader shared across rounds: a bufio.Reader may buffer past
			// its own line, so a second reader over os.Stdin would miss input.
			stdin = bufio.NewReader(os.Stdin)
		}
		choice, perr := pickerPrompt(stdin, dim, se)
		if perr != nil {
			return perr
		}
		if dim == "region" {
			pickedRegion = choice
		} else {
			pickedSize = choice
		}
	}
	fmt.Printf("Created %s (%s, %s). Connecting…\n", res.WorkspaceID, flags.Repo, describeResolved(res))
	if line := describeSpawnDefaults(res); line != "" {
		fmt.Println(line)
	}
	return connect(ctx, c, res.WorkspaceID, connectOpts{})
}

// isTTY reports whether the CLI is running interactively — both stdin AND
// stdout are terminals (the same gate cmdLogin uses for the device-flow UI).
// A package var so tests can force either arm: the test harness's stdin/
// stdout are files/pipes, never real terminals, so the true term.IsTerminal
// gate cannot be exercised there.
var isTTY = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// requiredCreateDimension classifies a Create error as one of the
// force-select 400s ({"error":"region-required"|"size-required",…}). ok is
// true only for those two codes; the decoded body rides along for the picker
// (TTY) or the listing error (non-TTY). Everything else — including the
// explicit-invalid …-not-available codes — goes to explainCreate.
func requiredCreateDimension(err error) (string, client.SelectableError, bool) {
	var ae *client.APIError
	if !asAPIError(err, &ae) {
		return "", client.SelectableError{}, false
	}
	se, decoded := client.DecodeSelectableError(ae.Body)
	if !decoded {
		return "", client.SelectableError{}, false
	}
	switch se.Code {
	case "region-required":
		return "region", se, true
	case "size-required":
		return "size", se, true
	}
	return "", se, false
}

// requiredDimensionErr renders a force-select 400 as a terminal error (the
// non-TTY arm, plus the TTY dead ends: an empty selectable list, or a
// dimension the server re-reports after a pick). main prints it to stderr and
// exits non-zero; the value is never substituted (CI-safe).
func requiredDimensionErr(dim string, se client.SelectableError) error {
	msg := se.Detail
	if msg == "" {
		msg = "a " + dim + " is required"
	}
	if len(se.Selectable) > 0 {
		msg += " — selectable: " + strings.Join(se.Selectable, ", ")
	}
	return fmt.Errorf("%s (re-run with --%s)", msg, dim)
}

// pickerPrompt runs the numbered force-select picker over the server's
// selectable list (a numbered-stdin prompt, in a loop):
// print the server's detail plus a 1-based list, read a selection,
// RE-PROMPT on garbage (a non-number or out-of-range entry), abort on an
// empty line / EOF. It reads from an injected reader rather than os.Stdin
// directly so the loop is unit-testable without a real TTY (the TTY gate
// itself is the isTTY var, decided by the caller).
func pickerPrompt(in *bufio.Reader, dim string, se client.SelectableError) (string, error) {
	detail := se.Detail
	if detail == "" {
		detail = "a " + dim + " is required"
	}
	fmt.Println(detail)
	for i, v := range se.Selectable {
		fmt.Printf("%d) %s\n", i+1, v)
	}
	for {
		fmt.Printf("Select a %s [1-%d]: ", dim, len(se.Selectable))
		line, rerr := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			// Empty line or EOF (Ctrl-D) with nothing entered → abort.
			return "", fmt.Errorf("no %s selected", dim)
		}
		n, cerr := strconv.Atoi(line)
		if cerr != nil || n < 1 || n > len(se.Selectable) {
			fmt.Printf("invalid selection %q — enter a number between 1 and %d\n", line, len(se.Selectable))
			continue // re-prompt; a post-EOF retry reads "" and aborts above
		}
		_ = rerr // a trailing read error is harmless once a valid line was parsed
		return se.Selectable[n-1], nil
	}
}

// describeSpawnDefaults renders the server's per-dimension resolution echo
// for the `new` success output, e.g. "Using region iad (account default) ·
// size shared-4x (repo default)". Region and size resolve independently
// (their sources may differ); a dimension the server didn't echo (an older
// server) is omitted, and the whole line is empty when neither is echoed —
// the caller then prints nothing.
func describeSpawnDefaults(r *client.CreateResult) string {
	render := func(name, value, source string) string {
		s := name + " " + value
		if how := describeSource(source); how != "" {
			s += " (" + how + ")"
		}
		return s
	}
	var parts []string
	if r.Region != "" {
		parts = append(parts, render("region", r.Region, r.RegionSource))
	}
	if r.Size != "" {
		parts = append(parts, render("size", r.Size, r.SizeSource))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Using " + strings.Join(parts, " · ")
}

// describeSource maps a per-dimension resolution-source token to its display
// wording: "explicit" (the flag), "repo default" (the (context, repo)
// refinement), "account default" (the context-wide value). An unknown token
// renders verbatim (forward-compat); empty renders empty (no parenthetical).
func describeSource(source string) string {
	switch source {
	case "explicit":
		return "explicit"
	case "repo":
		return "repo default"
	case "context-wide":
		return "account default"
	}
	return source
}

// cmdSetDefaultRegion / cmdSetDefaultSize set (or clear) a repo's owning
// context's spawn default on the SERVER: the defaults are server-side so they
// stay consistent across the CLI and web UI on every device. Both dimensions
// share setDefaultSetting.
func cmdSetDefaultRegion(ctx context.Context, args []string) error {
	return setDefaultSetting(ctx, args, "default-region")
}

func cmdSetDefaultSize(ctx context.Context, args []string) error {
	return setDefaultSetting(ctx, args, "default-size")
}

// setDefaultSetting drives one POST /api/devbox-settings write. The write
// targets the repo's OWNING context's defaults: --repo names the repo, the
// server derives its owning context and owner/admin-gates the write. A repo is
// REQUIRED — the server rejects a settings write with no repo ("repo is
// required"), so the CLI fails fast before the POST when --repo is absent. An
// empty value argument or --clear clears the default. Both dimensions run an
// ADVISORY pre-flight that warns when the value isn't in the dimension's
// catalog listing (GET /api/regions / GET /api/workspaces/sizes — a typo
// catch); the AUTHORITATIVE gate (including the owner/admin gate) runs
// server-side at the POST — the edge's 4xx (detail + selectable list) is
// surfaced either way.
func setDefaultSetting(ctx context.Context, args []string, setting string) error {
	dim := strings.TrimPrefix(setting, "default-") // "region" | "size"
	var flags struct {
		Repo  string `name:"repo" help:"set the default for this repo (owner/name, a clone URL, or forge:host/owner/name); targets the repo's owning account, owner/admin-gated server-side"`
		Clear bool   `name:"clear" help:"clear the default"`
		Value string `arg:"" optional:"" help:"the default value to set (a region slug or size); omit (or use --clear) to clear"`
	}
	if err := kongx.Parse("set-"+setting, &flags, args); err != nil {
		return err
	}
	// parseInterleaved returned all positionals; kong exposes just the single
	// value positional. Reconstruct the same "any positional present?" shape.
	var pos []string
	if flags.Value != "" {
		pos = []string{flags.Value}
	}
	if flags.Repo == "" {
		return fmt.Errorf("--repo is required")
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	repo, err := repoid.ResolveIdentity(flags.Repo, "")
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()

	// Clear: an explicit --clear, or no value argument.
	if flags.Clear || len(pos) < 1 {
		if err := c.SetDevboxSetting(rctx, repo, setting, "", true); err != nil {
			return err
		}
		fmt.Printf("Default %s cleared (%s).\n", dim, repo)
		return nil
	}

	value := pos[0]
	warnUnknownSettingValue(rctx, c, dim, repo, value)
	if err := c.SetDevboxSetting(rctx, repo, setting, value, false); err != nil {
		return err
	}
	fmt.Printf("Default %s set to %s (%s).\n", dim, value, repo)
	return nil
}

// warnUnknownSettingValue is the ADVISORY pre-flight shared by both
// set-default dimensions (UX only): it reads the dimension's catalog listing
// (`rift regions` / `rift sizes`) and warns on stderr when value isn't in it,
// catching a typo before the POST. The region preview keys on the repo
// (?repo= → the repo's owning context). The authoritative gate runs
// server-side at the POST regardless — an unlisted value is still sent and the
// server's 4xx is surfaced — and a failed catalog read is silently skipped:
// the pre-flight never blocks the write.
func warnUnknownSettingValue(ctx context.Context, c *client.Client, dim, repo, value string) {
	var known []string
	switch dim {
	case "region":
		res, err := c.Regions(ctx, repo)
		if err != nil {
			return
		}
		for _, r := range res.Regions {
			known = append(known, r.Slug)
		}
	case "size":
		cat, err := c.Sizes(ctx)
		if err != nil {
			return
		}
		for _, s := range cat.Sizes {
			known = append(known, s.ID)
		}
	default:
		return
	}
	for _, k := range known {
		if k == value {
			return
		}
	}
	fmt.Fprintf(os.Stderr, "warning: %q is not in `rift %ss` — sending anyway; the server will validate it.\n", value, dim)
}

// cmdSetRepoBuilderSize sets (or clears) the per-repo BUILDER size — the VM
// guest managed image builds for the repo run on. Builds carry no context, so
// this is repo-scoped only (no --context/--repo split); an empty size or
// --clear reverts the repo to the server's global default. Validity is
// authoritative server-side.
func cmdSetRepoBuilderSize(ctx context.Context, args []string) error {
	const usage = "rift set-repo-builder-size <repo> [SIZE | --clear]"
	var flags struct {
		Clear bool   `name:"clear" help:"clear the repo's builder size (revert to the global default)"`
		Repo  string `arg:"" optional:"" help:"repo (owner/name, a clone URL, or forge:host/owner/name)"`
		Size  string `arg:"" optional:"" help:"builder size to set; omit (or use --clear) to revert to the global default"`
	}
	if err := kongx.Parse("set-repo-builder-size", &flags, args); err != nil {
		return err
	}
	// parseInterleaved returned the positionals in order; rebuild that shape from
	// kong's two positional fields (repo, then optional size).
	var pos []string
	if flags.Repo != "" {
		pos = append(pos, flags.Repo)
	}
	if flags.Size != "" {
		pos = append(pos, flags.Size)
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: %s", usage)
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	repo, err := repoid.ResolveIdentity(pos[0], "")
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()

	// Clear: an explicit --clear, or no size argument.
	if flags.Clear || len(pos) < 2 {
		if err := c.SetRepoBuilderSize(rctx, repo, "", true); err != nil {
			return err
		}
		fmt.Printf("Builder size for %s cleared (global default applies).\n", repo)
		return nil
	}
	size := pos[1]
	if err := c.SetRepoBuilderSize(rctx, repo, size, false); err != nil {
		return err
	}
	fmt.Printf("Builder size for %s set to %s.\n", repo, size)
	return nil
}

// describeResolved renders the api's resolved-selection echo for the `new`
// success line: "<ref> @ <short-commit>" (or just the short commit for an
// --image spawn, where resolved_ref is empty), noting a fallback to default.
func describeResolved(r *client.CreateResult) string {
	short := r.ResolvedCommit
	if len(short) > 12 {
		short = short[:12]
	}
	var s string
	if r.ResolvedRef != "" {
		s = r.ResolvedRef + " @ " + short
	} else {
		s = short
	}
	if r.Fallback {
		s += " (fell back to default branch)"
	}
	return s
}

// normalizeRef maps a branch name to its full ref. A value already in
// "refs/heads/<branch>" (or any "refs/..." form) form is left as-is; a bare
// "<branch>" becomes "refs/heads/<branch>". Matches the api/client normalize.
func normalizeRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "refs/") {
		return ref
	}
	return "refs/heads/" + ref
}

// inferBranchRef reads the cwd's current git branch and returns its full
// "refs/heads/<branch>" ref. On a detached HEAD (rev-parse yields "HEAD") or
// any git error (no repo), it returns "" so the caller omits the ref and the
// api resolves the default branch's head.
func inferBranchRef() string {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || branch == "HEAD" {
		return ""
	}
	return "refs/heads/" + branch
}

// explainCreate turns the api's typed errors into actionable messages. `repo`
// is the canonical identity the api looked up — naming it lets the developer
// compare against what CI registered.
//
// It branches on the response BODY's `error` CODE (the boot-selection and
// spawn-dimension codes below), not just the HTTP status: a 409 can now be
// any of image-not-ready / image-not-ready-for-ref / ambiguous-image, each
// with its own message and carried data (available_refs / candidates), and a
// 400 can be an explicit-invalid region/size (…-not-available, carrying the
// selectable list). The force-select …-required codes never reach here — the
// cmdNew create loop intercepts them for the picker / listing error. When the
// body has no recognized code (or is undecodable) it falls back to the prior
// status-based messages.
func explainCreate(err error, repo string) error {
	var ae *client.APIError
	if !asAPIError(err, &ae) {
		return err
	}
	var body struct {
		Error         string   `json:"error"`
		AvailableRefs []string `json:"available_refs"`
		Candidates    []string `json:"candidates"`
		Limit         int      `json:"limit"`
	}
	_ = json.Unmarshal([]byte(ae.Body), &body)
	switch body.Error {
	case "workspace-cap-reached":
		return errors.New(workspaceCapMessage(body.Limit))
	case "image-not-ready":
		return fmt.Errorf("no ready image for %s yet — push to the default branch and let CI build it first (new never builds)", repo)
	case "image-not-ready-for-ref":
		msg := fmt.Sprintf("that ref has no built image for %s", repo)
		if len(body.AvailableRefs) > 0 {
			msg += " — built refs: " + strings.Join(body.AvailableRefs, ", ")
		}
		return errors.New(msg)
	case "image-not-found":
		return fmt.Errorf("no image for that commit in %s — check `rift image ls`", repo)
	case "ambiguous-image":
		msg := "that commit prefix is ambiguous"
		if len(body.Candidates) > 0 {
			msg += " — candidates: " + strings.Join(body.Candidates, ", ")
		}
		return errors.New(msg)
	case "image-prefix-too-short":
		return errors.New("--image needs at least 7 hex chars of the commit SHA")
	case "region-not-available", "size-not-available":
		// An EXPLICIT --region/--size the server rejected (unknown or
		// retired): fail with the server's detail + the selectable list —
		// never substituted, and never a picker (the flag was deliberate).
		se, _ := client.DecodeSelectableError(ae.Body)
		return errors.New(se.Message())
	}
	// No recognized image-error code — fall back to status-based messages.
	switch ae.Status {
	case 409:
		return fmt.Errorf("no ready image for %s yet — push to the default branch and let CI build it first (new never builds)", repo)
	case 503:
		return fmt.Errorf("no ready relay in the region — an operator must add relay capacity")
	}
	return err
}

// workspaceCapMessage renders the per-tenant workspace-cap 429 as prose that
// says what to do. Nothing deletes a box automatically any more, so a tenant
// that reaches the quota STAYS there — this refusal is where most users first
// learn the quota exists, and printing the raw JSON leaves them with no move.
//
// It is keyed by its CALLER on the body's `error` string, never on the 429
// status code: the same edge answers 429 `claim-rejected` for a recovered
// claim token whose rejection reason is by construction indistinguishable —
// cap, credit or repo access — so printing this text there would be a guess.
//
// The count comes from the body's `limit`; a body without one (an older
// server) drops the numbers rather than inventing them.
func workspaceCapMessage(limit int) string {
	const remedy = "Remove one to make room:\n  rift ls\n  rift rm <id>"
	if limit <= 0 {
		return "You have reached your box limit, and boxes are not deleted automatically.\n" + remedy
	}
	n := strconv.Itoa(limit)
	return "You have " + n + " of " + n + " boxes, and boxes are not deleted automatically.\n" + remedy
}

func cmdConnect(ctx context.Context, args []string) error {
	var flags struct {
		New     bool   `name:"new" help:"create a fresh session instead of attaching an existing one"`
		Session string `name:"session" help:"attach (or create) the session with this name"`
		ID      string `arg:"" optional:"" help:"workspace id to connect to"`
	}
	if err := kongx.Parse("connect", &flags, args); err != nil {
		return err
	}
	if flags.ID == "" {
		return fmt.Errorf("usage: rift connect [--new] [--session NAME] <id>")
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	return connect(ctx, c, flags.ID, connectOpts{newSession: flags.New, sessionName: flags.Session})
}

// machineTarget resolves the workspace a lifecycle verb acts on when the CLI
// runs in-VM (RIFT_WORKSPACE_ID present). The machine token's subject is the
// VM's own workspace, so the id argument is optional and, when given, must be
// the machine's own id.
func machineTarget(machineID string, args []string) (string, error) {
	if len(args) == 0 || args[0] == machineID {
		return machineID, nil
	}
	return "", fmt.Errorf("in-VM, rift may only act on this workspace (%s), not %s", machineID, args[0])
}

// inVMRefusal is the single wording for "this verb needs a developer login".
// lifecycle() gates every non-`stop` verb on it (the machine token only opens
// the self-service agent routes), and cmdRestart mirrors that gate — restart's
// second half IS `start`, which the same token cannot issue.
func inVMRefusal(verb string) error {
	return fmt.Errorf("rift %s is not available in-VM — run it from your laptop (machine tokens may only stop/resize/keepalive their own workspace)", verb)
}

func lifecycle(ctx context.Context, args []string, verb string) error {
	// `stop` is the only lifecycle verb with a flag: --cold forces the cold
	// tier, discarding the RAM image so the next start is a full boot. Parsing
	// start/rm against a struct that has NO Cold field is what makes
	// `rift start --cold` an error rather than a silently ignored flag.
	var id string
	var cold bool
	if verb == "stop" {
		var flags struct {
			Cold bool   `name:"cold" help:"park cold: discard the RAM image, so the next start is a full boot"`
			ID   string `arg:"" optional:"" help:"workspace id to stop"`
		}
		if err := kongx.Parse(verb, &flags, args); err != nil {
			return err
		}
		id, cold = flags.ID, flags.Cold
	} else {
		var flags struct {
			ID string `arg:"" optional:"" help:"workspace id"`
		}
		if err := kongx.Parse(verb, &flags, args); err != nil {
			return err
		}
		id = flags.ID
	}
	// machineTarget wants the raw positional args; kong exposes just the single
	// optional id positional, so reconstruct the equivalent slice.
	var posArgs []string
	if id != "" {
		posArgs = []string{id}
	}

	c, cfg, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	if cfg.MachineWorkspaceID != "" {
		// In-VM: the machine token only opens the self-service agent routes.
		if verb != "stop" {
			return inVMRefusal(verb)
		}
		if id, err = machineTarget(cfg.MachineWorkspaceID, posArgs); err != nil {
			return err
		}
		err = c.MachineStop(rctx, id, cold)
	} else {
		if id == "" {
			if verb == "stop" {
				return fmt.Errorf("usage: rift stop [--cold] <id>")
			}
			return fmt.Errorf("usage: rift %s <id>", verb)
		}
		switch verb {
		case "stop":
			err = c.Stop(rctx, id, cold)
		case "start":
			err = explainStart(c.Start(rctx, id), id)
		case "rm":
			err = c.Destroy(rctx, id)
		}
	}
	if err == nil {
		if cold {
			fmt.Printf("%s: stop (cold)\n", id)
		} else {
			fmt.Printf("%s: %s\n", id, verb)
		}
	}
	return err
}

// explainStart DELIBERATELY does not wrap with %w. Wrapping would make the
// underlying *client.APIError reachable via errors.As — but main renders a
// returned error verbatim, so it would also append ": HTTP 409: {…}" to every
// line below, which is precisely the raw body §4.3 requires these messages to
// replace. The unwrapping is the point, not an oversight.
//
// explainStart maps `rift start`'s 409 onto copy a user can act on, and is
// keyed on the body's stable `error` code rather than on the status.
//
// The one that matters is `ineligible-status`: a start can race the un-park
// callback's corrective, which flips a rest-state row to `stopping` and
// deliberately mirrors NOTHING — so `rift ls` still shows the box at rest while
// the module answers `ineligible-status`. Copy along the lines of "the box is
// not stopped" would read as a lie against the list the user just looked at;
// the truth is that the box is mid-transition and the same command works a few
// seconds later. `running` and the three teardown statuses get their own wording
// so the mid-transition line is never printed over a box that is not in one.
// Everything else — including the pre-ready statuses (`provisioning`,
// `provisioned`, `starting`) and `resizing` — takes the mid-transition line.
// That is exact for a park/resume race and approximate for a box coming up for
// the first time, where the box is mid-SOMETHING and the retry advice holds
// either way; sharpening it would mean naming five more statuses in copy whose
// whole point is to stop enumerating them at the user.
func explainStart(err error, id string) error {
	code, detail, ok := lifecycleConflict(err)
	if !ok || code != "ineligible-status" {
		// Not a 409, or one of the other lifecycle rejections the un-park can
		// answer with (no-healthy-relay, superseded) — those are real answers
		// about capacity, not a race, and must not be dressed as one.
		return err
	}
	// The edge's detail is "workspace status: <status>"; the status is worth
	// showing, but its absence must not cost the guidance.
	status := strings.TrimSpace(strings.TrimPrefix(detail, "workspace status:"))
	switch status {
	case "running":
		return fmt.Errorf("%s is already running", id)
	case "ending", "destroying", "done":
		return fmt.Errorf("%s is %s — it is being torn down and cannot be started", id, status)
	}
	if status == "" || status == detail {
		return fmt.Errorf("%s is mid-transition — a park or resume is still settling. Retry in a few seconds: rift start %s", id, id)
	}
	return fmt.Errorf("%s is mid-transition (the server sees %s) — a park or resume is still settling. Retry in a few seconds: rift start %s", id, status, id)
}

// --- restart -------------------------------------------------------------
//
// `rift restart` is a CLIENT-SIDE COMPOSE — a cold stop, then a start — not a
// server operation. A restart that was a bare `start` would resume the very RAM
// snapshot a restart exists to discard (the provider invalidates a snapshot
// only when the machine is *stopped*), so the cold stop is the load-bearing
// half; the server has no `restart` verb and gains none.
//
// The compose reads the box's status once and dispatches on it. The dispatch
// table is total over every status a row can hold: `running`/`suspended`/
// `stopped` dispatch immediately, the four park transients and the three
// pre-ready states poll to a settled status first, and the three teardown
// states error — a teardown wins over a revival.

// restartTerminalStates abort EVERY poll phase, not just the pre-dispatch
// ones: a concurrent `rift rm` can take a box out of a park transient, out of
// pre-ready, or out of `stopped` while this compose waits to start it, and a
// poll set that does not name these three burns its whole deadline waiting for
// a settle that will never come.
var restartTerminalStates = map[string]bool{"ending": true, "destroying": true, "done": true}

// restartSettledStates are the statuses the two pre-dispatch phases wait for —
// and exactly the statuses the table dispatches on. The rest-state members are
// what keeps the pre-ready phase honest: the bring-up-deadline park takes a box
// that never came up to `stopping` → `stopped`, so a `running`-or-terminal poll
// set would hang on precisely that cohort.
var restartSettledStates = map[string]bool{"running": true, "stopped": true, "suspended": true}

// restartWaitStates are the seven statuses the compose WAITS OUT instead of
// dispatching on. Two rationales, one behavior — and one map, because nothing
// reads them apart:
//   - the four park/realization transients (stopping, suspending, resuming,
//     resizing) settle on their own, so dispatching now buys only a 409;
//   - the three pre-ready states (provisioning, provisioned, starting) are a box
//     still coming up, and there is no partly-booted state to restart FROM.
//
// Either way the compose polls to learn where the box lands, then dispatches on
// that.
var restartWaitStates = map[string]bool{
	"stopping": true, "suspending": true, "resuming": true, "resizing": true,
	"provisioning": true, "provisioned": true, "starting": true,
}

// restartPhaseDeadline bounds each of the four poll phases independently, and
// restartPollHold is one cursor long-poll iteration (waitRunning's shape).
//
// These are `var` rather than `const` so a test can shorten them to
// milliseconds. That is not a cosmetic choice: as constants, the deadline
// give-up branch is unreachable from any test — no case can wait five minutes —
// and the requirement §4.3 states by name (a give-up in the stop phase must
// print `rift stop --cold <id>`, NEVER `rift start <id>`, which 409s by the same
// guard and sends the user in a circle) would be pinned only on the 409-twice
// path, while the deadline is the likelier give-up in production.
var (
	restartPhaseDeadline = 5 * time.Minute
	restartPollHold      = 40 * time.Second
)

const (
	// restartLegAttempts is one dispatch plus ONE re-entry after a 409. Each
	// leg has its own racing producer — an automatic park can flip
	// `running → stopping` under the stop, and the un-park corrective can flip
	// a rest-state row to `stopping` under the start — so each leg carries its
	// own budget. On the second 409 the compose gives up printing the box's
	// actual status, never the raw transport error.
	restartLegAttempts = 2
)

func cmdRestart(ctx context.Context, args []string) error {
	var flags struct {
		ID string `arg:"" optional:"" help:"workspace id to restart"`
	}
	if err := kongx.Parse("restart", &flags, args); err != nil {
		return err
	}
	c, cfg, err := authedClient()
	if err != nil {
		return err
	}
	// Refused in-VM, mirroring lifecycle()'s gate: restart's second half is
	// `start`, which a machine token may not issue. A box that cold-stopped
	// itself is restarted from a laptop.
	if cfg.MachineWorkspaceID != "" {
		return inVMRefusal("restart")
	}
	if flags.ID == "" {
		return fmt.Errorf("usage: rift restart <id>")
	}
	return restart(ctx, c, flags.ID)
}

// restart runs the compose. Each iteration resolves the current status to one
// the table dispatches on (polling when it is a transient or pre-ready state),
// then issues one leg. The loop is bounded by the two per-leg 409 budgets: every
// iteration either spends one of them or completes a leg.
func restart(ctx context.Context, c *client.Client, id string) error {
	status, err := restartStatus(ctx, c, id)
	if err != nil {
		return err
	}
	stopLeft, startLeft := restartLegAttempts, restartLegAttempts
	for {
		// Phase 1 — resolve to a dispatchable status.
		switch {
		case restartTerminalStates[status]:
			return restartTornDown(id, status)
		case restartWaitStates[status]:
			// Nothing has been dispatched yet, so the give-up line is the whole
			// command: re-running it re-enters the table from wherever the box
			// then is.
			if status, err = restartPoll(ctx, c, id, restartSettledStates, "a settled state", "rift restart "+id); err != nil {
				return err
			}
		case restartSettledStates[status]:
			// Dispatchable as-is.
		default:
			return restartNoMove(id, status)
		}

		// Phase 2 — dispatch one leg.
		switch status {
		case "running", "suspended":
			// From `suspended` this is the cold-DOWN: the wedged snapshot can
			// only be discarded by a stop, so an already-parked box still takes
			// this leg.
			rctx, cancel := ctxTimeout(ctx, 30*time.Second)
			err = c.Stop(rctx, id, true)
			cancel()
			if isIneligibleStatus(err) {
				// An automatic park won the race between the status read and
				// this call, and no park is accepted from a park transient.
				stopLeft--
				if stopLeft <= 0 {
					return restartConflict(ctx, c, id, "cold stop", "rift stop --cold "+id)
				}
				if status, err = restartStatus(ctx, c, id); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			fmt.Printf("%s: stopping (cold — the RAM image is discarded)…\n", id)
			// The target is `stopped` EXACTLY, never "a rest state":
			// `suspended` is one, and starting from it would resume the very
			// snapshot this restart is discarding. The give-up line is
			// `rift stop --cold`, NOT `rift start` — the un-park guard admits
			// only rest states, so a start issued into a park transient 409s.
			if status, err = restartPoll(ctx, c, id, map[string]bool{"stopped": true}, "stopped", "rift stop --cold "+id); err != nil {
				return err
			}
		case "stopped":
			rctx, cancel := ctxTimeout(ctx, 30*time.Second)
			err = c.Start(rctx, id)
			cancel()
			if isIneligibleStatus(err) {
				// The un-park corrective flips a rest-state row to `stopping`
				// while mirroring nothing, so the status read above (and
				// `rift ls`) can still say `stopped` while the module refuses
				// the start. It settles within one provider round-trip, after
				// which the same start succeeds.
				startLeft--
				if startLeft <= 0 {
					return restartConflict(ctx, c, id, "start", "rift start "+id)
				}
				if status, err = restartStatus(ctx, c, id); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				// Everything else — 402 out of credit, 404, transport — is the
				// server's answer and is surfaced as-is. Note the box is
				// already cold-parked by the time a credit refusal arrives:
				// stopping is what stops the spend, so the stop leg is never
				// credit-gated.
				return err
			}
			fmt.Printf("%s: starting…\n", id)
			if _, err = restartPoll(ctx, c, id, map[string]bool{"running": true}, "running", "rift start "+id); err != nil {
				return err
			}
			fmt.Printf("%s: restarted\n", id)
			return nil
		default:
			// Unreachable: phase 1 leaves only restartSettledStates, which is
			// exactly the two cases above. Here so a later edit that widens one
			// set without the other errors instead of spinning.
			return restartNoMove(id, status)
		}
	}
}

// restartStatus reads the box's status (a snapshot read, no cursor).
func restartStatus(ctx context.Context, c *client.Client, id string) (string, error) {
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	ws, _, err := c.Get(rctx, id, "")
	if err != nil {
		return "", err
	}
	return ws.Status, nil
}

// restartPoll drives one poll phase to a definite outcome: it returns the
// status once it is in want, aborts on a teardown, and on the phase deadline
// gives up printing the box's ACTUAL status plus the command that retries the
// phase that failed (retryCmd — which is why the stop phase passes
// `rift stop --cold`, never `rift start`).
//
// It copies waitRunning's cursor long-poll shape — a snapshot read for the
// cursor, then a hold per iteration — but NOT its exit set, which is not total
// for these phases.
func restartPoll(ctx context.Context, c *client.Client, id string, want map[string]bool, wantDesc, retryCmd string) (string, error) {
	gctx, cancel := ctxTimeout(ctx, 30*time.Second)
	ws, cursor, err := c.Get(gctx, id, "")
	cancel()
	if err != nil {
		return "", err
	}
	status := ws.Status
	deadline := time.Now().Add(restartPhaseDeadline)
	for {
		if want[status] {
			return status, nil
		}
		if restartTerminalStates[status] {
			return "", restartTornDown(id, status)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("workspace %s did not reach %s within %s — it is %s. Retry with: %s",
				id, wantDesc, restartPhaseDeadline, status, retryCmd)
		}
		pctx, pcancel := ctxTimeout(ctx, restartPollHold)
		next, ncursor, err := c.Get(pctx, id, cursor)
		pcancel()
		if err != nil {
			return "", err
		}
		// A hold that timed out with no change returns an empty workspace and
		// the same cursor — re-poll, don't overwrite the status with a zero.
		if next.WorkspaceID != "" {
			status = next.Status
		}
		cursor = ncursor
	}
}

// restartConflict renders the give-up after a dispatched leg was refused
// twice. It re-reads the box so the user sees its ACTUAL status and the command
// that finishes the job, rather than the raw `HTTP 409: {…}` body.
//
// The status is labelled as what the SERVER now reports, not as what `rift ls`
// shows — those are different reads, and in this exact race they disagree. The
// re-read below is GET /api/workspaces/{id}, i.e. the $$workspaces row; `rift ls`
// renders the $$user-workspaces mirror. Row 6's corrective flips the row to
// `stopping` while mirroring nothing, so the row says `stopping` while the list
// still says `stopped`. Printing the row under an "rift ls shows" label would
// send the user to look at something that says otherwise.
//
// A FAILED re-read is its own branch and carries the error. Folding it into the
// "still settling" line asserts the one thing this function cannot know when the
// read is what broke — and it is wrong for every interesting cause: a 404 from a
// concurrent `rift rm`, a 401 from an expired token, and a dropped connection all
// render as "retry in a few seconds", advice that cannot succeed.
func restartConflict(ctx context.Context, c *client.Client, id, leg, retryCmd string) error {
	status, err := restartStatus(ctx, c, id)
	switch {
	case err != nil:
		return fmt.Errorf("workspace %s refused the %s twice, and re-reading its status failed: %w — once that is resolved, finish with: %s", id, leg, err, retryCmd)
	case status == "":
		// Reachable only from a degenerate server: a 200 whose body carries no
		// workspace object decodes to the zero value, and restartStatus cannot
		// tell that from a real empty status. Without this branch that case falls
		// to the default and renders "(the server now reports: )" with nothing
		// after the colon. Pinned by TestRestartConflictCopyEdgeCases.
		return fmt.Errorf("workspace %s refused the %s twice — it is still settling a park or resume. Retry in a few seconds with: %s", id, leg, retryCmd)
	default:
		return fmt.Errorf("workspace %s refused the %s twice — it is still settling a park or resume (the server now reports: %s). Retry in a few seconds with: %s", id, leg, status, retryCmd)
	}
}

// restartNoMove is the dispatch table's total-ness backstop, shared by both
// switches so their wording cannot drift apart.
func restartNoMove(id, status string) error {
	return fmt.Errorf("workspace %s is %s — rift restart has no move from that state; check `rift ls`", id, status)
}

func restartTornDown(id, status string) error {
	return fmt.Errorf("workspace %s is %s — it is being torn down, so it cannot be restarted", id, status)
}

// lifecycleConflict decodes a lifecycle 409 — the shared
// `{"error":<stable code>,"detail":<prose>}` body every lifecycle op answers a
// dropped op with. ok is false for anything that is not a 409.
func lifecycleConflict(err error) (code, detail string, ok bool) {
	var ae *client.APIError
	if err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusConflict {
		return "", "", false
	}
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal([]byte(ae.Body), &body)
	return body.Error, body.Detail, true
}

// isIneligibleStatus reports whether err is the ONE 409 the compose can lose a
// race to: the row was mid-transition when the call landed. The other
// rejections a lifecycle op can answer with — no-healthy-relay, superseded,
// not-idle, size-unchanged — are answers about the world, not races, so they
// surface unretried.
func isIneligibleStatus(err error) bool {
	code, _, ok := lifecycleConflict(err)
	return ok && code == "ineligible-status"
}

func cmdResize(ctx context.Context, args []string) error {
	var flags struct {
		Size string `name:"size" help:"new guest size (required)"`
		ID   string `arg:"" optional:"" help:"workspace id to resize"`
	}
	if err := kongx.Parse("resize", &flags, args); err != nil {
		return err
	}
	// machineTarget wants the raw positional args; kong exposes just the single
	// optional id positional, so reconstruct the equivalent slice.
	var posArgs []string
	if flags.ID != "" {
		posArgs = []string{flags.ID}
	}
	c, cfg, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	var id string
	if cfg.MachineWorkspaceID != "" {
		if flags.Size == "" {
			return fmt.Errorf("usage: rift resize [<id>] --size S")
		}
		if id, err = machineTarget(cfg.MachineWorkspaceID, posArgs); err != nil {
			return err
		}
		err = c.MachineResize(rctx, id, flags.Size)
	} else {
		if flags.ID == "" || flags.Size == "" {
			return fmt.Errorf("usage: rift resize <id> --size S")
		}
		id = flags.ID
		err = c.Resize(rctx, id, flags.Size)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s: resizing to %s (reboots; reconnect when running)\n", id, flags.Size)
	return nil
}

func cmdKeepalive(ctx context.Context, args []string) error {
	var flags struct {
		For time.Duration `name:"for" default:"8h" help:"keep alive for this long"`
		ID  string        `arg:"" optional:"" help:"workspace id to keep alive"`
	}
	if err := kongx.Parse("keepalive", &flags, args); err != nil {
		return err
	}
	// machineTarget wants the raw positional args; kong exposes just the single
	// optional id positional, so reconstruct the equivalent slice.
	var posArgs []string
	if flags.ID != "" {
		posArgs = []string{flags.ID}
	}
	c, cfg, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	var id string
	if cfg.MachineWorkspaceID != "" {
		if id, err = machineTarget(cfg.MachineWorkspaceID, posArgs); err != nil {
			return err
		}
		err = c.MachineKeepalive(rctx, id, flags.For.Milliseconds())
	} else {
		if flags.ID == "" {
			return fmt.Errorf("usage: rift keepalive <id> [--for 8h]")
		}
		id = flags.ID
		err = c.Keepalive(rctx, id, flags.For.Milliseconds())
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s: kept alive for %s\n", id, flags.For)
	return nil
}
