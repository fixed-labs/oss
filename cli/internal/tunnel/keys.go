package tunnel

import (
	"crypto/rand"
	"encoding/base64"

	"golang.org/x/crypto/curve25519"
)

// GenerateKeypair makes a fresh Curve25519 WireGuard keypair (base64, wg
// format). The private key is per-session and never leaves the process; the
// public key is what the attach call records as the authorized laptop peer.
// Clamping matches WireGuard's (RFC 7748 X25519).
func GenerateKeypair() (privB64, pubB64 string, err error) {
	var priv [32]byte
	if _, err = rand.Read(priv[:]); err != nil {
		return "", "", err
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64

	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv[:]),
		base64.StdEncoding.EncodeToString(pub), nil
}
