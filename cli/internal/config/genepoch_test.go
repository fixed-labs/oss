package config

import "testing"

// TestGenEpochStore round-trips per-box gen-epochs and proves the loss-notice
// comparison the connect flow relies on: a missing record reports no prior
// value, and a stored value comes back exactly.
func TestGenEpochStore(t *testing.T) {
	// Isolate the config dir to a temp HOME/XDG so the test never touches the
	// developer's real ~/.config/rift.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir) // darwin/UserConfigDir fallback

	// No record yet → ok=false.
	if _, ok, err := LastGenEpoch("ws-1"); err != nil || ok {
		t.Fatalf("fresh store: want (_, false, nil), got ok=%v err=%v", ok, err)
	}

	if err := StoreGenEpoch("ws-1", 7); err != nil {
		t.Fatalf("store: %v", err)
	}
	v, ok, err := LastGenEpoch("ws-1")
	if err != nil || !ok || v != 7 {
		t.Fatalf("after store: want (7, true, nil), got (%d, %v, %v)", v, ok, err)
	}

	// A second box is independent.
	if _, ok, _ := LastGenEpoch("ws-2"); ok {
		t.Fatalf("ws-2 should have no record")
	}

	// Overwrite is last-write-wins.
	if err := StoreGenEpoch("ws-1", 9); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if v, _, _ := LastGenEpoch("ws-1"); v != 9 {
		t.Fatalf("overwrite: want 9, got %d", v)
	}
}
