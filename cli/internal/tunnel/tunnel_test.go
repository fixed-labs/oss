package tunnel

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestGenerateKeypairIsValidWg(t *testing.T) {
	priv, pub, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	for name, k := range map[string]string{"priv": priv, "pub": pub} {
		raw, err := base64.StdEncoding.DecodeString(k)
		if err != nil {
			t.Fatalf("%s not base64: %v", name, err)
		}
		if len(raw) != 32 {
			t.Fatalf("%s is %d bytes, want 32", name, len(raw))
		}
	}
	// Clamping: a valid X25519 private key has these bits fixed.
	raw, _ := base64.StdEncoding.DecodeString(priv)
	if raw[0]&7 != 0 || raw[31]&128 != 0 || raw[31]&64 == 0 {
		t.Fatal("private key not clamped per WireGuard")
	}
	// Two generations differ.
	p2, _, _ := GenerateKeypair()
	if priv == p2 {
		t.Fatal("keypairs not random")
	}
}

func TestUAPIConfig(t *testing.T) {
	priv, _, _ := GenerateKeypair()
	_, wsPub, _ := GenerateKeypair()
	cfg, err := Params{
		LaptopPrivateKey:   priv,
		WorkspacePublicKey: wsPub,
		RelayEndpoint:      "1.2.3.4",
		RelayPort:          49152,
		WorkspaceWgIP:      "fd5e:de7b::9",
	}.UAPIConfig()
	if err != nil {
		t.Fatalf("UAPIConfig: %v", err)
	}
	wants := []string{
		"endpoint=1.2.3.4:49152",
		"allowed_ip=fd5e:de7b::9/128",
		"persistent_keepalive_interval=25", // defaulted
		"public_key=",
		"private_key=",
	}
	for _, w := range wants {
		if !strings.Contains(cfg, w) {
			t.Fatalf("config missing %q:\n%s", w, cfg)
		}
	}
	// Keys must be HEX in the UAPI (64 hex chars), not base64.
	for _, line := range strings.Split(cfg, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && (k == "private_key" || k == "public_key") {
			if len(v) != 64 {
				t.Fatalf("%s should be 64 hex chars, got %d: %q", k, len(v), v)
			}
		}
	}
}

func TestUAPIConfigValidation(t *testing.T) {
	priv, _, _ := GenerateKeypair()
	_, pub, _ := GenerateKeypair()
	base := Params{LaptopPrivateKey: priv, WorkspacePublicKey: pub,
		RelayEndpoint: "1.2.3.4", RelayPort: 49152, WorkspaceWgIP: "fd5e::9"}

	noEndpoint := base
	noEndpoint.RelayEndpoint = ""
	if _, err := noEndpoint.UAPIConfig(); err == nil {
		t.Fatal("missing endpoint should error")
	}
	noIP := base
	noIP.WorkspaceWgIP = ""
	if _, err := noIP.UAPIConfig(); err == nil {
		t.Fatal("missing workspace ip should error")
	}
	badKey := base
	badKey.WorkspacePublicKey = "not-base64!!"
	if _, err := badKey.UAPIConfig(); err == nil {
		t.Fatal("bad key should error")
	}
}

func TestQuietTransientNetErrs(t *testing.T) {
	var got []string
	sink := func(format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }
	filtered := quietTransientNetErrs(sink)

	// The real sleep/wake flood line wireguard-go emits — must be dropped.
	filtered("peer(%s) - Failed to send data packets: %v", "Vjuw…L0S0",
		"write udp 0.0.0.0:59434: sendmmsg: network is unreachable")
	// Every transient pattern, dropped regardless of surrounding text.
	for _, p := range transientNetErrPatterns {
		filtered("send failed: %s", p)
	}
	if len(got) != 0 {
		t.Fatalf("transient no-network errors should be dropped, forwarded: %q", got)
	}

	// Genuine errors must pass through verbatim (format applied).
	filtered("Failed to send handshake initiation: %v", "no known endpoint")
	filtered("Invalid MAC of handshake")
	want := []string{
		"Failed to send handshake initiation: no known endpoint",
		"Invalid MAC of handshake",
	}
	if len(got) != len(want) {
		t.Fatalf("real errors: got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("forwarded[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
