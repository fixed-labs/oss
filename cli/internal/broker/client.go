package broker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strings"
	"time"

	"github.com/fixed-labs/oss/cli/internal/secrets"
)

// dialTimeout bounds the box→laptop dial; the laptop handler is one hop over the
// established tunnel, so a slow/absent response means the laptop session is down.
const dialTimeout = 10 * time.Second

// Errors the box-side client surfaces so `devbox run` can pick a distinct exit
// code and message. ErrUnreachable means there's no live connection (the agent
// must NOT fall back to reading a file); ErrUnknownSecretRemote means the handler
// rejected the named secret; ErrMultipleConnections means more than one
// connection is attached, so the broker can't pick which to dial.
//
// "Connection" here is one `devbox connect` attachment, NOT a laptop and NOT the
// ssh-agent forwarding session: the agent publishes one overlay IP per attachment
// row, and the broker refuses when it sees more than one. A single laptop with
// two live connects trips this just as two laptops would — and, today, so do
// STALE attachment rows from connects that didn't disconnect cleanly (the file is
// derived from the server's attachment records, not from live WireGuard handshakes).
var (
	// ErrUnreachable — could not reach or talk to the broker handler (incl. no
	// connection attached at all).
	ErrUnreachable = errors.New("broker handler unreachable")
	// ErrUnknownSecretRemote — the handler returned unknown-secret.
	ErrUnknownSecretRemote = errors.New("unknown secret")
	// ErrMultipleConnections — more than one connection is attached to this box,
	// so the broker can't tell which to dial (the exactly-one assumption). Kept
	// distinct from ErrUnreachable so `devbox run` can explain stale connections
	// rather than the misleading "your session is down — reconnect".
	ErrMultipleConnections = errors.New("multiple connections attached")
)

// Client is the box-side broker client. It discovers the laptop's overlay IP
// from the agent-published LaptopIPFile and dials the handler over wg0.
// The box uses KERNEL WireGuard,
// so the dial is an ordinary net.Dial routed by the kernel — not the CLI's
// netstack.
type Client struct {
	// LaptopIP is the laptop's overlay IP. If empty, Fetch discovers it via
	// DiscoverLaptopIP (the agent-published LaptopIPFile).
	LaptopIP string
	// Port is the handler port; zero means BrokerPort.
	Port int
	// Dial overrides the dialer (tests inject a loopback/pipe dialer). nil uses a
	// plain net.Dialer over the kernel route.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// FetchInject asks the handler for the named secret's env pairs. Returns the
// pairs, or ErrUnknownSecretRemote / ErrUnreachable.
func (c *Client) FetchInject(ctx context.Context, secret string) ([]secrets.EnvPair, error) {
	resp, err := c.do(ctx, Request{Secret: secret, Mode: ModeInject})
	if err != nil {
		return nil, err
	}
	out := make([]secrets.EnvPair, len(resp.Env))
	for i, p := range resp.Env {
		out[i] = secrets.EnvPair{Name: p.Name, Value: p.Value}
	}
	return out, nil
}

// FetchMaterialize asks the handler for the named secret's raw source bytes (the
// --materialize-to escape hatch).
func (c *Client) FetchMaterialize(ctx context.Context, secret string) ([]byte, error) {
	resp, err := c.do(ctx, Request{Secret: secret, Mode: ModeMaterialize})
	if err != nil {
		return nil, err
	}
	return resp.Material, nil
}

// do performs one request/response exchange and maps the handler's structured
// error onto a typed error.
func (c *Client) do(ctx context.Context, req Request) (Response, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		// dial already classifies: ErrUnreachable for a network/dial failure or no
		// connection, ErrMultipleConnections for >1. Pass it through unchanged.
		return Response{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	if err := writeFrame(conn, req); err != nil {
		return Response{}, fmt.Errorf("%w: send: %v", ErrUnreachable, err)
	}
	var resp Response
	if err := readFrame(conn, &resp); err != nil {
		return Response{}, fmt.Errorf("%w: recv: %v", ErrUnreachable, err)
	}
	switch resp.Error {
	case "":
		return resp, nil
	case ErrUnknownSecret:
		return Response{}, fmt.Errorf("%w: %s", ErrUnknownSecretRemote, resp.Detail)
	default:
		// A bad-request / internal handler error: not a transport failure, but the
		// command can't proceed. Surface the value-free detail as a generic failure.
		return Response{}, fmt.Errorf("broker handler error (%s): %s", resp.Error, resp.Detail)
	}
}

// dial opens a connection to the handler, using the injected Dial in tests or a
// kernel-routed net.Dial to the discovered laptop overlay IP in production.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	port := c.Port
	if port == 0 {
		port = BrokerPort
	}
	if c.Dial != nil {
		// In tests the address is illustrative; the injected dialer ignores it. A
		// dialer error is a transport failure → ErrUnreachable.
		conn, err := c.Dial(ctx, "tcp", net.JoinHostPort(c.laptopIPOrEmpty(), fmt.Sprint(port)))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		return conn, nil
	}
	ip := c.LaptopIP
	if ip == "" {
		var err error
		// Discovery errors are already classified (ErrUnreachable for no connection,
		// ErrMultipleConnections for >1) — pass them through; only the genuine
		// network dial failure below is mapped to ErrUnreachable.
		if ip, err = DiscoverLaptopIP(); err != nil {
			return nil, err
		}
	}
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, fmt.Sprint(port)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return conn, nil
}

func (c *Client) laptopIPOrEmpty() string {
	if c.LaptopIP == "" {
		return "laptop"
	}
	return c.LaptopIP
}

// LaptopIPFile is where the in-VM agent publishes the overlay /128(s) of the
// currently-LIVE connection(s), one bare IP per line (truncated to empty when none
// is live). The broker client reads it to learn where to dial. The agent narrows
// this to peers with a recent WireGuard handshake — re-evaluated on its heartbeat
// cadence, not just on a peer-set change — so a stale strand left by a closed
// `devbox connect` is excluded and ErrMultipleConnections fires only for genuinely
// >1 live connection (the agent's live-laptop-IP publisher narrows the set).
//
// We deliberately do NOT use `wg show wg0 allowed-ips`: reading a WireGuard
// interface needs CAP_NET_ADMIN, but `devbox run` is unprivileged (it runs as
// the box's `dev` user), so `wg show` fails with "Operation not permitted". The
// agent runs as root, is the only component that owns the authorized-peer set,
// and so is the only one that can expose the laptop IP to the unprivileged run
// user — which it does here. Kept in sync with the agent's laptopIPFile
// constant; they are separate Go modules and
// cannot share the constant. A var (not const) so tests can point it at a temp
// file.
var LaptopIPFile = "/run/devbox/laptop-wg-ips"

// DiscoverLaptopIP reads the attached laptop's overlay IP from the agent-
// published LaptopIPFile. Today it assumes exactly one attached laptop. A
// missing or empty file means no laptop
// session is attached (or the agent has not reconciled the peer yet) — a real
// failure surfaced clearly, NOT a fall-through to a root-only `wg show` that
// would just fail again for the unprivileged run user.
func DiscoverLaptopIP() (string, error) {
	data, err := os.ReadFile(LaptopIPFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No file ⇒ no connection attached ⇒ the session is down: classify as
			// ErrUnreachable so the user is told to reconnect (never to read a file).
			return "", fmt.Errorf("%w: no connection attached (%s does not exist)", ErrUnreachable, LaptopIPFile)
		}
		return "", fmt.Errorf("read %s: %w", LaptopIPFile, err)
	}
	return parseLaptopIPs(string(data))
}

// parseLaptopIPs returns the single laptop overlay IP from the published file's
// contents (one bare IP per line; blank or non-IP lines are ignored). Today it
// assumes exactly one attached laptop; 0 or >1 is an error (multi-laptop
// disambiguation rides the deferred agent-config path).
func parseLaptopIPs(s string) (string, error) {
	var ips []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || net.ParseIP(line) == nil {
			continue
		}
		ips = append(ips, line)
	}
	switch len(ips) {
	case 0:
		// Empty file ⇒ no connection attached (agent truncated it on detach): same
		// "session down" semantics as a missing file.
		return "", fmt.Errorf("%w: no connection attached (no IP in %s)", ErrUnreachable, LaptopIPFile)
	case 1:
		return ips[0], nil
	default:
		// More than one connection's overlay IP is published; the broker needs
		// exactly one. Distinct from "unreachable" so `devbox run` can explain stale
		// connections rather than tell the user to reconnect.
		return "", fmt.Errorf("%w: %d connections attached (%s); the broker needs exactly one", ErrMultipleConnections, len(ips), strings.Join(ips, ", "))
	}
}
