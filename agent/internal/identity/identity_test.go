package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeWg is a deterministic WgKeygen: pubkey = "PUB(" + priv + ")".
type fakeWg struct{ genCalls int }

func (f *fakeWg) GenKey() (string, error) {
	f.genCalls++
	return fmt.Sprintf("priv-%d", f.genCalls), nil
}

func (f *fakeWg) PubKey(priv string) (string, error) {
	return "PUB(" + priv + ")", nil
}

func TestEnsureGeneratesOnceAndIsStable(t *testing.T) {
	dir := t.TempDir()
	wg := &fakeWg{}

	first, err := Ensure(dir, wg)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if first.WgPubkey != "PUB(priv-1)" {
		t.Fatalf("wg pubkey: %q", first.WgPubkey)
	}
	if !strings.HasPrefix(first.SSHHostPubkey, "ssh-ed25519 ") {
		t.Fatalf("ssh host pubkey line: %q", first.SSHHostPubkey)
	}

	// Second boot: NOTHING regenerates; pubkeys byte-identical.
	second, err := Ensure(dir, wg)
	if err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if wg.genCalls != 1 {
		t.Fatalf("wg GenKey called %d times across two boots (want 1)", wg.genCalls)
	}
	if second.WgPubkey != first.WgPubkey || second.SSHHostPubkey != first.SSHHostPubkey {
		t.Fatalf("identity changed across boots:\n  %+v\n  %+v", first, second)
	}
}

func TestPrivateMaterialPermissions(t *testing.T) {
	dir := t.TempDir()
	id, err := Ensure(dir, &fakeWg{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, p := range []string{id.WgPrivateKeyPath, id.SSHHostKeyPath} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", p, info.Mode().Perm())
		}
	}
	if filepath.Dir(id.WgPrivateKeyPath) != dir {
		t.Fatalf("wg key outside state dir: %s", id.WgPrivateKeyPath)
	}
}

func TestHostPubkeyLineIsValidSSHWireFormat(t *testing.T) {
	dir := t.TempDir()
	id, err := Ensure(dir, &fakeWg{})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	parts := strings.Fields(id.SSHHostPubkey)
	if len(parts) != 2 || parts[0] != "ssh-ed25519" {
		t.Fatalf("line shape: %q", id.SSHHostPubkey)
	}
	wire, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	// two length-prefixed fields: "ssh-ed25519" (11 bytes), key (32 bytes)
	wantLen := 4 + 11 + 4 + ed25519.PublicKeySize
	if len(wire) != wantLen {
		t.Fatalf("wire length %d, want %d", len(wire), wantLen)
	}
	if string(wire[4:15]) != "ssh-ed25519" {
		t.Fatalf("wire alg field: %q", wire[4:15])
	}
}
