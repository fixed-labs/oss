package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/fixed-labs/oss/cli/clikit/kongx"
	"github.com/fixed-labs/oss/cli/clikit/table"
	"github.com/fixed-labs/oss/cli/internal/repoid"
)

// cmdWatch / cmdUnwatch / cmdWatched are the `rift watch` family (FIX-202
// build-queue throttle): manage which git refs of a repo build devbox images.
// Presence in the watched set is the gate — a pushed ref builds an image iff it
// is watched. All three resolve the repo from the cwd git remote (the canonical
// forge:host/owner/name id, via the shared flow-1 resolution — see
// repoid.Resolve in internal/repoid), overridable with --repo/--forge.
//
//	watch <ref>     start watching a ref (builds its tip immediately)
//	unwatch <ref>   stop watching a ref (future pushes stop building)
//	watched         newest-first table: REF STATUS ADDED-BY AGE

// cmdWatch handles `rift watch <ref> [--repo ..] [--forge ..]`.
func cmdWatch(ctx context.Context, args []string) error {
	return setWatch(ctx, args, true)
}

// cmdUnwatch handles `rift unwatch <ref> [--repo ..] [--forge ..]`.
func cmdUnwatch(ctx context.Context, args []string) error {
	return setWatch(ctx, args, false)
}

// setWatch is the shared body of watch/unwatch: parse the ref + repo flags,
// resolve the repo, and POST the matching endpoint.
func setWatch(ctx context.Context, args []string, watch bool) error {
	verb := "unwatch"
	if watch {
		verb = "watch"
	}
	var flags struct {
		Repo  string `name:"repo" help:"repo (owner/name, a clone URL, or forge:host/owner/name); inferred from cwd if absent"`
		Forge string `name:"forge" help:"forge type of the repo's host (only github this phase)"`
		Ref   string `arg:"" optional:"" help:"the git ref to watch/unwatch"`
	}
	if err := kongx.Parse(verb, &flags, args); err != nil {
		return err
	}
	if flags.Ref == "" {
		return fmt.Errorf("usage: rift %s <ref>", verb)
	}
	ref := flags.Ref
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	r, err := repoid.Resolve(flags.Repo, flags.Forge)
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	if watch {
		err = c.Watch(rctx, r, ref)
	} else {
		err = c.Unwatch(rctx, r, ref)
	}
	if err != nil {
		return err
	}
	if watch {
		fmt.Printf("watching %s on %s\n", ref, r)
	} else {
		fmt.Printf("unwatched %s on %s\n", ref, r)
	}
	return nil
}

// cmdWatched handles `rift watched [--repo ..] [--forge ..]`.
func cmdWatched(ctx context.Context, args []string) error {
	var flags struct {
		Repo  string `name:"repo" help:"repo (owner/name, a clone URL, or forge:host/owner/name); inferred from cwd if absent"`
		Forge string `name:"forge" help:"forge type of the repo's host (only github this phase)"`
	}
	if err := kongx.Parse("watched", &flags, args); err != nil {
		return err
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	r, err := repoid.Resolve(flags.Repo, flags.Forge)
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	items, err := c.ListWatched(rctx, r)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Printf("No watched refs for %s.\n", r)
		return nil
	}
	t := table.New(os.Stdout, "REF", "STATUS", "ADDED-BY", "AGE")
	for _, it := range items {
		t.Row(
			it.Ref,
			it.Status,
			it.AddedBy,
			humanizeAge(it.AddedAt))
	}
	return t.Flush()
}
