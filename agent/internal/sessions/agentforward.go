package sessions

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// agentProxy gives a long-lived session shell a STABLE SSH_AUTH_SOCK whose
// backing agent is the CURRENTLY-attached forwarding client — the box half of
// the `std:ssh` real-forwarding path.
//
// The problem it solves: the shell is created ONCE and persists across
// attaches/reconnects, so its SSH_AUTH_SOCK env value is fixed at spawn time —
// but the laptop's SSH agent is reachable only through a PER-ATTACH gliderlabs
// agent listener (ssh.NewAgentListener / ForwardAgentConnections) that comes and
// goes with each connection. A stable socket the Manager owns bridges the two:
// the shell always dials the same path; each accepted connection is proxied to
// whichever attach currently holds the forwarding source.
//
// Behavior:
//   - No forwarding-capable client attached → accepted connections are closed
//     immediately, so `ssh`/`git` on the box fail cleanly ("agent refused
//     operation" / no keys) rather than hang. Identical to `ssh -A` after the
//     laptop disconnects.
//   - On attach-with-forwarding the source is (re)pointed at that attach's agent
//     listener socket; on detach it is cleared. A reconnect re-points it to the
//     new connection's listener — so forwarding survives reconnect.
//   - Multi-attach: the source is the most-recent forwarding attach; the socket
//     serves that one connection's agent (documented edge — `ssh -A` likewise
//     has a single agent per shell).
//
// Concurrency: source is swapped under mu and read under mu on every accepted
// connection, so the swap is safe under -race.
type agentProxy struct {
	path string // the stable SSH_AUTH_SOCK path handed to the shell
	ln   net.Listener
	log  *slog.Logger

	mu     sync.Mutex
	source string // current forwarding attach's agent-listener socket ("" = none)
	closed bool
}

// newAgentProxy creates the stable socket under dir, named for the session id,
// and starts its accept loop. The returned proxy's path is the value to export
// as SSH_AUTH_SOCK in the session shell. A nil proxy (with a nil error) is
// returned when dir is empty (forwarding disabled) — callers must nil-check.
//
// uid/gid are the login user the session shell runs as (the setuid credential).
// The proxy is created by the agent process (root), but the shell that dials
// this socket as its SSH_AUTH_SOCK runs unprivileged — so without a chown the
// shell can neither traverse the 0700-root dir nor connect to the root-owned
// socket (EACCES), and agent forwarding silently fails. Chowning both to the
// login user fixes that and matches the discipline the per-attach source-socket
// paths already follow (sshserver setupAgentForward / direct-shell). Pass uid<0
// (or gid<0) to skip the chown — i.e. no setuid credential, so the shell runs as
// the agent (root) and root ownership already works (tests, non-setuid setups).
//
// Single-tenant safety mirrors the source-socket paths: the dir stays 0700, so
// chowning it to the login user narrows access to exactly that user — the only
// other unprivileged uid on a devbox. Re-review if a box ever hosts a second uid.
func newAgentProxy(dir, sessionID string, uid, gid int, log *slog.Logger) (*agentProxy, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// One stable path per session. Remove any stale socket from a prior process
	// (a crash can leave the file; bind would otherwise fail with EADDRINUSE).
	path := filepath.Join(dir, "agent-"+sessionID+".sock")
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if uid >= 0 && gid >= 0 {
		// Chown the dir (so the shell can traverse into it) and the socket (so the
		// shell can connect — a unix-socket connect needs write, which a root-owned
		// 0755 socket denies a non-root user). A bind/listen failure here would
		// leak the listener, so tear it down on error.
		if cerr := os.Lchown(dir, uid, gid); cerr != nil {
			_ = ln.Close()
			_ = os.Remove(path)
			return nil, cerr
		}
		if cerr := os.Lchown(path, uid, gid); cerr != nil {
			_ = ln.Close()
			_ = os.Remove(path)
			return nil, cerr
		}
	}
	p := &agentProxy{path: path, ln: ln, log: log}
	go p.acceptLoop()
	return p, nil
}

// SockPath is the stable SSH_AUTH_SOCK value for the shell. Empty for a nil
// proxy (so the env var is simply not set).
func (p *agentProxy) SockPath() string {
	if p == nil {
		return ""
	}
	return p.path
}

// setSource points the proxy at attach's gliderlabs agent-listener socket
// (sourceSock). Called on a forwarding-capable attach.
func (p *agentProxy) setSource(sourceSock string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.source = sourceSock
	p.mu.Unlock()
}

// clearSource drops the forwarding source IFF it still points at sourceSock —
// so a detach can't clobber a newer attach that already re-pointed it (a
// reconnect's setSource may land before the old attach's deferred clearSource).
func (p *agentProxy) clearSource(sourceSock string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.source == sourceSock {
		p.source = ""
	}
	p.mu.Unlock()
}

func (p *agentProxy) currentSource() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.source
}

// acceptLoop serves the stable socket: each accepted connection (the shell's
// agent client) is proxied to the current forwarding source, or closed when
// there is none.
func (p *agentProxy) acceptLoop() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return // listener closed (Close) — stop.
		}
		go p.proxyConn(conn)
	}
}

// proxyConn bridges one shell-agent connection to the current source agent
// socket. With no source it closes the connection (clean failure). The source
// is read at accept time; a swap mid-connection doesn't redirect an already-open
// pipe (matches per-connection agent semantics).
func (p *agentProxy) proxyConn(local net.Conn) {
	defer local.Close()
	source := p.currentSource()
	if source == "" {
		return // no forwarding client attached → fail closed
	}
	upstream, err := net.Dial("unix", source)
	if err != nil {
		return // the attach's listener went away between swap and dial → fail closed
	}
	defer upstream.Close()
	// When EITHER direction finishes (EOF or error), close BOTH ends so the
	// surviving io.Copy unblocks promptly and neither goroutine lingers. Without
	// this, a half-close (one side closed, the other never EOFs) would wedge the
	// surviving copy and stall the deferred Close()s. closeOnce keeps it safe
	// under -race: both halves may race to close, but each Conn closes once.
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = local.Close()
			_ = upstream.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer closeBoth(); _, _ = io.Copy(upstream, local) }()
	go func() { defer wg.Done(); defer closeBoth(); _, _ = io.Copy(local, upstream) }()
	wg.Wait()
}

// Close stops the accept loop and removes the stable socket. Idempotent.
func (p *agentProxy) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	_ = p.ln.Close()
	_ = os.Remove(p.path)
}
