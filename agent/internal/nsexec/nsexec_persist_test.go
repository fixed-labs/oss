package nsexec

// T11 — EnsurePersistAgent: the self-copy that makes the in-target sftp
// re-exec resolvable. The stale-copy fixture is SAME-SIZE/DIFFERENT-BYTES
// (built by copying the test binary and mutating bytes) so the size shortcut
// cannot mask a content-compare regression. The skip-if-identical optimization
// is deliberately NOT asserted (no mtime pinning): an always-atomic-rewrite
// refactor preserves everything consumers observe.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func selfBytes(t *testing.T) []byte {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertPersistCopy(t *testing.T, target string, want []byte) {
	t.Helper()
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("persist copy missing: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("persist copy mode = %v, want 0755", fi.Mode().Perm())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("persist copy differs from the running binary (%d vs %d bytes / content mismatch)", len(got), len(want))
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Fatalf("temp residue left behind: %q", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("target dir entries = %v, want only the agent copy", entries)
	}
}

func TestEnsurePersistAgent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "persist", "rift", "agent")
	n := &NS{Bootstrap: false, PersistAgentPath: target, log: discardLog()}

	// nil receiver: no-op, no error.
	var nilNS *NS
	if err := nilNS.EnsurePersistAgent(); err != nil {
		t.Fatalf("nil-NS EnsurePersistAgent: %v", err)
	}

	// Legacy: no-op — the target dir is untouched. Armed: the SAME fixture
	// with only Bootstrap flipped (below) creates the copy.
	if err := n.EnsurePersistAgent(); err != nil {
		t.Fatalf("legacy EnsurePersistAgent: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(target)); !os.IsNotExist(err) {
		t.Fatalf("legacy mode touched the persist volume (dir stat err = %v, want not-exist)", err)
	}

	// Bootstrap: the copy exists, mode 0755, content == the running binary,
	// no .tmp.* residue.
	n.Bootstrap = true
	if err := n.EnsurePersistAgent(); err != nil {
		t.Fatalf("bootstrap EnsurePersistAgent: %v", err)
	}
	want := selfBytes(t)
	assertPersistCopy(t, target, want)

	// Stale copy, same size / different bytes: must be replaced with the
	// running binary's exact bytes.
	mutated := append([]byte(nil), want...)
	mutated[len(mutated)/2] ^= 0xFF
	if len(mutated) != len(want) || bytes.Equal(mutated, want) {
		t.Fatal("fixture broken: stale copy must be same-size/different-bytes")
	}
	if err := os.WriteFile(target, mutated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := n.EnsurePersistAgent(); err != nil {
		t.Fatalf("repair EnsurePersistAgent: %v", err)
	}
	assertPersistCopy(t, target, want)
}
