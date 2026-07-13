package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/config"
)

// resolveLoginAPIURL picks the control-plane base URL for `rift login`.
// Precedence: the explicit --api / RIFT_API_URL value (passed in as flagVal),
// then a previously-saved config, then the hosted production default — so a
// first login on a laptop needs no flag while returning users keep whatever
// endpoint they last logged into.
func resolveLoginAPIURL(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if prev, _ := config.Load(); prev != nil && prev.APIBaseURL != "" {
		return prev.APIBaseURL
	}
	return config.DefaultAPIBaseURL
}

// cmdLogin runs the device flow and persists the minted bearer. The session
// proves identity only; the acting context is resolved per command and its
// per-device default is set separately by `rift set-default-context`.
func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	apiFlag := fs.String("api", os.Getenv("RIFT_API_URL"), "rift API base URL")
	noBrowser := fs.Bool("no-browser", false, "do not auto-open the verification URL in a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	apiURL := resolveLoginAPIURL(*apiFlag)

	c := client.New(apiURL, "")
	startCtx, cancelStart := ctxTimeout(ctx, 30*time.Second)
	defer cancelStart()
	start, err := c.DeviceStart(startCtx)
	if err != nil {
		return fmt.Errorf("starting login: %w", err)
	}
	url := start.VerificationURL

	// Printed before raw mode (ordinary \n); the always-present fallback.
	fmt.Printf("To log in, open:\n\n    %s\n\nand enter the code:  %s\n\nWaiting for approval…\n",
		url, start.UserCode)

	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	if shouldAutoOpen(os.Getenv, interactive, *noBrowser) {
		if err := openBrowser(url); err != nil {
			slog.Warn("rift login: could not open browser", "err", err) // → diag logfile
		}
	}

	pollCtx, cancelPoll := ctxTimeout(ctx, 10*time.Minute)
	defer cancelPoll()

	var tok *client.DeviceToken
	if interactive {
		tok, err = pollInteractive(pollCtx, cancelPoll, c, start, url)
	} else {
		tok, err = c.PollUntilToken(pollCtx, start) // today's behavior exactly
	}
	if err != nil {
		if errors.Is(err, errLoginCanceled) {
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
	fmt.Println("Logged in.")
	return nil
}

func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	// --context is a purely client-side display filter over the boxes you own
	// (matched exactly against each row's billed context form-value); it is NOT
	// a server-authorized selector. Absent → show everything you own.
	contextID := fs.String("context", "", "only list boxes in this context (form-value)")
	if err := fs.Parse(args); err != nil {
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
	if *contextID != "" {
		filtered := items[:0]
		for _, it := range items {
			if it.Context == *contextID {
				filtered = append(filtered, it)
			}
		}
		items = filtered
	}
	if len(items) == 0 {
		fmt.Println("No workspaces.")
		return nil
	}
	fmt.Printf("%-38s  %-12s  %-10s  %s\n", "WORKSPACE", "STATUS", "SIZE", "REPO")
	for _, it := range items {
		fmt.Printf("%-38s  %-12s  %-10s  %s\n", it.WorkspaceID, it.Status, it.Size, it.Repo)
	}
	return nil
}

// cmdSetDefaultContext sets the per-device default acting context, written to
// the local config. With an argument (a form-value, for scripting) it validates
// that value by EXACT string equality against GET /api/contexts and stores it;
// with no argument it prints a 1-based numbered picker and reads a selection
// from stdin. Requires a logged-in CLI (it calls GET /api/contexts).
func cmdSetDefaultContext(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("set-default-context", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, cfg, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	items, err := c.Contexts(rctx)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("no contexts available for this account")
	}

	var chosen string
	if fs.NArg() >= 1 {
		// Argument form: validate the given form-value by exact equality against
		// the live list, catching typos / stale company references up front
		// rather than deferring to a `new` 403.
		want := fs.Arg(0)
		for _, it := range items {
			if it.FormValue == want {
				chosen = it.FormValue
				break
			}
		}
		if chosen == "" {
			var b strings.Builder
			for _, it := range items {
				fmt.Fprintf(&b, "\n  %s  (%s)", it.Label, it.FormValue)
			}
			return fmt.Errorf("unknown context %q — valid contexts:%s", want, b.String())
		}
	} else {
		// Interactive form: plain numbered stdin prompt (no third-party TUI).
		for i, it := range items {
			fmt.Printf("%d) %s  (%s)\n", i+1, it.Label, it.FormValue)
		}
		fmt.Printf("Select a context [1-%d]: ", len(items))
		line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			// Empty line or EOF (Ctrl-D) with nothing entered → abort.
			return fmt.Errorf("no selection")
		}
		n, cerr := strconv.Atoi(line)
		if cerr != nil || n < 1 || n > len(items) {
			return fmt.Errorf("invalid selection %q — enter a number between 1 and %d", line, len(items))
		}
		_ = rerr // a trailing read error is harmless once a valid line was parsed
		chosen = items[n-1].FormValue
	}

	cfg.DefaultContext = chosen
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("Default context set to %s.\n", chosen)
	return nil
}

// --- Repo identity: canonical grammar + offline forge resolution (flow 1) ---
//
// A repo is identified by the canonical forge-qualified string
// "<forge>:<host>/<owner>/<name>" (e.g. "github:github.com/acme/widget") — the
// identical grammar the server validates at ingress. The checked-in fixtures
// (testdata/canonical-repo-vectors.json, shared with the server's Clojure
// tests) are the executable contract. Only forge "github" on host github.com
// is serviced this phase; everything else is rejected, never guessed.

// forgeEnum is the closed set of forge-type tokens the grammar recognizes
// (pinned identically in the server implementation). Membership drives the
// already-canonical (form-2) input classification; it does NOT mean the forge
// is serviceable — only "github" validates this phase.
var forgeEnum = map[string]bool{
	"github":       true,
	"gitlab":       true,
	"bitbucket":    true,
	"gitea":        true,
	"forgejo":      true,
	"azure-devops": true,
	"sourcehut":    true,
}

// saasForges is the closed built-in table of well-known SaaS hosts → forge
// type: one of the two explicit forge sources (the other is --forge). An
// unrecognized host is an error, never a guess. github.com only this phase.
var saasForges = map[string]string{"github.com": "github"}

// implementedForges is the set --forge accepts this phase: only "github" is
// serviced end to end. Distinct from forgeEnum (grammar-recognized tokens) —
// an in-enum-but-unimplemented --forge (e.g. gitlab) is rejected with the
// unknown/unsupported-forge error, never passed through to canonicalization
// (which would mis-report a shape problem the input doesn't have).
var implementedForges = map[string]bool{"github": true}

// defaultRepoHost is the host a host-less bare "owner/name" pair resolves to.
const defaultRepoHost = "github.com"

// errRepoInvalid is the pinned rejection for any input canonicalRepo cannot
// parse or validate.
var errRepoInvalid = errors.New("invalid repo — use owner/repo or the full forge:host/owner/repo form")

// GitHub segment character rules, applied after lowercasing: owner has no
// leading/trailing/consecutive hyphens and is ≤39 chars; name is ≤100 chars of
// [a-z0-9._-] and not "." or "..".
var (
	githubOwnerRe = regexp.MustCompile(`^[a-z0-9](-?[a-z0-9])*$`)
	githubNameRe  = regexp.MustCompile(`^[a-z0-9._-]+$`)
)

// decomposeRepo classifies a raw repo input into one of the grammar's four
// forms and splits it into a host authority and a repo path. The order is
// load-bearing (the forms collide on ':'):
//
//  1. contains "://"             → URL: authority up to the first '/', path after.
//  2. leading "<forge-enum>:"    → already-canonical: strip the prefix (the
//     caller's resolved forge is authoritative); authority up to the first '/'.
//  3. a ':' before the first '/' → scp "[user@]host:path" — the colon is a path
//     separator, scp carries NO port ("git@host:2021/repo" is path "2021/repo").
//  4. else                       → bare "owner/name" pair (no host).
//
// hasHost is false only for form 4.
func decomposeRepo(in string) (authority string, hasHost bool, path string) {
	// splitAuthority: authority up to the first '/', path after it.
	splitAuthority := func(rest string) (string, bool, string) {
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[:j], true, rest[j+1:]
		}
		return rest, true, ""
	}
	if i := strings.Index(in, "://"); i >= 0 {
		return splitAuthority(in[i+3:])
	}
	if i := strings.IndexByte(in, ':'); i >= 0 && forgeEnum[strings.ToLower(in[:i])] {
		return splitAuthority(in[i+1:])
	}
	ci := strings.IndexByte(in, ':')
	si := strings.IndexByte(in, '/')
	if ci >= 0 && (si < 0 || ci < si) {
		return in[:ci], true, in[ci+1:]
	}
	return "", false, in
}

// canonicalHost normalizes a host authority — the single host canonicalizer
// used at every parse/compare site: strip an embedded "user[:pass]@",
// lowercase, drop a trailing "/", drop a ":443".
func canonicalHost(authority string) string {
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		authority = authority[i+1:]
	}
	h := strings.ToLower(authority)
	h = strings.TrimSuffix(h, "/")
	h = strings.TrimSuffix(h, ":443")
	return h
}

// canonicalRepo normalizes a repo input (URL, scp, bare pair, or an
// already-canonical string) to the canonical "<forge>:<host>/<owner>/<name>"
// the whole pipeline keys on. forge is the pre-resolved forge type (see
// resolveRepoIdentity); defaultHost applies only when the input carries no
// host (the bare-pair case). Idempotent on canonical "github" input. Only
// forge "github" on host github.com validates this phase — anything else is
// errRepoInvalid.
func canonicalRepo(input, forge, defaultHost string) (string, error) {
	authority, hasHost, path := decomposeRepo(strings.TrimSpace(input))
	host := canonicalHost(defaultHost)
	if hasHost {
		host = canonicalHost(authority)
	}
	f := strings.ToLower(strings.TrimSpace(forge))
	if f != "github" || host != "github.com" {
		return "", errRepoInvalid
	}
	path = strings.TrimRight(path, "/")
	path = strings.TrimSuffix(path, ".git")
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if s == "" {
			return "", errRepoInvalid
		}
		segs[i] = strings.ToLower(s)
	}
	if len(segs) != 2 { // github namespace depth is exactly 1 (owner + name)
		return "", errRepoInvalid
	}
	owner, name := segs[0], segs[1]
	if len(owner) > 39 || !githubOwnerRe.MatchString(owner) {
		return "", errRepoInvalid
	}
	if len(name) > 100 || name == "." || name == ".." || !githubNameRe.MatchString(name) {
		return "", errRepoInvalid
	}
	return f + ":" + host + "/" + owner + "/" + name, nil
}

// resolveRepoIdentity is the CLI's offline flow-1: decompose the input just
// enough to learn the host (a bare pair defaults to defaultRepoHost), resolve
// the forge from exactly one explicit source — (a) the built-in SaaS table,
// (b) an explicit --forge — and then canonicalize. A --forge that conflicts
// with a known SaaS host is an error; an unrecognized host with no --forge is
// an error. Never a guess, never a network call.
func resolveRepoIdentity(input, forgeFlag string) (string, error) {
	in := strings.TrimSpace(input)
	authority, hasHost, _ := decomposeRepo(in)
	host := defaultRepoHost
	if hasHost {
		host = canonicalHost(authority)
	}
	flag := strings.ToLower(strings.TrimSpace(forgeFlag))
	forge, known := saasForges[host]
	switch {
	case known:
		if flag != "" && flag != forge {
			return "", fmt.Errorf("--forge %s conflicts with %s (a %s host)", flag, host, forge)
		}
	case flag != "" && implementedForges[flag]:
		forge = flag
	default:
		// No forge source, or a --forge this phase doesn't service ("this
		// phase accepts only :github") — same pinned error either way.
		return "", fmt.Errorf("unknown/unsupported forge for host %s — pass --forge or register the instance", host)
	}
	return canonicalRepo(in, forge, defaultRepoHost)
}

// resolveRepo returns the canonical repo id: the --repo flag if set, else
// inferred from the cwd git remote — both through the same flow-1 resolution.
func resolveRepo(flagRepo, forgeFlag string) (string, error) {
	if flagRepo != "" {
		return resolveRepoIdentity(flagRepo, forgeFlag)
	}
	return inferRepo(forgeFlag)
}

// inferRepo derives the canonical repo from the cwd git remote origin. Any
// remote shape decomposes (https/ssh URL, scp, already-canonical); whether
// the host is serviceable is flow-1's call — e.g. a gitlab.com remote fails
// with the unknown-forge error, not a URL-shape error.
func inferRepo(forgeFlag string) (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("no git remote (run in a repo, or pass --repo): %w", err)
	}
	return resolveRepoIdentity(strings.TrimSpace(string(out)), forgeFlag)
}

func cmdNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	size := fs.String("size", "", "guest size (e.g. shared-2x)")
	region := fs.String("region", "", "Region (see 'rift regions')")
	repo := fs.String("repo", "", "repo (owner/name, a clone URL, or forge:host/owner/name); inferred from cwd if absent")
	forge := fs.String("forge", "", "forge type of the repo's host (only github this phase); required when the host isn't a known SaaS forge")
	contextID := fs.String("context", "", "acting context (personal:<id> | company:<id>); defaults to your `rift set-default-context`")
	ref := fs.String("ref", "", "boot the head image of this branch (e.g. main); mutually exclusive with --image")
	image := fs.String("image", "", "boot this exact commit's image (full SHA or ≥7-char prefix); mutually exclusive with --ref")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ref != "" && *image != "" {
		return fmt.Errorf("--ref and --image are mutually exclusive")
	}
	c, cfg, err := authedClient()
	if err != nil {
		return err
	}
	if *repo, err = resolveRepo(*repo, *forge); err != nil {
		return err
	}
	cid := *contextID
	if cid == "" {
		cid = cfg.DefaultContext
	}
	if cid == "" {
		return fmt.Errorf("no context set — run `rift set-default-context` or pass --context <ctx>")
	}

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
	case *image != "":
		// no ref; fallback inert
	case *ref != "":
		sendRef = normalizeRef(*ref)
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
	pickedRegion, pickedSize := *region, *size
	picked := map[string]bool{}
	var stdin *bufio.Reader
	var res *client.CreateResult
	for {
		rctx, cancel := ctxTimeout(ctx, 30*time.Second)
		res, err = c.Create(rctx, *repo, pickedSize, pickedRegion, cid, sendRef, *image, fallback)
		cancel()
		if err == nil {
			break
		}
		dim, se, ok := requiredCreateDimension(err)
		if !ok {
			return explainCreate(err, *repo)
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
	fmt.Printf("Created %s (%s, %s). Connecting…\n", res.WorkspaceID, *repo, describeResolved(res))
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
// selectable list (the cmdSetDefaultContext numbered-stdin precedent, in a
// loop): print the server's detail plus a 1-based list, read a selection,
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

// cmdSetDefaultRegion / cmdSetDefaultSize set (or clear) a context-anchored
// spawn default on the SERVER (unlike set-default-context, which writes the
// local config): the defaults are server-side so they stay consistent across
// the CLI and web UI on every device. Both dimensions share setDefaultSetting.
func cmdSetDefaultRegion(ctx context.Context, args []string) error {
	return setDefaultSetting(ctx, args, "default-region")
}

func cmdSetDefaultSize(ctx context.Context, args []string) error {
	return setDefaultSetting(ctx, args, "default-size")
}

// setDefaultSetting drives one POST /api/devbox-settings write. The write is
// scoped to the acting context — resolved exactly as cmdNew resolves it
// (--context flag, else the config's set-default-context value) — and
// optionally refined to one repo via --repo (absent → context-wide). An empty
// value argument or --clear clears the default. Both dimensions run an
// ADVISORY pre-flight that warns when the value isn't in the dimension's
// catalog listing (GET /api/regions / GET /api/workspaces/sizes — a typo
// catch); the AUTHORITATIVE gate runs server-side at the POST — the edge's
// 4xx (detail + selectable list) is surfaced either way.
func setDefaultSetting(ctx context.Context, args []string, setting string) error {
	dim := strings.TrimPrefix(setting, "default-") // "region" | "size"
	fs := flag.NewFlagSet("set-"+setting, flag.ContinueOnError)
	repoFlag := fs.String("repo", "", "scope the default to this repo (owner/name, a clone URL, or forge:host/owner/name); absent → context-wide")
	clear := fs.Bool("clear", false, "clear the default")
	contextID := fs.String("context", "", "acting context (personal:<id> | company:<id>); defaults to your `rift set-default-context`")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	c, cfg, err := authedClient()
	if err != nil {
		return err
	}
	cid := *contextID
	if cid == "" {
		cid = cfg.DefaultContext
	}
	if cid == "" {
		return fmt.Errorf("no context set — run `rift set-default-context` or pass --context <ctx>")
	}
	repo := ""
	if *repoFlag != "" {
		if repo, err = resolveRepoIdentity(*repoFlag, ""); err != nil {
			return err
		}
	}
	scope := "context-wide"
	if repo != "" {
		scope = repo
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()

	// Clear: an explicit --clear, or no value argument.
	if *clear || len(pos) < 1 {
		if err := c.SetDevboxSetting(rctx, cid, repo, setting, "", true); err != nil {
			return err
		}
		fmt.Printf("Default %s cleared (%s, %s).\n", dim, cid, scope)
		return nil
	}

	value := pos[0]
	warnUnknownSettingValue(rctx, c, dim, cid, value)
	if err := c.SetDevboxSetting(rctx, cid, repo, setting, value, false); err != nil {
		return err
	}
	fmt.Printf("Default %s set to %s (%s, %s).\n", dim, value, cid, scope)
	return nil
}

// warnUnknownSettingValue is the ADVISORY pre-flight shared by both
// set-default dimensions (UX only): it reads the dimension's catalog listing
// (`rift regions` / `rift sizes`) and warns on stderr when value isn't in it,
// catching a typo before the POST. The authoritative gate runs server-side at
// the POST regardless — an unlisted value is still sent and the server's 4xx
// is surfaced — and a failed catalog read is silently skipped: the pre-flight
// never blocks the write.
func warnUnknownSettingValue(ctx context.Context, c *client.Client, dim, contextID, value string) {
	var known []string
	switch dim {
	case "region":
		res, err := c.Regions(ctx, contextID)
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
	fs := flag.NewFlagSet("set-repo-builder-size", flag.ContinueOnError)
	clear := fs.Bool("clear", false, "clear the repo's builder size (revert to the global default)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: %s", usage)
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	repo, err := resolveRepoIdentity(pos[0], "")
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()

	// Clear: an explicit --clear, or no size argument.
	if *clear || len(pos) < 2 {
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

// parseInterleaved parses fs's flags wherever they appear in args — before,
// between, or after positionals — and returns the positionals in order. The
// stdlib flag package stops flag parsing at the first positional, which would
// silently IGNORE a trailing flag (`rift set-default-region iad --repo r`
// writing the wrong scope); the documented grammar puts flags after the
// positionals (`rift set-repo-builder-size <REPO> [--clear] <SIZE>`), so this
// loop re-parses past each positional instead. An unknown flag still errors
// loudly wherever it sits — fs.Parse rejects it on the pass that reaches it.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
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
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	newSession := fs.Bool("new", false, "create a fresh session instead of attaching an existing one")
	sessionName := fs.String("session", "", "attach (or create) the session with this name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: rift connect [--new] [--session NAME] <id>")
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	return connect(ctx, c, fs.Arg(0), connectOpts{newSession: *newSession, sessionName: *sessionName})
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
	fs := flag.NewFlagSet("resize", flag.ContinueOnError)
	size := fs.String("size", "", "new guest size (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, cfg, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	var id string
	if cfg.MachineWorkspaceID != "" {
		if *size == "" {
			return fmt.Errorf("usage: rift resize [<id>] --size S")
		}
		if id, err = machineTarget(cfg.MachineWorkspaceID, fs.Args()); err != nil {
			return err
		}
		err = c.MachineResize(rctx, id, *size)
	} else {
		if fs.NArg() < 1 || *size == "" {
			return fmt.Errorf("usage: rift resize <id> --size S")
		}
		id = fs.Arg(0)
		err = c.Resize(rctx, id, *size)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s: resizing to %s (reboots; reconnect when running)\n", id, *size)
	return nil
}

func cmdKeepalive(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("keepalive", flag.ContinueOnError)
	for_ := fs.Duration("for", 8*time.Hour, "keep alive for this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, cfg, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	var id string
	if cfg.MachineWorkspaceID != "" {
		if id, err = machineTarget(cfg.MachineWorkspaceID, fs.Args()); err != nil {
			return err
		}
		err = c.MachineKeepalive(rctx, id, for_.Milliseconds())
	} else {
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: rift keepalive <id> [--for 8h]")
		}
		id = fs.Arg(0)
		err = c.Keepalive(rctx, id, for_.Milliseconds())
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s: kept alive for %s\n", id, for_)
	return nil
}
