package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
)

// cmdImage is the `devbox image` command group: inspect and pin the per-repo
// base images CI builds (boot-selection's catalog). All subcommands resolve
// the repo from the cwd git remote (the canonical forge:host/owner/name id,
// via the shared flow-1 resolution — see resolveRepo in commands.go).
//
//	image ls           newest-first table: COMMIT REFS AGE BOXES FLAGS
//	image pin <sha>    mark an image never-reap
//	image unpin <sha>  clear the pin
func cmdImage(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: rift image ls|pin <sha>|unpin <sha>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return imageLs(ctx, rest)
	case "pin":
		return imagePin(ctx, rest, true)
	case "unpin":
		return imagePin(ctx, rest, false)
	default:
		return fmt.Errorf("rift image: unknown subcommand %q (want ls|pin|unpin)", sub)
	}
}

func imageLs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image ls", flag.ContinueOnError)
	repo := fs.String("repo", "", "repo (owner/name, a clone URL, or forge:host/owner/name); inferred from cwd if absent")
	forge := fs.String("forge", "", "forge type of the repo's host (only github this phase)")
	limit := fs.Int("limit", 0, "max images to list (0 = server default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	r, err := resolveRepo(*repo, *forge)
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	items, err := c.ListImages(rctx, r, *limit)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Printf("No images for %s.\n", r)
		return nil
	}
	fmt.Printf("%-14s  %-24s  %-8s  %-5s  %s\n", "COMMIT", "REFS", "AGE", "BOXES", "FLAGS")
	for _, it := range items {
		commit := it.Commit
		if len(commit) > 12 {
			commit = commit[:12]
		}
		fmt.Printf("%-14s  %-24s  %-8s  %-5d  %s\n",
			commit,
			strings.Join(it.Heads, ","),
			humanizeAge(it.CreatedAt),
			it.BoxCount,
			imageFlags(it))
	}
	return nil
}

func imagePin(ctx context.Context, args []string, pin bool) error {
	fs := flag.NewFlagSet("image pin", flag.ContinueOnError)
	repo := fs.String("repo", "", "repo (owner/name, a clone URL, or forge:host/owner/name); inferred from cwd if absent")
	forge := fs.String("forge", "", "forge type of the repo's host (only github this phase)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	verb := "pin"
	if !pin {
		verb = "unpin"
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: rift image %s <sha>", verb)
	}
	commit := fs.Arg(0)
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	r, err := resolveRepo(*repo, *forge)
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	if pin {
		err = c.PinImage(rctx, r, commit)
	} else {
		err = c.UnpinImage(rctx, r, commit)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s: %sned\n", commit, verb)
	return nil
}

// imageFlags renders the FLAGS column: default / pinned, comma-joined (an image
// can be both the default head AND pinned).
func imageFlags(it client.ImageItem) string {
	var flags []string
	if it.Default {
		flags = append(flags, "default")
	}
	if it.Pinned {
		flags = append(flags, "pinned")
	}
	return strings.Join(flags, ",")
}

// humanizeAge renders an epoch-millis registration time as a coarse age (e.g.
// "3d", "5h", "12m", "<1m") relative to now.
func humanizeAge(createdAtMs int64) string {
	if createdAtMs <= 0 {
		return "?"
	}
	d := time.Since(time.UnixMilli(createdAtMs))
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}
