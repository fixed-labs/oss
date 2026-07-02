package main

import (
	"context"
	"fmt"
	"time"
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
	fmt.Printf("%-14s  %-30s  %4s  %8s  %s\n", "ID", "NAME", "CPU", "MEM(MB)", "PRICE")
	for _, s := range cat.Sizes {
		id := s.ID
		if s.ID == def {
			id += "*" // mark the effective default row
		}
		fmt.Printf("%-14s  %-30s  %4d  %8d  %s\n", id, s.DisplayName, s.CPU, s.MemoryMB, s.Price)
	}
	if def != "" {
		fmt.Printf("\ndefault: %s\n", def)
	}
	return nil
}
