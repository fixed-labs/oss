package sessions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// genEpochFile is the file under StateDir holding the agent's process-generation
// counter. StateDir is overlay-backed (the /persist Fly volume), so the value
// survives stop/resume/resize — the basis of the monotonic gen-epoch mechanism.
const genEpochFile = "gen-epoch"

// ReadAndBumpGenEpoch implements the boot-reconcile gen-epoch step (the
// monotonic gen-epoch mechanism).
// On a FRESH process it reads genEpoch=E from stateDir, writes E+1 BEFORE the
// caller reports (so a crash mid-write still advances the gate — it stays
// monotonic), and returns E+1. The caller then POSTs
// TombstoneStaleSessions{gen_epoch: E+1}; `main` is created lazily on the next
// connect stamped E+1, so the strict-less-than tombstone spares the current
// generation. A missing/garbage file reads as E=0.
func ReadAndBumpGenEpoch(stateDir string) (next int64, err error) {
	path := filepath.Join(stateDir, genEpochFile)
	cur := int64(0)
	if b, rerr := os.ReadFile(path); rerr == nil {
		if v, perr := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); perr == nil {
			cur = v
		}
	}
	next = cur + 1
	// Write atomically: tmp + rename, so a crash never leaves a torn value.
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return 0, fmt.Errorf("gen-epoch mkdir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(next, 10)), 0o644); err != nil {
		return 0, fmt.Errorf("gen-epoch write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, fmt.Errorf("gen-epoch rename: %w", err)
	}
	return next, nil
}

// BootReconcile is the full fresh-process reconcile: it bumps the epoch (done by
// the caller via ReadAndBumpGenEpoch, whose result is the Manager's genEpoch)
// and POSTs TombstoneStaleSessions{gen_epoch: m.genEpoch}, removing every
// control-plane session row with gen-epoch < m.genEpoch — all prior
// generations. The map is empty on a fresh process, so this is unconditional
// at startup. Idempotent: re-POSTing the same epoch is a no-op tombstone.
func (m *Manager) BootReconcile(ctx context.Context) error {
	if m.api == nil {
		return nil
	}
	if err := m.api.TombstoneStaleSessions(ctx, m.genEpoch); err != nil {
		m.log.Warn("sessions: TombstoneStaleSessions POST failed", "gen_epoch", m.genEpoch, "err", err)
		return err
	}
	m.log.Info("sessions: boot reconcile tombstoned stale sessions", "gen_epoch", m.genEpoch)
	return nil
}

// contextWithTimeout is a tiny helper so callers needn't import context+time.
func contextWithTimeout(seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
}
