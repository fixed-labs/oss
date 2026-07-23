package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fixed-labs/oss/cli/clikit/kongx"
	"github.com/fixed-labs/oss/cli/clikit/table"
	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/repoid"
)

// cmdPool is the `rift pool` command group: manage per-(repo, ref, region, size)
// warm-VM pools. Sub-pools are owned by the repo's derived billing context
// (Personal for personal-account repos; Company for org repos — server-resolved,
// never caller-named). All reads/writes go through GET/POST /api/pool.
//
//	pool ls [REPO]                           personal config, or repo live counts, or --org config
//	pool set <repo> <ref> <region> <size> N  upsert a (repo,ref,region,size,count) tuple
//	pool rm  <repo> <ref> <region> <size>    remove (= set count 0)
func cmdPool(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: rift pool ls [REPO] | set <repo> <ref> <region> <size> <count> | rm <repo> <ref> <region> <size>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return poolLs(ctx, rest)
	case "set":
		return poolSet(ctx, rest)
	case "rm", "remove":
		return poolRm(ctx, rest)
	default:
		return fmt.Errorf("rift pool: unknown subcommand %q (want ls|set|rm)", sub)
	}
}

// --- pool ls ---

func poolLs(ctx context.Context, args []string) error {
	var flags struct {
		Org  string `name:"org" help:"show config for this org (name or id) instead of personal context"`
		Repo string `arg:"" optional:"" help:"an optional REPO arg for the status (live-count) view"`
	}
	if err := kongx.Parse("pool ls", &flags, args); err != nil {
		return err
	}

	// Positional: an optional REPO arg for the status (live-count) view.
	repoArg := flags.Repo
	if repoArg != "" && flags.Org != "" {
		return fmt.Errorf("rift pool ls: --org and a REPO argument are mutually exclusive")
	}

	c, _, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()

	result, err := c.PoolLs(rctx, repoArg, flags.Org)
	if err != nil {
		return explainPoolError(err)
	}

	// ---- frozen banner ----
	if result.Frozen {
		fmt.Println("POOLS PAUSED — payment required. Configured counts shown; 0 running.")
		fmt.Println()
	}

	// ---- repo view (status / live counts) ----
	if repoArg != "" {
		if len(result.Repos) == 0 {
			fmt.Printf("No warm pools configured for %s.\n", repoArg)
			return nil
		}
		t := table.New(os.Stdout,
			"REPO", "REF", "REGION", "SIZE", "DES", "AVL", "WRM", "POP", "STATE").
			WithMaxWidths(30, 14, 12, 4).
			WithRightAlign(4, 5, 6, 7)
		addPoolRows(t, result.Repos, true)
		return t.Flush()
	}

	// ---- config view (personal or --org) ----
	scope := "personal"
	if flags.Org != "" {
		scope = "org " + flags.Org
	}
	if len(result.Repos) == 0 {
		fmt.Printf("No warm pools configured (%s).\n", scope)
	} else {
		if result.Cap > 0 {
			fmt.Printf("Cap: %d used / %d total\n\n", result.Total, result.Cap)
		}
		t := table.New(os.Stdout,
			"REPO", "REF", "REGION", "SIZE", "DES").
			WithMaxWidths(30, 14, 12, 4).
			WithRightAlign(4)
		addPoolRows(t, result.Repos, false)
		if err := t.Flush(); err != nil {
			return err
		}
	}

	// ---- admin-org listing (personal view only, for --org discovery) ----
	if flags.Org == "" && repoArg == "" && len(result.AdminOf) > 0 {
		fmt.Println()
		fmt.Println("Orgs you admin (use --org <name-or-id> to view their pools):")
		for _, m := range result.AdminOf {
			fmt.Printf("  %-36s  %s\n", m.ID, m.Name)
		}
	}
	return nil
}

// addPoolRows appends the repos map as table rows. showLive adds the
// available/warming/popped/state columns. Repo/ref/region/size columns are
// capped by the table's WithMaxWidths (30/14/12/4), matching the prior trunc().
func addPoolRows(t *table.Table, repos map[string]client.PoolRepoEntry, showLive bool) {
	for repo, refs := range repos {
		for ref, regions := range refs {
			for region, sizes := range regions {
				for size, entry := range sizes {
					if showLive {
						t.Row(repo, ref, region, size,
							fmt.Sprintf("%d", entry.Desired),
							fmt.Sprintf("%d", entry.Available),
							fmt.Sprintf("%d", entry.Warming),
							fmt.Sprintf("%d", entry.Popped),
							entry.State)
					} else {
						t.Row(repo, ref, region, size,
							fmt.Sprintf("%d", entry.Desired))
					}
				}
			}
		}
	}
}

// --- pool set ---

func poolSet(ctx context.Context, args []string) error {
	const usage = "usage: rift pool set <repo> <ref> <region> <size> <count>"
	var flags struct {
		Repo   string `arg:"" optional:"" help:"repo (owner/name, a clone URL, or forge:host/owner/name)"`
		Ref    string `arg:"" optional:"" help:"git ref"`
		Region string `arg:"" optional:"" help:"placement/region"`
		Size   string `arg:"" optional:"" help:"guest size"`
		Count  string `arg:"" optional:"" help:"desired warm count (non-negative integer)"`
	}
	if err := kongx.Parse("pool set", &flags, args); err != nil {
		return err
	}
	if flags.Repo == "" || flags.Ref == "" || flags.Region == "" || flags.Size == "" || flags.Count == "" {
		return fmt.Errorf(usage)
	}
	repoRaw, ref, region, size, countStr := flags.Repo, flags.Ref, flags.Region, flags.Size, flags.Count
	// Watched refs are stored in full "refs/heads/<branch>" form (what the
	// server's ref-watched check + claim pop key on), so normalize a bare
	// branch here too — matches `rift new --ref` and the server's own normalize.
	ref = normalizeRef(ref)
	count := 0
	if _, err := fmt.Sscanf(countStr, "%d", &count); err != nil || count < 0 {
		return fmt.Errorf("count must be a non-negative integer (got %q)\n%s", countStr, usage)
	}
	repo, err := repoid.ResolveIdentity(repoRaw, "")
	if err != nil {
		return err
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := c.PoolSet(rctx, repo, ref, region, size, count)
	if err != nil {
		return explainPoolError(err)
	}
	if result.Frozen {
		fmt.Printf("pool set: accepted — pools are paused (payment required); will activate when billing resumes.\n")
	} else {
		fmt.Printf("pool set: %s ref=%s region=%s size=%s count=%d\n", repo, ref, region, size, count)
	}
	return nil
}

// --- pool rm ---

func poolRm(ctx context.Context, args []string) error {
	const usage = "usage: rift pool rm <repo> <ref> <region> <size>"
	var flags struct {
		Repo   string `arg:"" optional:"" help:"repo (owner/name, a clone URL, or forge:host/owner/name)"`
		Ref    string `arg:"" optional:"" help:"git ref"`
		Region string `arg:"" optional:"" help:"placement/region"`
		Size   string `arg:"" optional:"" help:"guest size"`
	}
	if err := kongx.Parse("pool rm", &flags, args); err != nil {
		return err
	}
	if flags.Repo == "" || flags.Ref == "" || flags.Region == "" || flags.Size == "" {
		return fmt.Errorf(usage)
	}
	repoRaw, ref, region, size := flags.Repo, flags.Ref, flags.Region, flags.Size
	// Normalize the ref to full "refs/heads/<branch>" form so a removal keyed on
	// a bare branch still matches the stored (normalized) tuple key.
	ref = normalizeRef(ref)
	repo, err := repoid.ResolveIdentity(repoRaw, "")
	if err != nil {
		return err
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = c.PoolSet(rctx, repo, ref, region, size, 0)
	if err != nil {
		return explainPoolError(err)
	}
	fmt.Printf("pool rm: %s ref=%s region=%s size=%s\n", repo, ref, region, size)
	return nil
}

// --- error rendering ---

// explainPoolError renders structured 4xx bodies from the pool endpoints as
// actionable messages, matching the SelectableError convention used elsewhere
// in the CLI. Codes:
//
//	pool-cap-exceeded  409  {cap, total}  → "pool cap exceeded: N / M configured (…)"
//	ref-not-watched    409  {ref}         → "ref not watched: add it with `rift watch …`"
//	invalid-request    400  {detail}      → the server's count/validation detail, verbatim
//	ambiguous-org-name 409  {matching[]}  → "org name matches multiple — re-run with one of: …"
func explainPoolError(err error) error {
	var ae *client.APIError
	if !asAPIError(err, &ae) {
		return err
	}

	// Try the SelectableError shape first (covers region/size-style errors that
	// may come from the pool edge's own validation pass).
	if se, ok := client.DecodeSelectableError(ae.Body); ok {
		switch se.Code {
		case "pool-cap-exceeded":
			// Body carries cap and total as top-level fields alongside the code.
			var extra struct {
				Cap   int `json:"cap"`
				Total int `json:"total"`
			}
			_ = json.Unmarshal([]byte(ae.Body), &extra)
			msg := "pool cap exceeded"
			if extra.Cap > 0 {
				msg += fmt.Sprintf(": %d configured, cap is %d", extra.Total, extra.Cap)
			}
			if se.Detail != "" {
				msg += " — " + se.Detail
			}
			return errors.New(msg)

		case "ref-not-watched":
			var extra struct {
				Ref string `json:"ref"`
			}
			_ = json.Unmarshal([]byte(ae.Body), &extra)
			ref := extra.Ref
			if ref == "" && se.Detail != "" {
				ref = se.Detail
			}
			msg := "ref not watched"
			if ref != "" {
				msg += " (" + ref + ")"
			}
			msg += " — watch it first with `rift watch <ref>`"
			return errors.New(msg)

		case "ambiguous-org-name":
			// {"error":"ambiguous-org-name","matching":["id1","id2"]}: the org name
			// the caller passed to --org matches more than one company they admin.
			// (Decoded here rather than in the status-only block below, because
			// DecodeSelectableError already claimed this body via its "error" key.)
			var extra struct {
				Matching []string `json:"matching"`
			}
			_ = json.Unmarshal([]byte(ae.Body), &extra)
			msg := "org name matches multiple accounts — re-run with --org and one of:"
			for _, id := range extra.Matching {
				msg += "\n  " + id
			}
			return errors.New(msg)

		default:
			// A bad count comes back as {"error":"invalid-request","detail":
			// "count must be between 0 and 3"}; se.Message() surfaces that detail
			// verbatim. (The server never emits an "invalid-desired" code.)
			return errors.New(se.Message())
		}
	}

	// Undecodable or unrecognized 4xx: surface verbatim.
	if ae.Status >= 400 && ae.Status < 500 {
		return errors.New(strings.TrimSpace(ae.Body))
	}
	return err
}
