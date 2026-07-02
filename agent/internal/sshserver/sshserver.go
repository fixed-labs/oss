// Package sshserver is the agent-embedded, WireGuard-identity SSH server
// (modeled on Tailscale SSH). Both interactive shells and real
// `ssh` ride this one server, bound to wg0's address:
//
//   - Authentication IS the connection's source wg-ip: the server runs with
//     no client auth and maps source-ip → {developer-id, login-user} from the
//     pulled agent config (the same peer set wgnet applies — one config, two
//     uses). Sound because the box's only WireGuard peers are authorized
//     laptops: a packet on wg0 with a given source provably came from that
//     laptop (cryptokey routing). A source absent from the table is refused
//     before any channel opens.
//   - Single login user ⇒ no PAM, no /etc/passwd traversal: sessions exec as
//     the peer's login user (setuid only when the agent runs as root and the
//     target differs — on a devbox that's environment selection, not a
//     security boundary; the box is single-tenant).
//   - Feature surface (settled): PTY shell, exec, env / window-change /
//     signals / exit-status, the sftp subsystem (via a CREDENTIALED re-exec
//     of the agent binary speaking sftp on stdio — no extra image deps),
//     direct-tcpip (-L/-W/-D), tcpip-forward (-R), and agent forwarding.
//   - Host key: the persisted first-boot ed25519 (identity package). No
//     TOFU, no CA — the CLI pins the pubkey from the attach bundle.
package sshserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/fixed-labs/oss/agent/internal/sessions"
	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// ServeSFTP serves the sftp protocol over rwc until EOF — the body of the
// `devboxes-agent sftp-subsystem` child invocation (stdio), kept here so the
// protocol path is testable in-process over a pipe.
func ServeSFTP(rwc io.ReadWriteCloser) error {
	srv, err := sftp.NewServer(rwc)
	if err != nil {
		return err
	}
	if err := srv.Serve(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// Peer is the identity a source wg-ip maps to (a row of the pulled config).
type Peer struct {
	DeveloperID string
	LoginUser   string
}

// Table is the live source-wg-ip → identity map, swapped wholesale on every
// config reconcile (idempotent full replacement, like wg0's peer set).
type Table struct {
	mu sync.RWMutex
	m  map[string]Peer // key: source IP string (no port, canonical form)
}

func NewTable() *Table { return &Table{m: map[string]Peer{}} }

// Replace swaps the whole table (the reconcile path). Keys are NORMALIZED to
// the canonical net.IP string so they match Lookup, which canonicalizes the
// connection's source. This matters because the upstream key is the
// control-plane's wg-ipv6 encoding, which formats every hextet
// as %02x%02x — so a host hextet < 0x1000 carries a leading zero ("…:0af8:…")
// that net.IP.String() strips ("…:af8:…"). Without normalizing here the raw
// key never matches the canonicalized Lookup and the connection is refused
// ("unauthorized source refused") for ~1/3 of laptop keys — the wg tunnel is
// fine (wg/ip canonicalize allowed-ips themselves), only this Go-map lookup
// mismatches.
func (t *Table) Replace(m map[string]Peer) {
	canon := make(map[string]Peer, len(m))
	for k, v := range m {
		if ip := net.ParseIP(k); ip != nil {
			canon[ip.String()] = v
		} else {
			canon[k] = v // not an IP (shouldn't happen) — keep as-is
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m = canon
}

// Lookup resolves a remote address ("[fd..]:port" / "ip:port") to a Peer.
func (t *Table) Lookup(remoteAddr net.Addr) (Peer, bool) {
	host, _, err := net.SplitHostPort(remoteAddr.String())
	if err != nil {
		host = remoteAddr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return Peer{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	p, ok := t.m[ip.String()]
	return p, ok
}

type Server struct {
	// Addr is the listen address — wg0's address:22 in production (overlay-
	// only by construction), loopback in tests.
	Addr string
	// HostKeyPEM is the persisted PKCS8 ed25519 host key (identity package).
	HostKeyPEM []byte
	// Table authorizes connections by source IP.
	Table *Table
	// SFTPExec re-execs this binary in sftp-subsystem mode; empty disables
	// the subsystem (tests that don't exercise it).
	SFTPExec string
	// Sessions is the box-side persistent-session Manager. When non-nil the
	// interactive-shell path (bare pty-req+shell, or an empty-id attach) routes
	// to a default-session attach (create-or-attach the single default session),
	// and the `devbox-session` subsystem is served,
	// so shells outlive the connection. When nil the server falls back to the
	// legacy per-connection pty.Start shell (tests that don't exercise sessions).
	Sessions *sessions.Manager
	Log      *slog.Logger

	// activeConns gauges authorized SSH connections currently open — the box's
	// interactive-liveness signal in the heartbeat. Interactive liveness is now
	// open SSH connections plus the laptop's cluster-side Presence ping (held for
	// the life of a `connect`, which keeps a detached-but-live session warm). See
	// supervisor.go.
	activeConns atomic.Int64

	srv *ssh.Server
}

// ActiveSessions reports how many authorized SSH connections are open right
// now — interactive shells, exec, sftp, and port-forward-only clients alike;
// any of them is a developer at the keyboard for idle-suspend purposes.
func (s *Server) ActiveSessions() int {
	return int(s.activeConns.Load())
}

// countedConn decrements the gauge exactly once when the connection closes
// (the ssh server closes the wrapped conn on teardown of every path).
type countedConn struct {
	net.Conn
	once  sync.Once
	gauge *atomic.Int64
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.gauge.Add(-1) })
	return c.Conn.Close()
}

// ResolveCredential returns the SysProcAttr credential for `login` — nil when
// no setuid is needed (already that user, or not root so we couldn't anyway).
// Exported so main.go can build the session Manager's shell credential from the
// same logic the per-connection shell uses.
func ResolveCredential(login string) (*syscall.Credential, error) {
	return resolveCredential(login)
}

func resolveCredential(login string) (*syscall.Credential, error) {
	u, err := user.Lookup(login)
	if err != nil {
		return nil, fmt.Errorf("login user %q: %w", login, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if os.Getuid() == uid {
		return nil, nil
	}
	if os.Getuid() != 0 {
		return nil, fmt.Errorf("agent uid %d cannot become %q (uid %d)", os.Getuid(), login, uid)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

// LoginShellAndHome is the exported form of loginShellAndHome, so main.go can
// build the session Manager's shell/home from the same logic.
func LoginShellAndHome(login string) (shell, home string) {
	return loginShellAndHome(login)
}

func loginShellAndHome(login string) (shell, home string) {
	shell, home = "/bin/sh", "/"
	if u, err := user.Lookup(login); err == nil && u.HomeDir != "" {
		home = u.HomeDir
	}
	// os/user doesn't expose the shell; $SHELL of the agent is wrong here.
	// devboxes-base sets a real login shell via RIFT_LOGIN_SHELL; default sh.
	if s := os.Getenv("RIFT_LOGIN_SHELL"); s != "" {
		shell = s
	}
	return shell, home
}

// Start builds and starts the server (non-blocking). Close with Close().
func (s *Server) Start() error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	signer, err := gossh.ParsePrivateKey(s.HostKeyPEM)
	if err != nil {
		// identity persists PKCS8 PEM; x/crypto parses OpenSSH + PKCS8 alike.
		return fmt.Errorf("parse host key: %w", err)
	}

	forwardHandler := &ssh.ForwardedTCPHandler{}
	authorized := func(ctx ssh.Context) bool {
		_, ok := s.Table.Lookup(ctx.RemoteAddr())
		return ok
	}

	srv := &ssh.Server{
		Addr:        s.Addr,
		HostSigners: []ssh.Signer{signer},
		Handler:     s.handleSession,
		// direct-tcpip (-L/-W/-D): allowed for authorized sources only.
		LocalPortForwardingCallback: func(ctx ssh.Context, host string, port uint32) bool {
			return authorized(ctx)
		},
		// tcpip-forward (-R)
		ReversePortForwardingCallback: func(ctx ssh.Context, host string, port uint32) bool {
			return authorized(ctx)
		},
		RequestHandlers: map[string]ssh.RequestHandler{
			"tcpip-forward":        forwardHandler.HandleSSHRequest,
			"cancel-tcpip-forward": forwardHandler.HandleSSHRequest,
		},
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      ssh.DefaultSessionHandler,
			"direct-tcpip": ssh.DirectTCPIPHandler,
		},
		// The WG-identity gate: refuse the CONNECTION (before any channel)
		// when the source wg-ip isn't an authorized peer. With cryptokey
		// routing upstream this should never fire for wg0 traffic — it's the
		// defense-in-depth for a misbound listener.
		ConnCallback: func(ctx ssh.Context, conn net.Conn) net.Conn {
			if _, ok := s.Table.Lookup(conn.RemoteAddr()); !ok {
				s.Log.Warn("ssh: unauthorized source refused", "remote", conn.RemoteAddr())
				_ = conn.Close()
				return nil
			}
			s.activeConns.Add(1)
			return &countedConn{Conn: conn, gauge: &s.activeConns}
		},
	}
	srv.SubsystemHandlers = map[string]ssh.SubsystemHandler{}
	if s.SFTPExec != "" {
		srv.SubsystemHandlers["sftp"] = s.handleSFTP
	}
	if s.Sessions != nil {
		// The OUT-OF-BAND session control channel rides the already-authenticated
		// SSH connection as a subsystem, beside sftp.
		srv.SubsystemHandlers["devbox-session"] = s.handleSessionSubsystem
	}
	s.srv = srv

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.Addr, err)
	}
	go func() {
		// The SSH accept loop is one of the goroutines the agent spawns directly,
		// so for crash containment, wrap it in recover(): an unrelated panic here
		// must not exit the whole process (and end every persistent session).
		defer func() {
			if r := recover(); r != nil {
				s.Log.Error("ssh accept loop panic recovered", "panic", r)
			}
		}()
		if serr := srv.Serve(ln); serr != nil && serr != ssh.ErrServerClosed {
			s.Log.Error("ssh server exited", "err", serr)
		}
	}()
	s.Log.Info("ssh: WG-identity server listening", "addr", ln.Addr().String())
	return nil
}

// ListenAddr returns the bound address (useful when Addr had port 0 in tests).
func (s *Server) ListenAddr() string {
	if s.srv == nil {
		return ""
	}
	return s.Addr
}

func (s *Server) Close() error {
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

// handleSession implements shell/exec with PTY, env, window-change, signals,
// agent forwarding, and exit-status propagation.
func (s *Server) handleSession(sess ssh.Session) {
	peer, ok := s.Table.Lookup(sess.RemoteAddr())
	if !ok {
		// ConnCallback already gates; belt-and-suspenders.
		_ = sess.Exit(255)
		return
	}
	login := peer.LoginUser
	cred, err := resolveCredential(login)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "rift: %v\n", err)
		_ = sess.Exit(255)
		return
	}
	shell, home := loginShellAndHome(login)

	var cmd *exec.Cmd
	if raw := sess.Command(); len(raw) > 0 {
		cmd = exec.Command(shell, "-c", sess.RawCommand())
	} else {
		cmd = exec.Command(shell, "-l")
	}
	cmd.Dir = home
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"USER="+login,
		"LOGNAME="+login,
		"DEVBOX_DEVELOPER_ID="+peer.DeveloperID,
	)
	cmd.Env = append(cmd.Env, sess.Environ()...)
	if cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}

	// Agent forwarding: vend a unix socket bridging to the client's agent.
	if ssh.AgentRequested(sess) {
		if l, lerr := ssh.NewAgentListener(); lerr == nil {
			sock := l.Addr().String()
			dir := filepath.Dir(sock)
			// NewAgentListener (running as the agent, root on a devbox) creates
			// `dir` at 0700 plus listener.sock, both root-owned. The shell is
			// exec'd as the login user via SysProcAttr.Credential, so without
			// handing the dir+socket to that uid/gid the dropped-privilege shell
			// can't even traverse the 0700 root dir — every agent client gets
			// EACCES ("Error connecting to agent: Permission denied"). gliderlabs
			// never removes `dir`, so reap it ourselves to stop a /tmp leak across
			// reconnects. Defers run LIFO: l.Close (stop listener, unlink socket)
			// runs before os.RemoveAll (drop the now-empty dir).
			defer os.RemoveAll(dir)
			defer l.Close()

			ok := true
			if cred != nil {
				// `dir` is freshly mkdir-temp'd (CSPRNG name) and 0700-root until
				// this chown, so no attacker can pre-plant either path — there's no
				// symlink/TOCTOU window; Lchown is belt-and-suspenders. The dir
				// keeps 0700, so access narrows to exactly the login user. This is
				// safe specifically because the box is single-tenant: the only
				// other unprivileged uid IS this login user. Re-review if a second
				// uid is ever added to a devbox.
				uid, gid := int(cred.Uid), int(cred.Gid)
				if cerr := os.Lchown(dir, uid, gid); cerr != nil {
					s.Log.Warn("ssh: chown agent socket dir", "err", cerr)
					ok = false
				} else if cerr := os.Lchown(sock, uid, gid); cerr != nil {
					s.Log.Warn("ssh: chown agent socket", "err", cerr)
					ok = false
				}
			}
			// Fail closed: a misowned socket is merely unreachable (EACCES), so
			// don't export a broken SSH_AUTH_SOCK or forward connections to it.
			if ok {
				go ssh.ForwardAgentConnections(l, sess)
				cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+sock)
			}
		}
	}

	ptyReq, winCh, isPty := sess.Pty()
	// Bare interactive shell (plain pty-req + shell, no command) routes to a
	// default-session attach (create-or-attach the single default session) so a
	// human or older client still gets a shell that
	// outlives the connection (regression-tested). A pty'd EXEC
	// (`ssh box -t cmd`) keeps the legacy one-shot path below.
	if isPty && len(sess.Command()) == 0 && s.Sessions != nil {
		s.attachDefault(sess, ptyReq, winCh)
		return
	}
	if isPty {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
		f, perr := pty.Start(cmd)
		if perr != nil {
			fmt.Fprintf(sess.Stderr(), "rift: pty: %v\n", perr)
			_ = sess.Exit(255)
			return
		}
		_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(ptyReq.Window.Width), Rows: uint16(ptyReq.Window.Height)})
		// The resize goroutine owns Setsize on f for the session's life; f is
		// closed only AFTER it drains (winCh closes on session teardown,
		// triggered by sess.Exit below) — closing earlier races an in-flight
		// Setsize on f's fd (caught by -race).
		resizeDone := make(chan struct{})
		go func() {
			defer close(resizeDone)
			for win := range winCh {
				_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(win.Width), Rows: uint16(win.Height)})
			}
		}()
		go forwardSignals(sess, cmd)
		go func() { _, _ = io.Copy(f, sess) }()
		_, _ = io.Copy(sess, f)
		_ = sess.Exit(waitExitCode(cmd))
		select {
		case <-resizeDone:
		case <-time.After(2 * time.Second): // bound teardown if winCh never closes
		}
		_ = f.Close()
		return
	}

	cmd.Stdout = sess
	cmd.Stderr = sess.Stderr()
	stdin, serr := cmd.StdinPipe()
	if serr == nil {
		go func() {
			defer stdin.Close()
			_, _ = io.Copy(stdin, sess)
		}()
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(sess.Stderr(), "rift: %v\n", err)
		_ = sess.Exit(255)
		return
	}
	go forwardSignals(sess, cmd)
	_ = sess.Exit(waitExitCode(cmd))
}

func forwardSignals(sess ssh.Session, cmd *exec.Cmd) {
	sigs := make(chan ssh.Signal, 1)
	sess.Signals(sigs)
	for sig := range sigs {
		if cmd.Process == nil {
			continue
		}
		if s, ok := sigMap[sig]; ok {
			_ = cmd.Process.Signal(s)
		}
	}
}

var sigMap = map[ssh.Signal]os.Signal{
	ssh.SIGINT:  syscall.SIGINT,
	ssh.SIGTERM: syscall.SIGTERM,
	ssh.SIGHUP:  syscall.SIGHUP,
	ssh.SIGKILL: syscall.SIGKILL,
	ssh.SIGQUIT: syscall.SIGQUIT,
	ssh.SIGUSR1: syscall.SIGUSR1,
	ssh.SIGUSR2: syscall.SIGUSR2,
}

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

// ---- Persistent-session control protocol ------------------------------------

// ctlFrame is the client's single newline-terminated JSON control frame.
type ctlFrame struct {
	Op   string `json:"op"`   // "list" | "attach" | "new"
	ID   string `json:"id"`   // attach
	Name string `json:"name"` // new
}

// listResp / attachResp are the server's single newline-terminated JSON
// response frames.
type listResp struct {
	GenEpoch int64                `json:"gen_epoch"`
	Sessions []sessions.ListEntry `json:"sessions"`
}

type attachResp struct {
	OK    bool   `json:"ok"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Error string `json:"error,omitempty"`
}

// handleSessionSubsystem implements the `devbox-session` subsystem: read one
// control frame, then either answer `list` (and close) or wire the PTY channel
// into a session's fan-out (attach/new). Authorization already happened at the
// connection gate (WG identity) — the subsystem rides that authenticated
// connection (authentication is unchanged).
func (s *Server) handleSessionSubsystem(sess ssh.Session) {
	if _, ok := s.Table.Lookup(sess.RemoteAddr()); !ok {
		_ = sess.Exit(255)
		return
	}
	br := bufio.NewReader(sess)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		_ = sess.Exit(255)
		return
	}
	var frame ctlFrame
	if jerr := json.Unmarshal(trimLine(line), &frame); jerr != nil {
		_ = writeJSONLine(sess, attachResp{OK: false, Error: "bad control frame: " + jerr.Error()})
		_ = sess.Exit(255)
		return
	}

	switch frame.Op {
	case "list":
		// `list` carries no PTY and never resizes anyone.
		ge, entries := s.Sessions.List()
		_ = writeJSONLine(sess, listResp{GenEpoch: ge, Sessions: entries})
		_ = sess.Exit(0)
		return

	case "attach", "new", "":
		ptyReq, winCh, isPty := sess.Pty()
		w, h := 80, 24
		if isPty {
			w, h = ptyReq.Window.Width, ptyReq.Window.Height
		}
		client := sessions.NewClient(w, h)
		var (
			target *sessions.Session
			aerr   error
		)
		switch {
		case frame.Op == "new":
			target, aerr = s.Sessions.New(frame.Name, client)
		case frame.Op == "attach" && frame.ID != "":
			target, aerr = s.Sessions.Attach(frame.ID, client)
		default: // empty id / empty op → default-session (create-or-attach the single default session)
			target, aerr = s.Sessions.CreateOrAttachDefault(client)
		}
		if aerr != nil {
			_ = writeJSONLine(sess, attachResp{OK: false, Error: aerr.Error()})
			_ = sess.Exit(1)
			return
		}
		_ = writeJSONLine(sess, attachResp{OK: true, ID: target.ID(), Name: target.Name()})
		// br may hold buffered keystroke bytes after the control line — drain
		// them into the session first, then stream the rest of the channel.
		s.streamAttached(sess, target, client, winCh, br)
		return

	default:
		_ = writeJSONLine(sess, attachResp{OK: false, Error: "unknown op: " + frame.Op})
		_ = sess.Exit(1)
		return
	}
}

// attachDefault is the bare-shell path (no subsystem): default-session attach
// (create-or-attach the single default session) to
// `main`. The channel becomes the raw PTY byte stream — no control frame.
func (s *Server) attachDefault(sess ssh.Session, ptyReq ssh.Pty, winCh <-chan ssh.Window) {
	w, h := ptyReq.Window.Width, ptyReq.Window.Height
	if w <= 0 || h <= 0 {
		w, h = 80, 24
	}
	client := sessions.NewClient(w, h)
	target, err := s.Sessions.CreateOrAttachDefault(client)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "rift: session: %v\n", err)
		_ = sess.Exit(255)
		return
	}
	s.streamAttached(sess, target, client, winCh, sess)
}

// streamAttached wires an attached client to its SSH channel: server→client is
// the session fan-out (scrollback replay then live output); client→server is
// keystrokes from `in` (the channel, possibly behind a bufio.Reader holding
// buffered bytes after the control line). Detach = the client closes the
// channel (we Detach, session preserved). Session end = the shell exited (we
// announce + exit with the code). The per-client input goroutine is interrupted
// by channel close, so detach is clean.
func (s *Server) streamAttached(sess ssh.Session, target *sessions.Session, client *sessions.Client, winCh <-chan ssh.Window, in io.Reader) {
	// Agent forwarding (`std:ssh` real-forwarding): if this attach
	// requested it, stand up a per-attach gliderlabs agent listener bridging to
	// the laptop's agent over THIS connection, and point the session's stable
	// SSH_AUTH_SOCK proxy at it for the attach's duration. Cleared on detach so a
	// fully-detached session has no live agent (parity with `ssh -A`); re-pointed
	// when a later attach forwards again.
	if cleanup := s.setupAgentForward(sess, target); cleanup != nil {
		defer cleanup()
	}

	// Resize stream: list/control frames never reach here; this is a real PTY
	// channel. Window-change requests recompute the smallest-attached size.
	go func() {
		for win := range winCh {
			target.Resize(client, win.Width, win.Height)
		}
	}()

	// Output writer: drain the client's bounded queue onto the channel until it
	// closes (detach / overflow drop / session end).
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		sessions.PipeToClient(sess, client)
	}()

	// Input reader: client keystrokes → PTY master. Returns on channel close
	// (EOF) — that's the detach signal.
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		_, _ = io.Copy(writerToSession{target}, in)
	}()

	// Wait for either the session to end (shell exit) or the client to detach.
	select {
	case <-target.Done():
		// Session END: announce the real exit code and exit the channel with it.
		code := target.ExitCode()
		fmt.Fprintf(sess, "\r\n[rift] session terminated, exit code %d\r\n", code)
		_ = sess.Exit(code)
	case <-inputDone:
		// Client detached (channel closed). Session is PRESERVED.
		target.Detach(client)
		_ = sess.Exit(0)
	}
	// Belt-and-suspenders: ensure the client is detached and the writer winds
	// down (Detach closes the queue, ending PipeToClient).
	target.Detach(client)
	<-writerDone
}

// setupAgentForward wires this attach's SSH agent into the persistent session's
// stable SSH_AUTH_SOCK proxy. Returns nil (no cleanup) when the
// client didn't request forwarding, the listener can't be created, or the
// dropped-privilege shell couldn't reach the socket. Otherwise it returns a
// cleanup that clears the session's forwarding source and reaps the per-attach
// listener + its temp dir.
//
// gliderlabs' NewAgentListener creates a 0700 root-owned dir + socket; the
// session shell runs as the (possibly dropped-privilege) login user, so the
// socket must be chowned to that user or the proxy's Dial of it fails. The
// session proxy connects to this socket as the AGENT (root), so only the proxy
// dials it — but we still chown so a direct dial from the login user works too,
// and to keep the same fail-closed posture as the legacy per-connection path.
func (s *Server) setupAgentForward(sess ssh.Session, target *sessions.Session) func() {
	if !ssh.AgentRequested(sess) {
		return nil
	}
	l, err := ssh.NewAgentListener()
	if err != nil {
		s.Log.Warn("ssh: agent listener for session forward", "err", err)
		return nil
	}
	sock := l.Addr().String()
	dir := filepath.Dir(sock)
	// The session's agent proxy (running as the agent, root) dials this socket,
	// so root ownership already works. Chown to the login user too (belt-and-
	// suspenders + parity with the legacy path) when a setuid credential applies.
	if peer, ok := s.Table.Lookup(sess.RemoteAddr()); ok {
		if cred, cerr := resolveCredential(peer.LoginUser); cerr == nil && cred != nil {
			uid, gid := int(cred.Uid), int(cred.Gid)
			_ = os.Lchown(dir, uid, gid)
			_ = os.Lchown(sock, uid, gid)
		}
	}
	go ssh.ForwardAgentConnections(l, sess)
	target.SetAgentSource(sock)
	return func() {
		target.ClearAgentSource(sock)
		_ = l.Close()         // stop ForwardAgentConnections, unlink the socket
		_ = os.RemoveAll(dir) // gliderlabs never reaps the temp dir
	}
}

// writerToSession adapts a *sessions.Session into an io.Writer for io.Copy of
// client keystrokes into the PTY master.
type writerToSession struct{ s *sessions.Session }

func (w writerToSession) Write(p []byte) (int, error) { return w.s.Write(p) }

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func trimLine(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// handleSFTP serves the sftp subsystem by re-exec'ing THIS binary in
// sftp-subsystem mode (see cmd main) with the login user's credential —
// pkg/sftp then runs with the right uid/gid and file ownership comes out
// correct, with no extra binaries baked into the image.
func (s *Server) handleSFTP(sess ssh.Session) {
	peer, ok := s.Table.Lookup(sess.RemoteAddr())
	if !ok {
		_ = sess.Exit(255)
		return
	}
	cred, err := resolveCredential(peer.LoginUser)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "rift: %v\n", err)
		_ = sess.Exit(255)
		return
	}
	_, home := loginShellAndHome(peer.LoginUser)
	cmd := exec.Command(s.SFTPExec, "sftp-subsystem")
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home, "USER="+peer.LoginUser)
	if cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = sess.Exit(255)
		return
	}
	cmd.Stdout = sess
	cmd.Stderr = sess.Stderr()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(sess.Stderr(), "rift: sftp: %v\n", err)
		_ = sess.Exit(255)
		return
	}
	go func() {
		defer stdin.Close()
		_, _ = io.Copy(stdin, sess)
	}()
	_ = sess.Exit(waitExitCode(cmd))
}
