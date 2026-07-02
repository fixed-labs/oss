// Package sessions is the box-side persistent-session module. It lives INSIDE
// the existing agent process — not a separate daemon — and owns the
// long-lived PTYs whose lifetime is decoupled from any one SSH connection:
//
//   - A Session is a login shell + the process group under it, fronted by a
//     PTY master. It ends ONLY when its root shell exits (cmd.Wait), never on
//     detach. Detach (clean disconnect, drop, sleep) leaves the session
//     running so reconnect re-attaches.
//   - Multiple clients can attach to one session (Mirrored attach); the
//     kernel serializes writes to the PTY master, so every mirrored client can
//     type and all see the same output.
//   - A fixed in-memory ring per session replays recent output on attach, so a
//     reconnect into a full-screen app repaints cleanly without the agent
//     modelling a terminal grid.
//
// The Manager is constructed once in main.go, handed to sshserver.Server, and
// its liveness accessors feed the supervisor's heartbeat. Terminal bytes never
// leave the box (no terminal bytes leave the box); only metadata is POSTed to the control
// plane via the api.Client.
package sessions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/fixed-labs/oss/agent/internal/api"
	"golang.org/x/sys/unix"
)

// ringSize is the fixed per-session scrollback buffer (raw output bytes). A
// reconnect replays from the most-recent full-screen clear within this window
// (or the whole ring if none), so a full-screen app repaints without the agent
// holding a terminal grid. Fixed + in-memory by design (a future scrollback
// upgrade wouldn't change the attach protocol).
const ringSize = 256 * 1024

// clientQueue bounds a single attached client's output backlog. The fan-out
// does a NON-BLOCKING enqueue onto this; on overflow the slow client is
// DETACHED (dropped) rather than allowed to stall the shared shell.
const clientQueue = 256

// SessionAPI is the slice of the agent's HTTP client the Manager drives —
// session metadata POSTs only (terminal bytes never cross). *api.Client
// satisfies it directly. Narrowed to an interface so the Manager is testable
// without a live control plane.
type SessionAPI interface {
	CreateSession(ctx context.Context, id, name string, genEpoch int64) error
	EndSession(ctx context.Context, id, reason string) error
	SyncSessions(ctx context.Context, genEpoch int64, snapshot map[string]api.SessionMeta) error
	TombstoneStaleSessions(ctx context.Context, genEpoch int64) error
}

// Manager is the per-box session registry. ONE mutex guards every map mutation
// — including name allocation + insert — so two concurrent first-connects can't
// both create `main`.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session // id → session
	byName   map[string]string   // name → id

	shell string              // login shell (e.g. /bin/bash -l)
	home  string              // login home dir
	login string              // login name for the shell's USER= env ("" → derive from home)
	cred  *syscall.Credential // setuid credential for the shell (nil = no setuid)

	api      SessionAPI
	genEpoch int64
	log      *slog.Logger

	// agentSockDir is where per-session SSH_AUTH_SOCK proxy sockets live (the
	// box half of agent forwarding). Empty disables forwarding
	// (no SSH_AUTH_SOCK exported) — e.g. tests that don't exercise it.
	agentSockDir string

	// lastDetachAt is the most-recent detach timestamp across ALL held PTYs —
	// the keep-warm clock the supervisor's interactiveLive computation reads.
	lastDetachAt time.Time
}

// Config carries the Manager's construction inputs from main.go.
type Config struct {
	Shell string
	Home  string
	// Login is the authoritative login name (main.go's resolved login user) used
	// for the shell's USER= env. Empty falls back to deriving the name from Home.
	Login    string
	Cred     *syscall.Credential
	API      SessionAPI
	GenEpoch int64
	Log      *slog.Logger
	// AgentSockDir is the directory for per-session SSH_AUTH_SOCK proxy sockets
	// (agent forwarding). Empty disables forwarding.
	AgentSockDir string
}

// NewManager builds a Manager. genEpoch is the post-bump epoch (see Reconcile);
// every session created stamps it.
func NewManager(c Config) *Manager {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		sessions:     map[string]*Session{},
		byName:       map[string]string{},
		shell:        c.Shell,
		home:         c.Home,
		login:        c.Login,
		cred:         c.Cred,
		api:          c.API,
		genEpoch:     c.GenEpoch,
		log:          log,
		agentSockDir: c.AgentSockDir,
	}
}

// GenEpoch returns the Manager's stamped process-generation epoch.
func (m *Manager) GenEpoch() int64 { return m.genEpoch }

// genSessionID mints a CSPRNG hex session id (minted server-side).
func genSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// nowMs is the wall clock, indirected for tests.
var nowMs = func() int64 { return time.Now().UnixMilli() }

// ErrNoSuchSession is returned by Attach for an unknown id.
var ErrNoSuchSession = errors.New("no such session")

// ListEntry is one row of a `list` response.
type ListEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	CreatedAt     int64  `json:"created_at"`
	AttachedCount int    `json:"attached_count"`
}

// List snapshots the registry for the `list` control frame.
func (m *Manager) List() (genEpoch int64, entries []ListEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries = make([]ListEntry, 0, len(m.sessions))
	for _, s := range m.sessions {
		entries = append(entries, ListEntry{
			ID:            s.id,
			Name:          s.name,
			CreatedAt:     s.createdAt,
			AttachedCount: s.attachedCount(),
		})
	}
	return m.genEpoch, entries
}

// startShell spawns a login shell on a fresh PTY. PTY mechanics by design:
// set ONLY SysProcAttr.Credential and let pty.Start own Setsid/Setctty; read
// the pgid AFTER start for kill(-pgid). authSock, when non-empty, is exported as
// the shell's stable SSH_AUTH_SOCK (the session's agent-forward proxy path).
func (m *Manager) startShell(authSock string) (master *os.File, cmd *exec.Cmd, pgid int, err error) {
	cmd = exec.Command(m.shell, "-l")
	cmd.Dir = m.home
	cmd.Env = append(os.Environ(),
		"HOME="+m.home,
		"USER="+m.userEnv(),
		"TERM=xterm-256color",
	)
	if authSock != "" {
		// A STABLE per-session socket the Manager owns; its backing agent is the
		// currently-attached forwarding client. Set even though no
		// client is forwarding yet — the path is fixed at spawn, the source is
		// swapped in/out as forwarding attaches come and go.
		cmd.Env = append(cmd.Env, "SSH_AUTH_SOCK="+authSock)
	}
	if m.cred != nil {
		// Only the Credential — pty.Start adds Setsid + Setctty itself; setting
		// them here would conflict.
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: m.cred}
	}
	master, err = pty.Start(cmd)
	if err != nil {
		return nil, nil, 0, err
	}
	// The session leader's pgid == its pid (pty.Start did Setsid). Read it after
	// start so kill(-pgid) reaps a setsid/nohup child holding the slave.
	pgid, perr := syscall.Getpgid(cmd.Process.Pid)
	if perr != nil {
		pgid = cmd.Process.Pid
	}
	return master, cmd, pgid, nil
}

// userEnv is the login name for the shell's USER env. main.go computes the
// authoritative login and passes it in (m.login); preferring it removes the risk
// that a name re-derived from the home dir disagrees with the user the box
// actually authorizes as. If no login was supplied (some other caller path), it
// falls back to the prior best-effort derivation from the home dir's last path
// segment so nothing breaks. The credential (uid/gid) remains authoritative for
// actual privilege; this is only the cosmetic env var.
func (m *Manager) userEnv() string {
	if m.login != "" {
		return m.login
	}
	home := m.home
	if home == "" || home == "/" {
		return ""
	}
	for i := len(home) - 1; i >= 0; i-- {
		if home[i] == '/' {
			return home[i+1:]
		}
	}
	return home
}

// credIDs returns the login user's uid/gid from the setuid credential, used to
// chown the agent-forward proxy socket so the unprivileged shell can reach it.
// Returns (-1, -1) when no credential applies (the shell runs as the agent/root,
// so no chown is needed) — newAgentProxy treats that as "skip the chown".
func (m *Manager) credIDs() (uid, gid int) {
	if m.cred == nil {
		return -1, -1
	}
	return int(m.cred.Uid), int(m.cred.Gid)
}

// CreateOrAttachDefault implements default-session selection (create-or-attach
// the single default session): with the registry empty,
// create+attach `main`; otherwise attach the single session; with more than one
// the caller (control protocol) renders a picker, so here we attach `main` if
// it exists else the first session. The bare-shell path and an empty-id attach
// both route here. The session is created under the lock so two concurrent
// first-connects share one `main`.
func (m *Manager) CreateOrAttachDefault(c *Client) (*Session, error) {
	m.mu.Lock()
	switch len(m.sessions) {
	case 0:
		s, err := m.newSessionLocked("main")
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		m.mu.Unlock()
		m.afterCreate(s)
		s.attach(c)
		m.touchAttach()
		return s, nil
	default:
		// Prefer `main`; else any one (deterministic enough for a single-session
		// box, which is the common case).
		var s *Session
		if id, ok := m.byName["main"]; ok {
			s = m.sessions[id]
		} else {
			for _, v := range m.sessions {
				s = v
				break
			}
		}
		m.mu.Unlock()
		s.attach(c)
		m.touchAttach()
		return s, nil
	}
}

// Attach wires a client to an existing session by id.
func (m *Manager) Attach(id string, c *Client) (*Session, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNoSuchSession
	}
	s.attach(c)
	m.touchAttach()
	return s, nil
}

// New creates a session (optionally named) and attaches the client. An empty
// name allocates `main` if free, else a generated name.
func (m *Manager) New(name string, c *Client) (*Session, error) {
	m.mu.Lock()
	if name == "" {
		if _, taken := m.byName["main"]; !taken {
			name = "main"
		} else {
			name = "session-" + time.Now().Format("150405")
		}
	}
	if _, taken := m.byName[name]; taken {
		m.mu.Unlock()
		return nil, fmt.Errorf("session name %q already exists", name)
	}
	s, err := m.newSessionLocked(name)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Unlock()
	m.afterCreate(s)
	s.attach(c)
	m.touchAttach()
	return s, nil
}

// newSessionLocked spawns the shell and inserts the session. The caller holds
// m.mu. It does NOT do the (slow / blocking) CreateSession POST — afterCreate
// does that without the lock.
func (m *Manager) newSessionLocked(name string) (*Session, error) {
	id, err := genSessionID()
	if err != nil {
		return nil, err
	}
	// A stable per-session SSH_AUTH_SOCK proxy for agent forwarding.
	// Best-effort: a proxy setup failure must not block the shell — the
	// session just lacks forwarding (warned, not fatal). nil when forwarding is
	// disabled (no sock dir configured) or on error.
	var proxy *agentProxy
	if m.agentSockDir != "" {
		uid, gid := m.credIDs()
		if p, perr := newAgentProxy(m.agentSockDir, id, uid, gid, m.log); perr != nil {
			m.log.Warn("sessions: agent-forward proxy setup failed; session has no SSH_AUTH_SOCK", "id", id, "err", perr)
		} else {
			proxy = p
		}
	}
	master, cmd, pgid, err := m.startShell(proxy.SockPath())
	if err != nil {
		proxy.Close()
		return nil, err
	}
	s := &Session{
		id:         id,
		name:       name,
		createdAt:  nowMs(),
		genEpoch:   m.genEpoch,
		master:     master,
		cmd:        cmd,
		pgid:       pgid,
		clients:    map[*Client]struct{}{},
		done:       make(chan struct{}),
		ring:       newRing(ringSize),
		mgr:        m,
		log:        m.log,
		agentProxy: proxy,
	}
	m.sessions[id] = s
	m.byName[name] = id
	go s.readLoop()
	go s.waitLoop()
	return s, nil
}

// afterCreate runs the read-after-write CreateSession POST (the barrier) and
// reports the new session, without the registry lock held.
func (m *Manager) afterCreate(s *Session) {
	if m.api == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.api.CreateSession(ctx, s.id, s.name, s.genEpoch); err != nil {
		m.log.Warn("sessions: CreateSession POST failed", "id", s.id, "err", err)
	}
}

// remove drops a session from the registry under the lock (called by the
// session's waitLoop on shell exit).
func (m *Manager) remove(s *Session) {
	m.mu.Lock()
	if cur, ok := m.sessions[s.id]; ok && cur == s {
		delete(m.sessions, s.id)
		if m.byName[s.name] == s.id {
			delete(m.byName, s.name)
		}
	}
	m.mu.Unlock()
}

// touchAttach fires an on-attach SyncSessions. It runs the POST in a goroutine:
// touchAttach is reached from the fan-out reader path (via a dropped last client)
// and from connection goroutines, and the reader must NEVER block (the whole
// point of the bounded-queue fan-out), so the HTTP POST can't be synchronous.
func (m *Manager) touchAttach() {
	go m.SyncNow()
}

// touchDetach records the most-recent-detach timestamp (the keep-warm clock the
// supervisor reads) and fires an on-detach SyncSessions. The POST runs in a
// goroutine for the same never-block-the-reader reason as touchAttach.
func (m *Manager) touchDetach() {
	m.mu.Lock()
	m.lastDetachAt = time.Now()
	m.mu.Unlock()
	go m.SyncNow()
}

// AttachedClients counts SESSIONS that have ≥1 attached client (a different
// axis from sshserver's connection gauge). Liveness accessor.
func (m *Manager) AttachedClients() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sessions {
		if s.attachedCount() > 0 {
			n++
		}
	}
	return n
}

// HeldLivePTYs counts SESSIONS with a live shell, attached or not. Liveness
// accessor — every session in the registry has a live shell (waitLoop removes
// it on exit), so this is the registry size.
func (m *Manager) HeldLivePTYs() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// LastDetachAt returns the most-recent detach timestamp across all held PTYs
// (zero if none yet). The supervisor measures "within detached-keepwarm-ms"
// from this.
func (m *Manager) LastDetachAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastDetachAt
}

// snapshot builds the SyncSessions payload — a snapshot of all live sessions
// keyed by id. Foreground cmd/cwd are read best-effort per session.
func (m *Manager) snapshot() map[string]api.SessionMeta {
	m.mu.Lock()
	live := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()
	out := make(map[string]api.SessionMeta, len(live))
	for _, s := range live {
		cmd, cwd := s.foreground()
		out[s.id] = api.SessionMeta{
			Name:          s.name,
			AttachedCount: int64(s.attachedCount()),
			ForegroundCmd: cmd,
			ForegroundCwd: cwd,
		}
	}
	return out
}

// SyncNow POSTs a snapshot of all live sessions. Fired on the heartbeat cadence
// (by the supervisor) AND on every attach/detach.
func (m *Manager) SyncNow() {
	if m.api == nil {
		return
	}
	snap := m.snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.api.SyncSessions(ctx, m.genEpoch, snap); err != nil {
		m.log.Warn("sessions: SyncSessions POST failed", "err", err)
	}
}

// foregroundProc reads the PTY foreground process group and resolves its comm +
// cwd from /proc (the agent runs as root, so it can read a login user's /proc).
// Best-effort: any error or a race with process exit yields "".
func foregroundProc(masterFd int) (cmd, cwd string) {
	pgrp, err := unix.IoctlGetInt(masterFd, unix.TIOCGPGRP)
	if err != nil || pgrp <= 0 {
		return "", ""
	}
	if b, rerr := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pgrp)); rerr == nil {
		cmd = trimNL(string(b))
	}
	if link, rerr := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pgrp)); rerr == nil {
		cwd = link
	}
	return cmd, cwd
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
