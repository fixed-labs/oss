// Package session is the CLI client for the agent's `devbox-session` SSH
// subsystem. The agent runs the
// server; this dials an SSH connection over the already-established WireGuard
// tunnel (NoClientAuth — the box authenticates the laptop by its overlay IP, the
// same posture as the interactive shell) with the box host key pinned, then
// drives the subsystem:
//
//   - List  → one {"op":"list"} frame; the server replies with one JSON line
//     carrying its gen_epoch and the session inventory, then closes the channel.
//   - Attach/New → a pty-req (initial Cols/Rows = the compositor's POST-chrome
//     size) then one {"op":"attach"|"new"} frame; on ok the channel becomes the
//     raw PTY byte stream (scrollback replay, then live output; keystrokes back),
//     which we hand to the compositor as its Inner. Detach = closing the channel.
//
// Framing on the channel: ONE newline-terminated JSON control frame from us, ONE
// newline-terminated JSON response frame from the server, then (attach/new ok)
// raw bytes.
package session

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// subsystemName is the SSH subsystem the agent registers for session control.
const subsystemName = "devbox-session"

// termType reports the TERM to advertise in the pty-req. The box renders into
// the laptop's emulator, so passing the laptop's TERM keeps terminfo coherent;
// fall back to a safe 256-color default.
func termType() string {
	if t := os.Getenv("TERM"); t != "" {
		return t
	}
	return "xterm-256color"
}

// Dialer dials a TCP address on the overlay (the tunnel's netstack DialContext).
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Client is a live SSH connection to the box, used to open session-control
// channels. One Client backs an entire connect (it survives detach/switch — only
// the per-session channel is reopened), and is reopened by the reconnect loop on
// a transport failure.
type Client struct {
	conn ssh.Conn
	cl   *ssh.Client

	// forwardAgent is set when SSH agent forwarding is active on this connection
	// (a std:ssh key resolved AND a local agent was reachable). When true,
	// openStream sends the per-channel auth-agent-req on attach/new so the box
	// vends an SSH_AUTH_SOCK; the connection-level auth-agent@openssh.com handler
	// is already registered (agent.ForwardToAgent in Dial).
	forwardAgent bool

	// localAgentConn is the dialed laptop agent socket, closed on Close.
	localAgentConn net.Conn

	// keepaliveDone stops the dead-peer keepalive goroutine on Close.
	keepaliveDone chan struct{}
	closeOnce     sync.Once
}

// keepalive tuning, mirroring OpenSSH's ServerAliveInterval=15 /
// ServerAliveCountMax=4 → probe every 15s, give up after 4 consecutive misses
// (~60s). On a blackholed transport (laptop sleep, mid-path drop, relay stall)
// with no TCP RST/FIN, the channel read would otherwise block forever and
// composite() never returns; closing the connection unblocks that read so the
// reconnect machinery in run_session.go re-dials.
const (
	keepaliveInterval    = 15 * time.Second
	keepaliveMaxFailures = 4
	keepaliveRequest     = "keepalive@openssh.com"
)

// Dial opens an SSH connection to wgIP:port over the tunnel, pinning the box's
// host key (hostPubkeyAuthorizedKey is the authorized_keys-format line the attach
// bundle carries as ssh_host_pubkey). user is the box login user; the server is
// NoClientAuth, so no client key is offered.
//
// When forwardAgent is true AND a local agent is reachable (SSH_AUTH_SOCK is set
// and dials), the connection-level auth-agent@openssh.com handler is registered
// (agent.ForwardToAgent) so the box's forwarded agent channels route to the
// laptop's agent — the std:ssh real-forwarding path. When no
// local agent is reachable, forwarding is silently skipped (forwardAgent stays
// false on the returned Client).
func Dial(ctx context.Context, dial Dialer, wgIP string, port int, user, hostPubkeyAuthorizedKey string, forwardAgent bool) (*Client, error) {
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostPubkeyAuthorizedKey))
	if err != nil {
		return nil, fmt.Errorf("parse box host key: %w", err)
	}
	addr := net.JoinHostPort(wgIP, fmt.Sprintf("%d", port))
	netConn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            nil, // NoClientAuth server: auth is the overlay IP
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		Timeout:         15 * time.Second,
	}
	// Bound the handshake by ctx: close the raw conn if ctx fires first.
	hsDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = netConn.Close()
		case <-hsDone:
		}
	}()
	sshConn, chans, reqs, err := ssh.NewClientConn(netConn, addr, cfg)
	close(hsDone)
	if err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	c := &Client{
		conn:          sshConn,
		cl:            ssh.NewClient(sshConn, chans, reqs),
		keepaliveDone: make(chan struct{}),
	}
	if forwardAgent {
		c.setupAgentForward()
	}
	go c.keepaliveLoop()
	return c, nil
}

// setupAgentForward dials the laptop's local SSH agent and registers a
// connection-level handler so the box's auth-agent@openssh.com channels route to
// it. Best-effort: with no reachable local agent (SSH_AUTH_SOCK unset or the
// dial fails) forwarding is skipped silently and c.forwardAgent stays false, so
// openStream won't request a (dead) SSH_AUTH_SOCK on the box.
func (c *Client) setupAgentForward() {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return
	}
	if err := agent.ForwardToAgent(c.cl, agent.NewClient(conn)); err != nil {
		_ = conn.Close()
		return
	}
	c.localAgentConn = conn
	c.forwardAgent = true
}

// keepaliveLoop probes the peer on keepaliveInterval with an OpenSSH keepalive
// global request. A blackholed transport gives no reply (and eventually an error
// once the underlying conn is torn down); after keepaliveMaxFailures consecutive
// misses we Close the connection, which unblocks any in-flight channel read so
// the reconnect loop can re-dial. Stops on Close via keepaliveDone.
func (c *Client) keepaliveLoop() {
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	c.runKeepalive(t.C, c.sendKeepalive)
}

// sendKeepalive sends one OpenSSH keepalive global request and reports whether it
// succeeded. SendRequest blocks until the peer replies (WantReply=true) or the
// connection errors; on a blackhole it returns an error once the SSH transport's
// own read times out / the conn is closed.
func (c *Client) sendKeepalive() bool {
	_, _, err := c.conn.SendRequest(keepaliveRequest, true, nil)
	return err == nil
}

// runKeepalive is the loop body, factored out for testing: on each tick it calls
// probe(); after keepaliveMaxFailures consecutive failures it Closes the client
// and returns. Stops on keepaliveDone.
func (c *Client) runKeepalive(tick <-chan time.Time, probe func() bool) {
	misses := 0
	for {
		select {
		case <-c.keepaliveDone:
			return
		case <-tick:
			if probe() {
				misses = 0
			} else {
				misses++
			}
			if misses >= keepaliveMaxFailures {
				_ = c.Close()
				return
			}
		}
	}
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.keepaliveDone != nil {
			close(c.keepaliveDone)
		}
		if c.localAgentConn != nil {
			_ = c.localAgentConn.Close()
		}
	})
	if c.cl != nil {
		return c.cl.Close()
	}
	return nil
}

// SessionInfo is one entry in a List response.
type SessionInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	CreatedAt     int64  `json:"created_at"`
	AttachedCount int    `json:"attached_count"`
}

// ListResult is the server's `list` reply.
type ListResult struct {
	GenEpoch int64         `json:"gen_epoch"`
	Sessions []SessionInfo `json:"sessions"`
}

// controlFrame is the single JSON control frame we write first.
type controlFrame struct {
	Op   string `json:"op"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// attachResp is the server's attach/new reply frame. (The LIST response carries
// gen_epoch — the loss-notice / per-box store read it from there; the attach ack
// does not, so it's absent here.)
type attachResp struct {
	OK    bool   `json:"ok"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

// openChannel opens a raw "session" channel and returns it with its request
// stream. We drive requests (pty-req / subsystem / window-change) and read
// exit-status off this stream directly, rather than via ssh.Session, because
// ssh.Session.Wait() requires a Start()/Run() (which would send an exec/shell
// request the subsystem replaces) — without it Wait() refuses with "session not
// started" and never surfaces the exit code.
func (c *Client) openChannel() (ssh.Channel, <-chan *ssh.Request, error) {
	return c.conn.OpenChannel("session", nil)
}

// List opens the subsystem, sends {"op":"list"}, reads the one JSON reply, and
// closes the channel. No pty-req (list carries no PTY and never resizes anyone).
func (c *Client) List(ctx context.Context) (*ListResult, error) {
	ch, reqs, err := c.openChannel()
	if err != nil {
		return nil, fmt.Errorf("open session channel: %w", err)
	}
	defer ch.Close()
	go ssh.DiscardRequests(reqs)

	if ok, err := ch.SendRequest("subsystem", true, marshalSubsystem(subsystemName)); err != nil || !ok {
		return nil, fmt.Errorf("request %s subsystem: %v (ok=%v)", subsystemName, err, ok)
	}
	if err := writeFrame(ch, controlFrame{Op: "list"}); err != nil {
		return nil, fmt.Errorf("write list frame: %w", err)
	}
	var res ListResult
	if err := readFrameCtx(ctx, ch, &res); err != nil {
		return nil, fmt.Errorf("read list reply: %w", err)
	}
	return &res, nil
}

// Attached is a live attached session: the raw PTY stream as the compositor's
// Inner, plus the server's ack metadata.
type Attached struct {
	*streamInner
	ID   string
	Name string
}

// Attach opens the subsystem with a pty-req (cols x rows = the compositor's
// post-chrome size), sends {"op":"attach","id":id}, reads the ack, and on ok
// returns the raw channel as an Inner. On a server "ok:false" it returns an
// error carrying the server message.
func (c *Client) Attach(ctx context.Context, id string, cols, rows int) (*Attached, error) {
	return c.openStream(ctx, controlFrame{Op: "attach", ID: id}, cols, rows)
}

// New creates and attaches a session named name (empty → the server names it),
// returning the raw channel as an Inner.
func (c *Client) New(ctx context.Context, name string, cols, rows int) (*Attached, error) {
	return c.openStream(ctx, controlFrame{Op: "new", Name: name}, cols, rows)
}

func (c *Client) openStream(ctx context.Context, frame controlFrame, cols, rows int) (*Attached, error) {
	ch, reqs, err := c.openChannel()
	if err != nil {
		return nil, fmt.Errorf("open session channel: %w", err)
	}
	// pty-req carries the INITIAL size; later resizes are window-change requests.
	if ok, err := ch.SendRequest("pty-req", true, marshalPtyReq(termType(), cols, rows)); err != nil || !ok {
		ch.Close()
		return nil, fmt.Errorf("pty-req: %v (ok=%v)", err, ok)
	}
	// Agent forwarding: request it on THIS channel BEFORE the subsystem request.
	// gliderlabs processes channel requests in order on one goroutine and launches
	// the subsystem handler when it sees `subsystem`; sending auth-agent-req first
	// guarantees the box's AgentRequested(sess) is true when the handler reads it.
	// Best-effort: a refusal just means no forwarded SSH_AUTH_SOCK on the box.
	if c.forwardAgent {
		_, _ = ch.SendRequest("auth-agent-req@openssh.com", true, nil)
	}
	if ok, err := ch.SendRequest("subsystem", true, marshalSubsystem(subsystemName)); err != nil || !ok {
		ch.Close()
		return nil, fmt.Errorf("request %s subsystem: %v (ok=%v)", subsystemName, err, ok)
	}
	if err := writeFrame(ch, frame); err != nil {
		ch.Close()
		return nil, fmt.Errorf("write %s frame: %w", frame.Op, err)
	}
	// The ack is one newline-terminated JSON line; after it, the channel is the
	// raw PTY stream. A bufio.Reader straddles the boundary: the ack line and any
	// already-buffered raw bytes follow it, so the Inner reads through the SAME
	// bufio.Reader to avoid losing buffered scrollback.
	br := bufio.NewReader(ch)
	var ack attachResp
	if err := readFrameBuffered(ctx, br, &ack); err != nil {
		ch.Close()
		return nil, fmt.Errorf("read %s ack: %w", frame.Op, err)
	}
	if !ack.OK {
		ch.Close()
		if ack.Error == "" {
			ack.Error = "session refused"
		}
		return nil, fmt.Errorf("%s: %s", frame.Op, ack.Error)
	}
	inner := newStreamInner(ch, br, reqs)
	return &Attached{streamInner: inner, ID: ack.ID, Name: ack.Name}, nil
}

// streamInner adapts a raw SSH channel into compositor.Inner: Read the raw
// stream, Write keystrokes, Resize via window-change, Close = clean detach (the
// server preserves the session). A goroutine consumes the channel's request
// stream to capture the exit-status the agent sends when the session really ends
// (vs. a transport drop, which closes without one).
type streamInner struct {
	ch ssh.Channel
	r  io.Reader

	closeOnce sync.Once
	closeErr  error

	exitMu    sync.Mutex
	exitCode  int
	exitKnown bool
	reqsDone  chan struct{}
}

func newStreamInner(ch ssh.Channel, r io.Reader, reqs <-chan *ssh.Request) *streamInner {
	s := &streamInner{ch: ch, r: r, reqsDone: make(chan struct{})}
	go func() {
		defer close(s.reqsDone)
		for req := range reqs {
			if req.Type == "exit-status" && len(req.Payload) >= 4 {
				code := int(uint32(req.Payload[0])<<24 | uint32(req.Payload[1])<<16 |
					uint32(req.Payload[2])<<8 | uint32(req.Payload[3]))
				s.exitMu.Lock()
				s.exitCode, s.exitKnown = code, true
				s.exitMu.Unlock()
			}
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()
	return s
}

func (s *streamInner) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *streamInner) Write(p []byte) (int, error) { return s.ch.Write(p) }

// Resize sends an SSH window-change for the PTY channel.
func (s *streamInner) Resize(cols, rows int) error {
	_, err := s.ch.SendRequest("window-change", false, marshalWindowChange(cols, rows))
	return err
}

// Close closes the channel — the wire-level detach signal (the server preserves
// the session). Idempotent.
func (s *streamInner) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.ch.Close()
	})
	return s.closeErr
}

// Wait blocks until the channel's request stream is drained (which happens once
// the channel closes), then returns the remote exit code and whether the server
// reported one. Used after a child-exit teardown to surface the agent's "session
// terminated, exit code N". A missing exit status (clean detach / transport
// drop) reports ok=false. It is bounded so a wedged transport can't hang the
// caller's reconnect decision.
func (s *streamInner) Wait() (code int, ok bool) {
	select {
	case <-s.reqsDone:
	case <-time.After(2 * time.Second):
	}
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	return s.exitCode, s.exitKnown
}

// --- RFC 4254 channel-request payloads ---

func marshalString(s string) []byte {
	b := make([]byte, 4+len(s))
	putUint32(b, uint32(len(s)))
	copy(b[4:], s)
	return b
}

func putUint32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func marshalSubsystem(name string) []byte { return marshalString(name) }

// marshalPtyReq builds an RFC 4254 §6.2 pty-req payload: TERM, cols, rows,
// width-px, height-px, and an empty modes string (tty_OP_END only).
func marshalPtyReq(term string, cols, rows int) []byte {
	var b []byte
	b = append(b, marshalString(term)...)
	b = appendUint32(b, uint32(cols))
	b = appendUint32(b, uint32(rows))
	b = appendUint32(b, uint32(cols*8))
	b = appendUint32(b, uint32(rows*8))
	b = append(b, marshalString("\x00")...) // modes: just tty_OP_END (0)
	return b
}

// marshalWindowChange builds an RFC 4254 §6.7 window-change payload.
func marshalWindowChange(cols, rows int) []byte {
	var b []byte
	b = appendUint32(b, uint32(cols))
	b = appendUint32(b, uint32(rows))
	b = appendUint32(b, uint32(cols*8))
	b = appendUint32(b, uint32(rows*8))
	return b
}

func appendUint32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
