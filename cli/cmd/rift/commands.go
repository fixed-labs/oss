package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/config"
)

// cmdLogin runs the device flow and persists the minted bearer. The session
// proves identity only; the acting context is resolved per command and its
// per-device default is set separately by `rift set-default-context`.
func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	apiURL := fs.String("api", os.Getenv("RIFT_API_URL"), "rift API base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *apiURL == "" {
		// Reuse a previously-saved API URL if the flag/env is absent.
		if prev, _ := config.Load(); prev != nil && prev.APIBaseURL != "" {
			*apiURL = prev.APIBaseURL
		}
	}
	if *apiURL == "" {
		return fmt.Errorf("--api <url> is required for the first login")
	}

	c := client.New(*apiURL, "")
	startCtx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	start, err := c.DeviceStart(startCtx)
	if err != nil {
		return fmt.Errorf("starting login: %w", err)
	}
	fmt.Printf("To log in, open:\n\n    %s\n\nand enter the code:  %s\n\nWaiting for approval…\n",
		start.VerificationURL, start.UserCode)

	pollCtx, cancel2 := ctxTimeout(ctx, 10*time.Minute)
	defer cancel2()
	tok, err := c.PollUntilToken(pollCtx, start)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	cfg, _ := config.Load()
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.APIBaseURL = *apiURL
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

// canonicalRepo normalizes a repo identity to the bare "owner/name" canonical
// the whole pipeline keys images by: lowercase owner/name, strip a "github:"
// label, a trailing ".git", and trailing slashes. CI registration and the
// server apply the same rule, so clone-casing
// / renamed-remote / trailing-slash skew can't make a registered image
// unresolvable. "github:owner/name" is accepted as INPUT for back-compat, but
// the canonical (what we send and display) is the bare pair.
func canonicalRepo(s string) (string, error) {
	in := strings.TrimSpace(s)
	rest := in
	if i := strings.Index(in, ":"); i >= 0 && strings.EqualFold(in[:i], "github") {
		rest = in[i+1:]
	}
	rest = strings.TrimRight(rest, "/")
	rest = strings.TrimSuffix(rest, ".git")
	rest = strings.ToLower(strings.TrimRight(rest, "/"))
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("repo must be owner/name, got %q", s)
	}
	return parts[0] + "/" + parts[1], nil
}

// inferRepo derives the canonical repo from the cwd git remote origin.
func inferRepo() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("no git remote (run in a repo, or pass --repo): %w", err)
	}
	return repoFromRemote(strings.TrimSpace(string(out)))
}

// repoFromRemote parses a git remote URL into the canonical repo identity.
// git@github.com:org/name.git  |  https://github.com/org/name(.git)
func repoFromRemote(url string) (string, error) {
	lower := strings.ToLower(url)
	switch {
	case strings.HasPrefix(lower, "git@github.com:"):
		return canonicalRepo(url[len("git@github.com:"):])
	case strings.Contains(lower, "github.com/"):
		return canonicalRepo(url[strings.Index(lower, "github.com/")+len("github.com/"):])
	}
	return "", fmt.Errorf("unrecognized remote %q — pass --repo owner/name", url)
}

func cmdNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	size := fs.String("size", "", "guest size (e.g. shared-2x)")
	region := fs.String("region", "", "Fly region")
	repo := fs.String("repo", "", "repo (owner/name); inferred from cwd if absent")
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
	if *repo == "" {
		if *repo, err = inferRepo(); err != nil {
			return err
		}
	} else if *repo, err = canonicalRepo(*repo); err != nil {
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

	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := c.Create(rctx, *repo, *size, *region, cid, sendRef, *image, fallback)
	if err != nil {
		return explainCreate(err, *repo)
	}
	fmt.Printf("Created %s (%s, %s). Connecting…\n", res.WorkspaceID, *repo, describeResolved(res))
	return connect(ctx, c, res.WorkspaceID, connectOpts{})
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
// It branches on the response BODY's `error` CODE (the set of boot-selection
// error codes below), not just the HTTP status: a 409 can now be any of
// image-not-ready / image-not-ready-for-ref / ambiguous-image, each with its
// own message and carried data (available_refs / candidates). When the body
// has no recognized image-error code (a non-image error, or an undecodable
// body) it falls back to the prior status-based messages.
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
