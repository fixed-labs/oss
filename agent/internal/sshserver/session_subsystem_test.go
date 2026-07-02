package sshserver

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
	"github.com/fixed-labs/oss/agent/internal/sessions"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// nullAPI is a no-op SessionAPI for server tests (POSTs are exercised in the
// sessions package).
type nullAPI struct{ mu sync.Mutex }

func (n *nullAPI) CreateSession(context.Context, string, string, int64) error { return nil }
func (n *nullAPI) EndSession(context.Context, string, string) error           { return nil }
func (n *nullAPI) SyncSessions(context.Context, int64, map[string]api.SessionMeta) error {
	return nil
}
func (n *nullAPI) TombstoneStaleSessions(context.Context, int64) error { return nil }

// startServerWithSessions starts a server with a Sessions Manager wired in on a
// fresh ephemeral loopback port; returns the addr and the Manager.
func startServerWithSessions(t *testing.T, table *Table) (addr string, mgr *sessions.Manager) {
	return startServerWithSessionsDir(t, table, "")
}

// startServerWithSessionsDir is startServerWithSessions with an explicit
// AgentSockDir (non-empty enables the per-session SSH_AUTH_SOCK agent-forward
// proxy).
func startServerWithSessionsDir(t *testing.T, table *Table, agentSockDir string) (addr string, mgr *sessions.Manager) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = l.Addr().String()
	_ = l.Close()

	mgr = sessions.NewManager(sessions.Config{
		Shell:        "/bin/bash",
		Home:         t.TempDir(),
		API:          &nullAPI{},
		GenEpoch:     9,
		AgentSockDir: agentSockDir,
	})
	s := &Server{
		Addr:       addr,
		HostKeyPEM: testHostKeyPEM(t),
		Table:      table,
		Sessions:   mgr,
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start with sessions: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return addr, mgr
}

// openSubsystem opens a `devbox-session` subsystem channel and returns the
// session + a reader over it. It requests a PTY when wantPTY.
func openSubsystem(t *testing.T, client *gossh.Client, wantPTY bool) (*gossh.Session, *bufio.Reader) {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if wantPTY {
		if err := sess.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err != nil {
			t.Fatalf("RequestPty: %v", err)
		}
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// gliderlabs dispatches by the subsystem name in the request.
	if err := requestSubsystem(sess, "devbox-session"); err != nil {
		t.Fatalf("subsystem: %v", err)
	}
	return sess, bufio.NewReader(stdout)
}

// requestSubsystem sends an ssh "subsystem" request (x/crypto has no public
// helper; we send the raw request like its sftp client does).
func requestSubsystem(sess *gossh.Session, name string) error {
	ok, err := sess.SendRequest("subsystem", true, gossh.Marshal(&struct{ Name string }{name}))
	if err != nil {
		return err
	}
	if !ok {
		return errSubsystemRefused
	}
	return nil
}

var errSubsystemRefused = &subsystemErr{}

type subsystemErr struct{}

func (*subsystemErr) Error() string { return "subsystem request refused" }

func TestSessionSubsystemList(t *testing.T) {
	srv, mgr := startServerWithSessions(t, authorizedTable(t))
	// Seed a session so list has a row.
	c := sessions.NewClient(80, 24)
	s, err := mgr.CreateOrAttachDefault(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Detach(c)

	client := dial(t, srv)
	sess, r := openSubsystem(t, client, false)
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte(`{"op":"list"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read list resp: %v", err)
	}
	var resp struct {
		GenEpoch int64 `json:"gen_epoch"`
		Sessions []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			AttachedCount int    `json:"attached_count"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode list resp %q: %v", line, err)
	}
	if resp.GenEpoch != 9 {
		t.Fatalf("gen_epoch = %d, want 9", resp.GenEpoch)
	}
	if len(resp.Sessions) != 1 || resp.Sessions[0].Name != "main" {
		t.Fatalf("list sessions = %+v, want one named main", resp.Sessions)
	}
}

// TestSessionSubsystemAttach: attach via the subsystem, type a command, see the
// echoed output on the raw byte stream after the ok frame.
func TestSessionSubsystemAttach(t *testing.T) {
	srv, _ := startServerWithSessions(t, authorizedTable(t))
	client := dial(t, srv)
	sess, r := openSubsystem(t, client, true)
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	// new (empty name) → creates main and attaches.
	if _, err := stdin.Write([]byte(`{"op":"new","name":""}` + "\n")); err != nil {
		t.Fatal(err)
	}
	// First line is the ok frame.
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read ok frame: %v", err)
	}
	var ok struct {
		OK   bool   `json:"ok"`
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(line, &ok); err != nil || !ok.OK {
		t.Fatalf("attach not ok: %q (%v)", line, err)
	}
	if ok.Name != "main" {
		t.Fatalf("attached name = %q, want main", ok.Name)
	}
	// Now the channel is the raw PTY stream. Type a marker.
	if _, err := stdin.Write([]byte("echo SUBSYS_MARKER_42\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var seen strings.Builder
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, rerr := r.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
			if strings.Contains(seen.String(), "SUBSYS_MARKER_42") {
				return // success
			}
		}
		if rerr != nil {
			break
		}
	}
	t.Fatalf("never saw marker in subsystem stream:\n%q", seen.String())
}

// TestBareShellRoutesToMain: a plain pty-req+shell (no subsystem, no command)
// routes to default-session selection (create-or-attach the single default
// session) → `main`. Regression guard for the bare-ssh path.
func TestBareShellRoutesToMain(t *testing.T) {
	srv, mgr := startServerWithSessions(t, authorizedTable(t))
	client := dial(t, srv)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Bare interactive shell: no command, just Shell().
	if err := sess.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if _, err := stdin.Write([]byte("echo BARE_MARKER_99\n")); err != nil {
		t.Fatal(err)
	}
	// The session must exist in the Manager as `main`.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.HeldLivePTYs() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mgr.HeldLivePTYs() < 1 {
		t.Fatal("bare shell did not create a Manager session")
	}
	_, entries := mgr.List()
	found := false
	for _, e := range entries {
		if e.Name == "main" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bare shell did not create `main`: %+v", entries)
	}
	// And output streams.
	r := bufio.NewReader(stdout)
	var seen strings.Builder
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, rerr := r.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
			if strings.Contains(seen.String(), "BARE_MARKER_99") {
				return
			}
		}
		if rerr != nil {
			break
		}
	}
	t.Fatalf("bare shell output missing marker:\n%q", seen.String())
}

// --- agent forwarding end-to-end ------------------------------

// marshalString / marshalPtyReq / marshalSubsystem mirror the CLI client's
// RFC 4254 channel-request payloads (kept local to this test).
func tMarshalString(s string) []byte {
	b := make([]byte, 4+len(s))
	b[0], b[1], b[2], b[3] = byte(len(s)>>24), byte(len(s)>>16), byte(len(s)>>8), byte(len(s))
	copy(b[4:], s)
	return b
}

func tMarshalPtyReq(term string, cols, rows int) []byte {
	put := func(b []byte, v uint32) []byte {
		return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	var b []byte
	b = append(b, tMarshalString(term)...)
	b = put(b, uint32(cols))
	b = put(b, uint32(rows))
	b = put(b, uint32(cols*8))
	b = put(b, uint32(rows*8))
	b = append(b, tMarshalString("\x00")...)
	return b
}

// TestSessionSubsystemAgentForward proves agent forwarding end-to-end: a
// client that registers agent forwarding (agent.ForwardToAgent) and sends
// auth-agent-req on the attach channel gets a working SSH_AUTH_SOCK in the
// session SHELL — `ssh-add -l` over that socket reaches the LAPTOP keyring and
// lists the forwarded key. This exercises the full chain: client ForwardToAgent
// + auth-agent-req → server AgentRequested → setupAgentForward + NewAgentListener
// → the session's stable agentProxy → shell's SSH_AUTH_SOCK.
func TestSessionSubsystemAgentForward(t *testing.T) {
	if _, err := exec.LookPath("ssh-add"); err != nil {
		t.Skip("ssh-add not available; skipping agent-forward e2e")
	}

	// Laptop-side keyring with one ed25519 key (the "agent").
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	const keyComment = "devbox-forward-test-key"
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv, Comment: keyComment}); err != nil {
		t.Fatal(err)
	}

	srv, _ := startServerWithSessionsDir(t, authorizedTable(t), t.TempDir())
	client := dial(t, srv)

	// Register the connection-level forwarding handler (what the CLI does in Dial).
	if err := agent.ForwardToAgent(client, keyring); err != nil {
		t.Fatalf("ForwardToAgent: %v", err)
	}

	// Open the session channel directly so we control request ORDER: pty-req,
	// then auth-agent-req (BEFORE subsystem, so the server's AgentRequested is set
	// before the subsystem handler launches), then subsystem.
	ch, reqs, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("OpenChannel: %v", err)
	}
	defer ch.Close()
	go gossh.DiscardRequests(reqs)

	if ok, err := ch.SendRequest("pty-req", true, tMarshalPtyReq("xterm", 80, 24)); err != nil || !ok {
		t.Fatalf("pty-req: %v (ok=%v)", err, ok)
	}
	if ok, err := ch.SendRequest("auth-agent-req@openssh.com", true, nil); err != nil || !ok {
		t.Fatalf("auth-agent-req: %v (ok=%v)", err, ok)
	}
	if ok, err := ch.SendRequest("subsystem", true, tMarshalString("devbox-session")); err != nil || !ok {
		t.Fatalf("subsystem: %v (ok=%v)", err, ok)
	}
	if _, err := ch.Write([]byte(`{"op":"new","name":""}` + "\n")); err != nil {
		t.Fatal(err)
	}

	r := bufio.NewReader(ch)
	ackLine, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack struct {
		OK bool `json:"ok"`
	}
	if jerr := json.Unmarshal(ackLine, &ack); jerr != nil || !ack.OK {
		t.Fatalf("attach not ok: %q (%v)", ackLine, jerr)
	}

	// In the shell, list the forwarded agent's keys. If SSH_AUTH_SOCK reaches the
	// laptop keyring, the output carries our key comment. The PTY read is moved to
	// a goroutine so the test bounds it (gossh.Channel has no read deadline).
	if _, err := ch.Write([]byte("ssh-add -l\n")); err != nil {
		t.Fatal(err)
	}
	seenCh := make(chan string, 1)
	go func() {
		var seen strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				seen.Write(buf[:n])
				if strings.Contains(seen.String(), keyComment) || strings.Contains(seen.String(), "no identities") {
					seenCh <- seen.String()
					return
				}
			}
			if rerr != nil {
				seenCh <- seen.String()
				return
			}
		}
	}()
	select {
	case got := <-seenCh:
		if strings.Contains(got, keyComment) {
			return // success: the forwarded key is visible in the session shell
		}
		if strings.Contains(got, "no identities") {
			t.Fatalf("agent reachable but empty — forwarding bridged to the wrong/empty source:\n%q", got)
		}
		t.Fatalf("forwarded key never visible via SSH_AUTH_SOCK in the shell:\n%q", got)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ssh-add -l output (forwarding likely not reaching the shell)")
	}
}
