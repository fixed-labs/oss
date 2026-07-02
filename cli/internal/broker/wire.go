// Package broker is the secret broker: the laptop-side credential
// handler (stateless) and the box-side client that reaches it over the
// WireGuard tunnel.
//
// The box's `devbox run` shim asks the handler for a named secret by NAME; the
// handler resolves it via the static provider, which reads the source on
// the laptop (where the credential lives) and shapes it into the typed
// credential, and returns the env pairs (inject) or the raw bytes (materialize).
// The agent never sees a value, and nothing is written to the box (except the
// deliberate, audited --materialize-to file, which the shim writes — not the
// broker).
//
// Wire protocol: one request, one response, framed as length-prefixed JSON over
// a single TCP connection on the overlay. We own BOTH ends, so the framing is
// deliberately minimal. A credential VALUE is NEVER logged on either side.
package broker

import (
	"encoding/json"
	"fmt"
	"io"
)

// BrokerPort is the fixed well-known overlay port the handler listens on and the
// box-side client dials. Agent-config publication of the port is deferred, so
// today it is pinned here and both ends share this const.
const BrokerPort = 51820

// Mode is the kind of delivery a request asks for.
type Mode string

const (
	// ModeInject asks the handler to resolve the secret and return its typed env
	// pairs (the default `devbox run` path).
	ModeInject Mode = "inject"
	// ModeMaterialize asks the handler for the raw source bytes (the
	// --materialize-to file-only escape hatch).
	ModeMaterialize Mode = "materialize"
)

// Request names one secret and a delivery mode.
type Request struct {
	Secret string `json:"secret"`
	Mode   Mode   `json:"mode"`
}

// EnvPair mirrors secrets.EnvPair on the wire (the broker package does not
// depend on a value type from secrets for its protocol shape).
type EnvPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Response carries the result of a request. Exactly one of Env (inject) or
// Material (materialize) is populated on success; Error (a stable code) is set on
// failure and the others are empty.
type Response struct {
	// Env holds the named env pairs for an inject request.
	Env []EnvPair `json:"env,omitempty"`
	// Material holds the raw source bytes for a materialize request.
	Material []byte `json:"material,omitempty"`
	// Error is a stable machine-readable code (see the Err* constants); empty on
	// success. The handler NEVER puts a credential value in here.
	Error string `json:"error,omitempty"`
	// Detail is a short, value-free human message accompanying Error.
	Detail string `json:"detail,omitempty"`
}

// Stable error codes the handler returns and the box-side client distinguishes.
// These never carry a credential value.
const (
	// ErrUnknownSecret — the secret is unknown, not an inject secret, or unmapped.
	ErrUnknownSecret = "unknown-secret"
	// ErrBadRequest — the request was malformed (unknown mode, empty secret).
	ErrBadRequest = "bad-request"
	// ErrInternal — the handler failed to read/extract the source.
	ErrInternal = "internal"
)

// maxFrame bounds a single JSON frame so a peer can't OOM the other end. A
// secret's source is capped at 1 MiB upstream (secrets.maxSecretBytes); add
// generous slack for base64 + JSON envelope.
const maxFrame = 4 << 20

// writeFrame writes a length-prefixed JSON frame: a 4-byte big-endian length
// followed by the JSON bytes.
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return fmt.Errorf("frame too large (%d bytes)", len(b))
	}
	var hdr [4]byte
	hdr[0] = byte(len(b) >> 24)
	hdr[1] = byte(len(b) >> 16)
	hdr[2] = byte(len(b) >> 8)
	hdr[3] = byte(len(b))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// readFrame reads a length-prefixed JSON frame into v.
func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n < 0 || n > maxFrame {
		return fmt.Errorf("frame length %d out of range", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
