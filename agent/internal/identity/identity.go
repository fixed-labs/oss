// Package identity owns the machine's first-boot identity: the WireGuard
// keypair and the SSH host key, generated ONCE and persisted under the
// state dir — an overlay path, so identity survives stop/resume/resize.
// Only the PUBLIC halves ever leave the VM (when the agent reports the
// workspace is ready); there is no
// cluster-side key generation and no TOFU/CA for the host key — the CLI
// trusts the host key relayed through the authenticated attach bundle.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WgKeygen abstracts the WireGuard key operations (exec of wireguard-tools in
// production; injected fakes in tests).
type WgKeygen interface {
	// GenKey returns a fresh base64 private key (`wg genkey`).
	GenKey() (string, error)
	// PubKey derives the base64 public key from a private key (`wg pubkey`).
	PubKey(privateKey string) (string, error)
}

// ExecWgKeygen shells out to the `wg` binary (wireguard-tools — part of the
// base-image contract).
type ExecWgKeygen struct{}

func (ExecWgKeygen) GenKey() (string, error) {
	out, err := exec.Command("wg", "genkey").Output()
	if err != nil {
		return "", fmt.Errorf("wg genkey: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (ExecWgKeygen) PubKey(privateKey string) (string, error) {
	cmd := exec.Command("wg", "pubkey")
	cmd.Stdin = strings.NewReader(privateKey + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("wg pubkey: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Identity is the persisted machine identity. Private material stays as
// paths (consumed by `wg set` / the SSH server); public halves are values
// (reported when the agent reports the workspace is ready).
type Identity struct {
	WgPrivateKeyPath string
	WgPubkey         string
	SSHHostKeyPath   string // PKCS8 PEM ed25519
	SSHHostPubkey    string // "ssh-ed25519 <base64>" authorized-keys line
}

// Ensure generates whatever identity material is MISSING and returns the
// full identity. Idempotent: an existing key is never regenerated (the
// reported pubkeys must be byte-stable across every reboot).
func Ensure(stateDir string, wg WgKeygen) (*Identity, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", stateDir, err)
	}
	wgKeyPath := filepath.Join(stateDir, "wg.key")
	wgPriv, err := ensureFile(wgKeyPath, func() (string, error) { return wg.GenKey() })
	if err != nil {
		return nil, err
	}
	wgPub, err := wg.PubKey(wgPriv)
	if err != nil {
		return nil, err
	}

	hostKeyPath := filepath.Join(stateDir, "ssh_host_ed25519_key")
	if _, err := os.Stat(hostKeyPath); os.IsNotExist(err) {
		if err := generateHostKey(hostKeyPath); err != nil {
			return nil, err
		}
	}
	hostPub, err := hostPubkeyLine(hostKeyPath)
	if err != nil {
		return nil, err
	}

	return &Identity{
		WgPrivateKeyPath: wgKeyPath,
		WgPubkey:         wgPub,
		SSHHostKeyPath:   hostKeyPath,
		SSHHostPubkey:    hostPub,
	}, nil
}

// ensureFile returns the file's trimmed content, generating + writing it
// (mode 0600) only when absent.
func ensureFile(path string, gen func() (string, error)) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	v, err := gen()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(v+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return v, nil
}

func generateHostKey(path string) error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return os.WriteFile(path, pemBytes, 0o600)
}

func loadHostKey(path string) (ed25519.PrivateKey, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key", path)
	}
	return priv, nil
}

// hostPubkeyLine renders the host key's public half as an authorized-keys
// style line ("ssh-ed25519 <base64>") — the form the cluster stores and the
// CLI pins. SSH wire format for an ed25519 public key is two length-prefixed
// fields: the algorithm name and the 32 raw key bytes (RFC 8709) — small
// enough to hand-roll, keeping the agent stdlib-only until the SSH server
// lands.
func hostPubkeyLine(path string) (string, error) {
	priv, err := loadHostKey(path)
	if err != nil {
		return "", err
	}
	pub := priv.Public().(ed25519.PublicKey)
	const alg = "ssh-ed25519"
	wire := make([]byte, 0, 4+len(alg)+4+ed25519.PublicKeySize)
	wire = binary.BigEndian.AppendUint32(wire, uint32(len(alg)))
	wire = append(wire, alg...)
	wire = binary.BigEndian.AppendUint32(wire, uint32(len(pub)))
	wire = append(wire, pub...)
	return alg + " " + base64.StdEncoding.EncodeToString(wire), nil
}
