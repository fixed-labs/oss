package sessions

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stubAgent is a tiny unix-socket server that, on each connection, writes marker
// then echoes whatever it reads — a stand-in for a per-attach gliderlabs agent
// listener so the proxy's bridging is testable without a real SSH stack.
func stubAgent(t *testing.T, marker string) (sockPath string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stub-agent.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.WriteString(c, marker)
				_, _ = io.Copy(c, c) // echo
			}(c)
		}
	}()
	return path
}

// dialProxyRead dials the proxy socket and returns up to n bytes (or "" on a
// clean immediate close). Bounded so a hang surfaces as a test failure.
func dialProxyRead(t *testing.T, sockPath string, n int) string {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, n)
	got, _ := io.ReadFull(conn, buf)
	return string(buf[:got])
}

// TestAgentProxySwapsSourceAndFailsClosed proves the box half of agent forwarding:
//   - no source attached → accepted connections close immediately (fail closed,
//     so git/ssh fail cleanly rather than hang).
//   - setSource → connections bridge to that source's agent.
//   - a reconnect re-points the source (setSource to a new agent) → connections
//     now bridge to the NEW source (forwarding survives reconnect).
//   - clearSource(old) after a re-point is a no-op (the detach of the old attach
//     can't clobber the newer reconnect's source).
//   - clearSource(current) → fail closed again (fully detached: no live agent).
func TestAgentProxySwapsSourceAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	p, err := newAgentProxy(dir, "sess-1", -1, -1, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// No source yet → fail closed (empty read).
	if got := dialProxyRead(t, p.path, 8); got != "" {
		t.Fatalf("no-source connect should close empty, got %q", got)
	}

	// Attach A.
	srcA := stubAgent(t, "AGENT-A")
	p.setSource(srcA)
	if got := dialProxyRead(t, p.path, len("AGENT-A")); got != "AGENT-A" {
		t.Fatalf("after setSource(A): got %q, want AGENT-A", got)
	}

	// Reconnect: a new connection's listener becomes the source (B). Forwarding
	// must follow the reconnect.
	srcB := stubAgent(t, "AGENT-B")
	p.setSource(srcB)
	if got := dialProxyRead(t, p.path, len("AGENT-B")); got != "AGENT-B" {
		t.Fatalf("after reconnect setSource(B): got %q, want AGENT-B", got)
	}

	// The OLD attach's deferred clearSource(A) must NOT clobber B (guarded swap).
	p.clearSource(srcA)
	if got := dialProxyRead(t, p.path, len("AGENT-B")); got != "AGENT-B" {
		t.Fatalf("clearSource(stale A) must not drop B: got %q, want AGENT-B", got)
	}

	// Detaching the current source → fully detached → fail closed.
	p.clearSource(srcB)
	if got := dialProxyRead(t, p.path, 8); got != "" {
		t.Fatalf("after clearSource(B): want fail-closed empty, got %q", got)
	}
}

// silentAgent accepts connections and then NEVER writes, reads, or closes them —
// a stand-in for an upstream agent that doesn't EOF its end. It returns the
// listener so the test can shut it down on cleanup.
func silentAgent(t *testing.T) (sockPath string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "silent-agent.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	var mu sync.Mutex
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // hold the conn open, do nothing with it
			mu.Unlock()
		}
	}()
	return path
}

// TestAgentProxyHalfCloseUnblocks proves the half-close robustness: when one
// io.Copy half of proxyConn finishes (here the shell-agent client half-closes by
// closing its end), proxyConn must close BOTH ends so the OTHER half — copying
// from an upstream that never EOFs — unblocks promptly rather than lingering. We
// assert by observing the proxy-side connection becomes readable-at-EOF shortly
// after the client closes its write side; without the both-ends close, the read
// would block on the silent upstream past the deadline.
func TestAgentProxyHalfCloseUnblocks(t *testing.T) {
	dir := t.TempDir()
	p, err := newAgentProxy(dir, "sess-hc", -1, -1, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Upstream source that holds the connection open and never EOFs.
	p.setSource(silentAgent(t))

	conn, err := net.Dial("unix", p.path)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	// Half-close: close only the WRITE side, so the proxy's local→upstream copy
	// sees EOF and returns. The fix must then close the upstream side too, which
	// closes the whole proxyConn — surfacing here as EOF on our read.
	if uc, ok := conn.(*net.UnixConn); ok {
		if err := uc.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	} else {
		t.Fatal("expected *net.UnixConn")
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	_, rerr := conn.Read(buf)
	if rerr == nil {
		t.Fatal("expected the proxy to close our conn after the half-close, got data")
	}
	if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
		t.Fatal("proxyConn did not close both ends: read blocked past the deadline (half-close wedge)")
	}
	// Any non-timeout error (EOF / use-of-closed) means the proxy closed the conn
	// promptly — the fix works.
}

// TestAgentProxyDisabledWhenNoDir proves forwarding is OFF when no sock dir is
// configured (nil proxy, empty SockPath, all methods no-op).
func TestAgentProxyDisabledWhenNoDir(t *testing.T) {
	p, err := newAgentProxy("", "sess-x", -1, -1, slog.Default())
	if err != nil {
		t.Fatalf("nil-dir proxy: %v", err)
	}
	if p != nil {
		t.Fatal("empty dir should yield a nil proxy (forwarding disabled)")
	}
	// nil-receiver methods must be safe.
	if p.SockPath() != "" {
		t.Fatal("nil proxy SockPath must be empty")
	}
	p.setSource("x")
	p.clearSource("x")
	p.Close()
}

// TestAgentProxyChownsToSessionUser locks in the fix for the agent-forwarding
// permission bug: the proxy is created by the agent (root) but its socket is the
// shell's SSH_AUTH_SOCK, and the shell runs as the unprivileged login user. The
// dir (0700) and socket are root-owned by default, so without a chown the shell
// can't traverse the dir or connect to the socket (EACCES) and forwarding
// silently fails. newAgentProxy must chown BOTH the dir and the socket to the
// login uid/gid. We chown to the current uid/gid (a non-root-permitted chown) so
// the test runs anywhere, and assert ownership landed on both paths.
func TestAgentProxyChownsToSessionUser(t *testing.T) {
	dir := t.TempDir()
	uid, gid := os.Getuid(), os.Getgid()
	p, err := newAgentProxy(dir, "sess-chown", uid, gid, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for _, path := range []string{dir, p.path} {
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if int(st.Uid) != uid || int(st.Gid) != gid {
			t.Fatalf("%s owned by %d:%d, want %d:%d (chown not applied)", path, st.Uid, st.Gid, uid, gid)
		}
	}

	// The listener must still work after the chown (bridging intact).
	p.setSource(stubAgent(t, "OK"))
	if got := dialProxyRead(t, p.path, 2); got != "OK" {
		t.Fatalf("post-chown bridge: got %q, want %q", got, "OK")
	}
}
