package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/fixed-labs/oss/cli/clikit/table"
)

// cmdSizes lists the offered VM sizes (the developer read surface over the
// server's size catalog). It marks
// the effective default — the size a blank `devbox new` resolves to — with a
// trailing "*" on its row and a closing "default: <id>" line.
func cmdSizes(ctx context.Context, args []string) error {
	c, _, err := authedClient()
	if err != nil {
		return err
	}
	rctx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	cat, err := c.Sizes(rctx)
	if err != nil {
		return err
	}
	if len(cat.Sizes) == 0 {
		fmt.Println("No sizes available.")
		return nil
	}
	def := ""
	if cat.EffectiveDefault != nil {
		def = *cat.EffectiveDefault
	}
	t := table.New(os.Stdout, "ID", "NAME", "CPU", "MEM(MB)", "PRICE").
		WithRightAlign(2, 3)
	for _, s := range cat.Sizes {
		id := s.ID
		if s.ID == def {
			id += "*" // mark the effective default row
		}
		t.Row(id, s.DisplayName, fmt.Sprintf("%d", s.CPU), fmt.Sprintf("%d", s.MemoryMB), s.Price)
	}
	if err := t.Flush(); err != nil {
		return err
	}
	if def != "" {
		fmt.Printf("\ndefault: %s\n", def)
	}
	return nil
}
