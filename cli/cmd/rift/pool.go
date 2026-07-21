package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
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
	fs := flag.NewFlagSet("pool ls", flag.ContinueOnError)
	org := fs.String("org", "", "show config for this org (name or id) instead of personal context")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Positional: an optional REPO arg for the status (live-count) view.
	var repoArg string
	if fs.NArg() > 0 {
		repoArg = fs.Arg(0)
	}
	if repoArg != "" && *org != "" {
		return fmt.Errorf("rift pool ls: --org and a REPO argument are mutually exclusive")
	}

	c, _, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()

	result, err := c.PoolLs(rctx, repoArg, *org)
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
		fmt.Printf("%-30s  %-14s  %-12s  %-4s  %3s %3s %3s %3s  %s\n",
			"REPO", "REF", "REGION", "SIZE", "DES", "AVL", "WRM", "POP", "STATE")
		printPoolRows(result.Repos, true)
		return nil
	}

	// ---- config view (personal or --org) ----
	scope := "personal"
	if *org != "" {
		scope = "org " + *org
	}
	if len(result.Repos) == 0 {
		fmt.Printf("No warm pools configured (%s).\n", scope)
	} else {
		if result.Cap > 0 {
			fmt.Printf("Cap: %d used / %d total\n\n", result.Total, result.Cap)
		}
		fmt.Printf("%-30s  %-14s  %-12s  %-4s  %3s\n",
			"REPO", "REF", "REGION", "SIZE", "DES")
		printPoolRows(result.Repos, false)
	}

	// ---- admin-org listing (personal view only, for --org discovery) ----
	if *org == "" && repoArg == "" && len(result.AdminOf) > 0 {
		fmt.Println()
		fmt.Println("Orgs you admin (use --org <name-or-id> to view their pools):")
		for _, m := range result.AdminOf {
			fmt.Printf("  %-36s  %s\n", m.ID, m.Name)
		}
	}
	return nil
}

// printPoolRows renders the repos map.  showLive adds the available/warming/popped/state columns.
func printPoolRows(repos map[string]client.PoolRepoEntry, showLive bool) {
	for repo, refs := range repos {
		for ref, regions := range refs {
			for region, sizes := range regions {
				for size, entry := range sizes {
					if showLive {
						fmt.Printf("%-30s  %-14s  %-12s  %-4s  %3d %3d %3d %3d  %s\n",
							trunc(repo, 30), trunc(ref, 14), trunc(region, 12), trunc(size, 4),
							entry.Desired, entry.Available, entry.Warming, entry.Popped,
							entry.State)
					} else {
						fmt.Printf("%-30s  %-14s  %-12s  %-4s  %3d\n",
							trunc(repo, 30), trunc(ref, 14), trunc(region, 12), trunc(size, 4),
							entry.Desired)
					}
				}
			}
		}
	}
}

// trunc truncates s to at most n runes, adding "…" if shortened.
func trunc(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}

// --- pool set ---

func poolSet(ctx context.Context, args []string) error {
	const usage = "usage: rift pool set <repo> <ref> <region> <size> <count>"
	fs := flag.NewFlagSet("pool set", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 5 {
		return fmt.Errorf(usage)
	}
	repoRaw, ref, region, size, countStr := fs.Arg(0), fs.Arg(1), fs.Arg(2), fs.Arg(3), fs.Arg(4)
	// Watched refs are stored in full "refs/heads/<branch>" form (what the
	// server's ref-watched check + claim pop key on), so normalize a bare
	// branch here too — matches `rift new --ref` and the server's own normalize.
	ref = normalizeRef(ref)
	count := 0
	if _, err := fmt.Sscanf(countStr, "%d", &count); err != nil || count < 0 {
		return fmt.Errorf("count must be a non-negative integer (got %q)\n%s", countStr, usage)
	}
	repo, err := resolveRepoIdentity(repoRaw, "")
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
	fs := flag.NewFlagSet("pool rm", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 4 {
		return fmt.Errorf(usage)
	}
	repoRaw, ref, region, size := fs.Arg(0), fs.Arg(1), fs.Arg(2), fs.Arg(3)
	// Normalize the ref to full "refs/heads/<branch>" form so a removal keyed on
	// a bare branch still matches the stored (normalized) tuple key.
	ref = normalizeRef(ref)
	repo, err := resolveRepoIdentity(repoRaw, "")
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
