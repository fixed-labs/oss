package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	}
	_ = json.Unmarshal([]byte(ae.Body), &body)
	switch body.Error {
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

func lifecycle(ctx context.Context, args []string, verb string) error {
	c, cfg, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	var id string
	if cfg.MachineWorkspaceID != "" {
		// In-VM: the machine token only opens the self-service agent routes.
		if verb != "suspend" {
			return fmt.Errorf("rift %s is not available in-VM — run it from your laptop (machine tokens may only suspend/resize/keepalive their own workspace)", verb)
		}
		if id, err = machineTarget(cfg.MachineWorkspaceID, args); err != nil {
			return err
		}
		err = c.MachineSuspend(rctx, id)
	} else {
		if len(args) < 1 {
			return fmt.Errorf("usage: rift %s <id>", verb)
		}
		id = args[0]
		switch verb {
		case "suspend":
			err = c.Suspend(rctx, id)
		case "resume":
			err = c.Resume(rctx, id)
		case "rm":
			err = c.Destroy(rctx, id)
		}
	}
	if err == nil {
		fmt.Printf("%s: %s\n", id, verb)
	}
	return err
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
