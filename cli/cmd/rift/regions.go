package main

import (
	"context"
	"fmt"
	"time"
)

// cmdRegions lists the selectable regions (the developer read surface over the
// server's region catalog). It marks the effective default — the region a
// blank `rift new` resolves to — with a trailing "*" on its row and a closing
// "default: <slug>" line. If the caller's own pinned default differs from the
// effective default and its row is deprecated, it prints a migrate hint: the
// pin is stale and a blank `rift new` no longer uses it.
//
// No repo is in play, so the preview keys on the caller's own Personal context
// (an empty ?repo=). To preview a specific repo's owning-context defaults, run
// a repo-scoped write dry-run via `rift set-default-region --repo R`.
func cmdRegions(ctx context.Context, args []string) error {
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := c.Regions(rctx, "")
	if err != nil {
		return err
	}
	if len(res.Regions) == 0 {
		fmt.Println("No regions available.")
		return nil
	}
	def := ""
	if res.EffectiveDefault != nil {
		def = *res.EffectiveDefault
	}
	fmt.Printf("%-14s  %-30s  %-11s  %s\n", "SLUG", "NAME", "STATUS", "AVAILABLE")
	for _, r := range res.Regions {
		slug := r.Slug
		if r.Slug == def {
			slug += "*" // mark the effective default row
		}
		avail := "no"
		if r.AvailableNow {
			avail = "yes"
		}
		fmt.Printf("%-14s  %-30s  %-11s  %s\n", slug, r.DisplayName, r.Status, avail)
	}
	if def != "" {
		fmt.Printf("\ndefault: %s\n", def)
	}
	// Migrate hint: the caller's stored pin is stale iff it differs from the
	// effective default AND its own row is deprecated (a still-listed pin the
	// caller can change). effective_default is always selectable, so a differing
	// deprecated pin means a blank `rift new` no longer uses the pin.
	if res.PinnedDefault != nil && *res.PinnedDefault != def {
		for _, r := range res.Regions {
			if r.Slug == *res.PinnedDefault && r.Status == "deprecated" {
				fmt.Printf("\nYour default region %q is deprecated; a blank `rift new` now uses %q.\n"+
					"Run `rift set-default-region <slug>` to pin a current one.\n", *res.PinnedDefault, def)
				break
			}
		}
	}
	return nil
}
