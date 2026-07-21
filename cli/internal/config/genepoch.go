package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// genEpochFile is the per-box last-seen gen-epoch store, kept beside config.json
// under the user's config dir. It backs the connect-time "loss notice": when the
// box's current gen-epoch exceeds the value last recorded here, the box
// restarted while we were detached and its in-box sessions were lost.
//
// The map is keyed by workspace-id. A missing file (or a missing key) means we
// have never connected to that box, so there is no prior epoch to compare and no
// loss to report.
//
// This store is deliberately ENV-SHARED across named sessions: it is reached off
// dir() (not the env-routed config.path()), and workspace-ids are unique per
// control plane, so a shared map cannot collide across envs. Accepted risk: two
// concurrent `rift connect`s in different env subshells race the whole-map
// read-modify-write; worst case is one lost epoch → one suppressed loss-notice.
// genepoch_test.go assumes this file is env-agnostic.
const genEpochFile = "gen-epochs.json"

func genEpochPath() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, genEpochFile), nil
}

func loadGenEpochs() (map[string]int64, error) {
	p, err := genEpochPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return map[string]int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]int64{}
	if err := json.Unmarshal(b, &m); err != nil {
		// A corrupt store shouldn't block connect; treat it as empty.
		return map[string]int64{}, nil
	}
	return m, nil
}

// LastGenEpoch returns the last-seen gen-epoch for workspaceID and whether one
// was recorded.
func LastGenEpoch(workspaceID string) (int64, bool, error) {
	m, err := loadGenEpochs()
	if err != nil {
		return 0, false, err
	}
	v, ok := m[workspaceID]
	return v, ok, nil
}

// StoreGenEpoch records workspaceID's current gen-epoch (last-write-wins). It
// reads-modifies-writes the whole map; the store is tiny and connect is not
// concurrent with itself for one box.
func StoreGenEpoch(workspaceID string, epoch int64) error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	m, err := loadGenEpochs()
	if err != nil {
		m = map[string]int64{}
	}
	m[workspaceID] = epoch
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(d, genEpochFile)
	return os.WriteFile(p, b, 0o600)
}
