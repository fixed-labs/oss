package main

import (
	"fmt"
	"os"

	"github.com/fixed-labs/oss/cli/internal/config"
)

// lastGenEpoch returns the per-box last-seen gen-epoch and whether one was
// recorded (a missing store/key means no prior connect, so no loss to report).
func lastGenEpoch(workspaceID string) (int64, bool) {
	v, ok, err := config.LastGenEpoch(workspaceID)
	if err != nil {
		return 0, false
	}
	return v, ok
}

// storeGenEpoch records the box's current gen-epoch; a write failure only
// disables the next loss notice, so it warns rather than fails the connect.
func storeGenEpoch(workspaceID string, epoch int64) error {
	if err := config.StoreGenEpoch(workspaceID, epoch); err != nil {
		fmt.Fprintf(os.Stderr, "rift: could not record session generation: %v\n", err)
		return err
	}
	return nil
}
