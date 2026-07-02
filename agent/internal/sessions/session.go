package sessions

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// Session is a long-lived PTY + the process group under it (a login shell and
// its children). It ends ONLY when its root shell exits (waitLoop / cmd.Wait),
// never on detach.
type Session struct {
	id        string
	name      string
	createdAt int64 // epoch-ms
	genEpoch  int64

	master *os.File  // PTY master
	cmd    *exec.Cmd // the root login shell
	pgid   int       // process-group id for kill(-pgid)

	mgr *Manager
	log *slog.Logger

	ring *ring // scrollback

	// agentProxy is the session's stable SSH_AUTH_SOCK proxy for agent
	// forwarding; nil when forwarding is disabled. Its source is
	// (re)pointed on a forwarding-capable attach and cleared on detach; it is
	// closed on session destroy.
	agentProxy *agentProxy

	mu       sync.Mutex
	clients  map[*Client]struct{}
	winW     int // current PTY window width (smallest of attached clients)
	winH     int
	doneOnce sync.Once
	done     chan struct{} // closed once on destroy
	exitCode int           // valid after done is closed
}

// Client is one attached connection's view of a session: a buffered output
// channel drained by the connection's own writer goroutine, so the fan-out
// never blocks on a slow client. Width/Height are this client's last-known
// window size, folded into the session's smallest-attached policy.
type Client struct {
	out    chan []byte
	width  int
	height int

	// detached guards the close of out so the fan-out's drop path and a clean
	// detach can't double-close.
	detachOnce sync.Once
}

// NewClient builds a client with the initial window size from its pty-req.
func NewClient(width, height int) *Client {
	return &Client{
		out:    make(chan []byte, clientQueue),
		width:  width,
		height: height,
	}
}

// Output is the channel a connection's writer goroutine drains to ship bytes to
// its client. It is closed when the client is detached (clean detach or
// overflow drop) OR when the session ends — the writer goroutine should range
// over it and exit on close.
func (c *Client) Output() <-chan []byte { return c.out }

func (s *Session) attachedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// Done returns the channel closed once when the session is destroyed; a
// connection selects on it to learn the shell exited.
func (s *Session) Done() <-chan struct{} { return s.done }

// ExitCode is valid after Done() is closed: the root shell's exit code.
func (s *Session) ExitCode() int {
	<-s.done
	return s.exitCode
}

// ID/Name/CreatedAt/GenEpoch expose session metadata for the control protocol.
func (s *Session) ID() string       { return s.id }
func (s *Session) Name() string     { return s.name }
func (s *Session) CreatedAt() int64 { return s.createdAt }
func (s *Session) GenEpoch() int64  { return s.genEpoch }

// Write sends client keystrokes to the PTY master. The kernel serializes writes
// to the master, so mirrored clients can all type safely.
func (s *Session) Write(p []byte) (int, error) {
	return s.master.Write(p)
}

// SetAgentSource points this session's stable SSH_AUTH_SOCK proxy at sourceSock
// — the socket of a forwarding-capable attach's gliderlabs agent listener.
// A no-op when the session has no proxy (forwarding disabled).
func (s *Session) SetAgentSource(sourceSock string) {
	s.agentProxy.setSource(sourceSock)
}

// ClearAgentSource drops the forwarding source IFF it still points at
// sourceSock (so a detach can't clobber a newer reconnect that already
// re-pointed the proxy). Called on detach/teardown of a forwarding attach.
func (s *Session) ClearAgentSource(sourceSock string) {
	s.agentProxy.clearSource(sourceSock)
}

// attach registers a client, recomputes the window size to the smallest of
// attached clients, replays scrollback into the client's queue, and signals a
// repaint. Detach is the channel close; it never ends the session.
//
// CRITICAL ordering (panic: send on closed channel): the scrollback replay is
// buffered into c.out with NON-BLOCKING sends while STILL holding s.mu, and the
// client is added to s.clients only AFTER. No other goroutine can see (and
// therefore close) c.out until it's registered, and once registered every send
// runs through the drop-guarded, non-blocking fanout. There is thus NO window in
// which fanout/waitLoop/Detach can close c.out concurrently with a send here.
// The replay is ≤2 items (clear + replay) into the empty clientQueue-cap
// channel, so the non-blocking sends never drop in practice.
func (s *Session) attach(c *Client) {
	replay := s.ring.replay()

	s.mu.Lock()
	// Buffer the replay BEFORE the client is visible to any closer. Non-blocking
	// sends into the empty queue: a leading clear+home for a clean slate, then the
	// replay, which RECONSTRUCTS the visible screen (it runs from the most-recent
	// clear marker, or the whole ring for a line-based shell with no marker).
	//
	// Do NOT clear AFTER the replay: the replay is the live screen, not just
	// off-screen scrollback. A trailing clear+home wipes it, and a line-based app
	// (a shell showing `ls` output) never repaints scrolled content on SIGWINCH —
	// so the user reconnects to a blank screen. The leading clear is enough for a
	// clean slate; the SIGWINCH below nudges full-screen apps (vim/tmux) to repaint
	// over the reconstructed frame.
	select {
	case c.out <- []byte("\x1b[2J\x1b[H"):
	default:
	}
	if len(replay) > 0 {
		select {
		case c.out <- replay:
		default:
		}
	}
	// Now register — from here fanout/Detach/waitLoop may target (and close)
	// c.out, but only through the non-blocking, drop-guarded paths.
	s.clients[c] = struct{}{}
	s.recomputeWinLocked()
	w, h := s.winW, s.winH
	s.mu.Unlock()

	// Set the (recomputed) window size on the master so the child redraws at the
	// right geometry.
	if w > 0 && h > 0 {
		_ = pty.Setsize(s.master, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
	}
	// Force the foreground process group to repaint. pty.Setsize only delivers
	// SIGWINCH when the geometry CHANGES; on reattach the new client almost always
	// has the same size as the held one, so Setsize is a silent no-op and an idle
	// shell (or a full-screen app) never redraws onto the freshly-cleared screen —
	// leaving it blank until the user manually resizes. An explicit SIGWINCH makes
	// the foreground app (bash readline, vim, tmux, …) redraw now, so reconnect
	// shows the live screen without a manual nudge.
	if s.pgid > 0 {
		_ = syscall.Kill(-s.pgid, syscall.SIGWINCH)
	}
}

// Detach removes a client, closes its output channel, recomputes the window
// size (held on last detach), and records the keep-warm clock. NEVER ends the
// session.
func (s *Session) Detach(c *Client) {
	s.mu.Lock()
	_, present := s.clients[c]
	if present {
		delete(s.clients, c)
	}
	last := len(s.clients) == 0
	if !last {
		s.recomputeWinLocked()
	}
	w, h := s.winW, s.winH
	s.mu.Unlock()
	if !present {
		return
	}
	c.detachOnce.Do(func() { close(c.out) })
	// On a non-last detach, apply the new (possibly larger) smallest size. On
	// last detach we HOLD the size (no resize).
	if !last && w > 0 && h > 0 {
		_ = pty.Setsize(s.master, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
	}
	s.mgr.touchDetach()
}

// Resize updates a client's reported size and recomputes the smallest-attached
// PTY size. list/new control frames carry no PTY and must never call this.
func (s *Session) Resize(c *Client, width, height int) {
	s.mu.Lock()
	if _, ok := s.clients[c]; !ok {
		s.mu.Unlock()
		return
	}
	c.width, c.height = width, height
	s.recomputeWinLocked()
	w, h := s.winW, s.winH
	s.mu.Unlock()
	if w > 0 && h > 0 {
		_ = pty.Setsize(s.master, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
	}
}

// recomputeWinLocked sets winW/winH to the SMALLEST of attached clients (so no
// client ever sees a line wider/taller than its own terminal). Caller holds
// s.mu. With no attached clients the size is held (unchanged).
func (s *Session) recomputeWinLocked() {
	w, h := 0, 0
	for c := range s.clients {
		if c.width <= 0 || c.height <= 0 {
			continue
		}
		if w == 0 || c.width < w {
			w = c.width
		}
		if h == 0 || c.height < h {
			h = c.height
		}
	}
	if w > 0 && h > 0 {
		s.winW, s.winH = w, h
	}
}

// readLoop is the single per-session reader: it blocks on the PTY master (NO
// deadline — cancelling the master read is not reliably interruptible),
// appends to the ring, and fans out to attached clients with a NON-BLOCKING
// enqueue, detaching any client whose queue overflows. The read buffer is
// reused, so bytes are COPIED before enqueue.
func (s *Session) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			// Copy out of the reused buffer before the ring/fan-out retain it.
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.ring.append(chunk)
			s.fanout(chunk)
		}
		if err != nil {
			// PTY EOF / closed master. Session END is driven by waitLoop
			// (cmd.Wait, accurate exit code), not here — just stop reading.
			return
		}
	}
}

// fanout enqueues chunk to every attached client without blocking; a client
// whose bounded queue is full is DETACHED (slow viewer dropped, shell never
// stalls).
func (s *Session) fanout(chunk []byte) {
	s.mu.Lock()
	drop := make([]*Client, 0)
	for c := range s.clients {
		select {
		case c.out <- chunk:
		default:
			drop = append(drop, c)
		}
	}
	for _, c := range drop {
		delete(s.clients, c)
		c.detachOnce.Do(func() { close(c.out) })
	}
	last := len(s.clients) == 0
	s.mu.Unlock()
	if len(drop) > 0 {
		s.log.Warn("sessions: dropped slow client(s)", "id", s.id, "count", len(drop))
		if last {
			// Overflow that emptied the attach set is still a detach for the
			// keep-warm clock.
			s.mgr.touchDetach()
		}
	}
}

// waitLoop blocks on the root shell's cmd.Wait for an accurate exit code, then
// destroys the session: kill(-pgid) to reap a setsid/nohup holder of the slave,
// close the master, remove from the registry, close done (unblocking attached
// connections), and POST an `end` event. This is the ONLY path that ends a
// session.
func (s *Session) waitLoop() {
	code := waitExitCode(s.cmd)
	// Reap the whole process group: a `setsid`/`nohup` child can hold the slave
	// open after the shell exits.
	if s.pgid > 0 {
		_ = syscall.Kill(-s.pgid, syscall.SIGHUP)
		_ = syscall.Kill(-s.pgid, syscall.SIGKILL)
	}
	_ = s.master.Close()

	// Tear down the agent-forward proxy (stop the accept loop, unlink the stable
	// socket) so it doesn't leak across the box's session lifetimes.
	s.agentProxy.Close()

	s.mgr.remove(s)

	// Close every attached client's output channel and the done signal exactly
	// once.
	s.mu.Lock()
	for c := range s.clients {
		c.detachOnce.Do(func() { close(c.out) })
	}
	s.clients = map[*Client]struct{}{}
	s.mu.Unlock()

	s.exitCode = code
	s.doneOnce.Do(func() { close(s.done) })

	s.reportEnd(code)
}

func (s *Session) reportEnd(code int) {
	if s.mgr.api == nil {
		return
	}
	ctx, cancel := contextWithTimeout(15)
	defer cancel()
	reason := fmt.Sprintf("shell exited code %d", code)
	if err := s.mgr.api.EndSession(ctx, s.id, reason); err != nil {
		s.log.Warn("sessions: EndSession POST failed", "id", s.id, "err", err)
	}
}

// foreground reads this session's foreground process cmd/cwd (best-effort) for
// the picker snapshot.
func (s *Session) foreground() (cmd, cwd string) {
	return foregroundProc(int(s.master.Fd()))
}

// waitExitCode mirrors sshserver.waitExitCode (kept local to avoid a dep edge).
func waitExitCode(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 255
}

// PipeToClient runs the per-client output writer: it drains the client's queue
// onto dst (the SSH channel) until the queue closes (detach/drop/session end).
// Returns when the queue is closed or dst errors.
func PipeToClient(dst io.Writer, c *Client) {
	for chunk := range c.out {
		if _, err := dst.Write(chunk); err != nil {
			return
		}
	}
}
