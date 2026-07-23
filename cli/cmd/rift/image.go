package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fixed-labs/oss/cli/clikit/kongx"
	"github.com/fixed-labs/oss/cli/clikit/table"
	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/repoid"
)

// cmdImage is the `devbox image` command group: inspect and pin the per-repo
// base images CI builds (boot-selection's catalog). All subcommands resolve
// the repo from the cwd git remote (the canonical forge:host/owner/name id,
// via the shared flow-1 resolution — see repoid.Resolve in internal/repoid).
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
	var flags struct {
		Repo  string `name:"repo" help:"repo (owner/name, a clone URL, or forge:host/owner/name); inferred from cwd if absent"`
		Forge string `name:"forge" help:"forge type of the repo's host (only github this phase)"`
		Limit int    `name:"limit" help:"max images to list (0 = server default)"`
	}
	if err := kongx.Parse("image ls", &flags, args); err != nil {
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
	items, err := c.ListImages(rctx, r, flags.Limit)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Printf("No images for %s.\n", r)
		return nil
	}
	t := table.New(os.Stdout, "COMMIT", "REFS", "AGE", "BOXES", "FLAGS")
	for _, it := range items {
		commit := it.Commit
		if len(commit) > 12 {
			commit = commit[:12]
		}
		t.Row(
			commit,
			strings.Join(it.Heads, ","),
			humanizeAge(it.CreatedAt),
			fmt.Sprintf("%d", it.BoxCount),
			imageFlags(it))
	}
	return t.Flush()
}

func imagePin(ctx context.Context, args []string, pin bool) error {
	var flags struct {
		Repo  string `name:"repo" help:"repo (owner/name, a clone URL, or forge:host/owner/name); inferred from cwd if absent"`
		Forge string `name:"forge" help:"forge type of the repo's host (only github this phase)"`
		SHA   string `arg:"" optional:"" help:"the image commit SHA to pin/unpin"`
	}
	if err := kongx.Parse("image pin", &flags, args); err != nil {
		return err
	}
	verb := "pin"
	if !pin {
		verb = "unpin"
	}
	if flags.SHA == "" {
		return fmt.Errorf("usage: rift image %s <sha>", verb)
	}
	commit := flags.SHA
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
