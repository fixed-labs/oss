package session

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeAgent is an in-process devbox-session SSH server that speaks the session
// subsystem's control-frame protocol, used to prove the client's framing and
// stream handover end-to-end without a
// real box. Behavior is driven by the handler the test installs.
type fakeAgent struct {
	hostSigner ssh.Signer
	hostPub    string // authorized_keys line for the client to pin
	// handle is called per session channel after the control frame is read. It
	// gets the parsed frame and the channel (already past pty/subsystem reqs).
	handle func(frame map[string]any, ch ssh.Channel)
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pub := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	return &fakeAgent{hostSigner: signer, hostPub: pub}
}

// serve runs the server side of an SSH conn on netConn (one connection).
func (a *fakeAgent) serve(t *testing.T, netConn net.Conn) {
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(a.hostSigner)
	sconn, chans, reqs, err := ssh.NewServerConn(netConn, cfg)
	if err != nil {
		return // client closed; fine for the list-then-close path
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go a.handleChannel(t, ch, creqs)
	}
}

func (a *fakeAgent) handleChannel(t *testing.T, ch ssh.Channel, reqs <-chan *ssh.Request) {
	// Answer pty-req / subsystem (reply true), ignore window-change. Once the
	// subsystem request lands, read the control frame and dispatch.
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			_ = req.Reply(true, nil)
		case "window-change":
			// no reply expected
		case "subsystem":
			_ = req.Reply(true, nil)
			a.dispatch(t, ch)
			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func (a *fakeAgent) dispatch(t *testing.T, ch ssh.Channel) {
	br := bufio.NewReader(ch)
	line, err := br.ReadBytes('\n')
	if err != nil {
		_ = ch.Close()
		return
	}
	var frame map[string]any
	if err := json.Unmarshal(line[:len(line)-1], &frame); err != nil {
		_ = ch.Close()
		return
	}
	if a.handle != nil {
		a.handle(frame, ch)
	}
}

// pipePair returns a connected client/server net.Conn pair over a loopback TCP
// listener. (A bare net.Pipe deadlocks the SSH version-banner exchange because
// it has no buffering — both sides block writing their banner; TCP's kernel
// buffer breaks the cycle.)
func pipePair(t *testing.T) (client net.Conn, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	cConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return cConn, r.c
}

// dialClient wires a client Client to a fresh in-process server conn.
func (a *fakeAgent) dialClient(t *testing.T) *Client {
	t.Helper()
	cConn, sConn := pipePair(t)
	go a.serve(t, sConn)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return cConn, nil
	}
	cl, err := Dial(context.Background(), dial, "10.0.0.1", 22, "dev", a.hostPub, false)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return cl
}

func TestListFraming(t *testing.T) {
	a := newFakeAgent(t)
	a.handle = func(frame map[string]any, ch ssh.Channel) {
		if frame["op"] != "list" {
			t.Errorf("op = %v, want list", frame["op"])
		}
		resp := `{"gen_epoch":42,"sessions":[{"id":"s1","name":"main","created_at":1000,"attached_count":2}]}` + "\n"
		_, _ = io.WriteString(ch, resp)
		_ = ch.Close()
	}
	cl := a.dialClient(t)
	defer cl.Close()

	res, err := cl.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.GenEpoch != 42 {
		t.Fatalf("gen_epoch = %d, want 42", res.GenEpoch)
	}
	if len(res.Sessions) != 1 || res.Sessions[0].ID != "s1" || res.Sessions[0].Name != "main" || res.Sessions[0].AttachedCount != 2 {
		t.Fatalf("sessions = %+v", res.Sessions)
	}
}

func TestAttachOKThenRawStream(t *testing.T) {
	a := newFakeAgent(t)
	a.handle = func(frame map[string]any, ch ssh.Channel) {
		if frame["op"] != "attach" || frame["id"] != "s1" {
			t.Errorf("frame = %+v, want attach s1", frame)
		}
		// ack ok, then raw scrollback + a marker, immediately after the newline so
		// the test proves the client's bufio reader doesn't drop buffered bytes.
		// (The attach ack carries no gen_epoch — that's a LIST-response field.)
		_, _ = io.WriteString(ch, `{"ok":true,"id":"s1","name":"main"}`+"\n"+"REPLAYED-SCROLLBACK")
		// echo one keystroke round-trip
		buf := make([]byte, 64)
		n, _ := ch.Read(buf)
		_, _ = ch.Write(append([]byte("ECHO:"), buf[:n]...))
		_ = ch.Close()
	}
	cl := a.dialClient(t)
	defer cl.Close()

	att, err := cl.Attach(context.Background(), "s1", 80, 23)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if att.ID != "s1" || att.Name != "main" {
		t.Fatalf("ack = %+v", att)
	}
	// The raw stream must carry the bytes written right after the ack newline.
	got := readSome(t, att, "REPLAYED-SCROLLBACK")
	if got != "REPLAYED-SCROLLBACK" {
		t.Fatalf("scrollback after ack lost: got %q", got)
	}
	// Keystroke round-trip over the raw channel.
	if _, err := att.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if echo := readSome(t, att, "ECHO:hi"); echo != "ECHO:hi" {
		t.Fatalf("echo = %q", echo)
	}
}

func TestAttachRefused(t *testing.T) {
	a := newFakeAgent(t)
	a.handle = func(frame map[string]any, ch ssh.Channel) {
		_, _ = io.WriteString(ch, `{"ok":false,"error":"no such session"}`+"\n")
		_ = ch.Close()
	}
	cl := a.dialClient(t)
	defer cl.Close()
	_, err := cl.Attach(context.Background(), "ghost", 80, 23)
	if err == nil {
		t.Fatal("want error on ok:false")
	}
}

func TestNewSession(t *testing.T) {
	a := newFakeAgent(t)
	a.handle = func(frame map[string]any, ch ssh.Channel) {
		if frame["op"] != "new" {
			t.Errorf("op = %v, want new", frame["op"])
		}
		_, _ = io.WriteString(ch, `{"ok":true,"id":"mint-1","name":"main"}`+"\n")
		_ = ch.Close()
	}
	cl := a.dialClient(t)
	defer cl.Close()
	att, err := cl.New(context.Background(), "", 80, 23)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if att.ID != "mint-1" {
		t.Fatalf("id = %q, want mint-1", att.ID)
	}
}

// TestSessionEndExitCode proves the child-exit path: the agent writes the
// "session terminated, exit code N" banner, sends an SSH exit-status request,
// then closes — and the client's Wait() surfaces N.
func TestSessionEndExitCode(t *testing.T) {
	a := newFakeAgent(t)
	a.handle = func(frame map[string]any, ch ssh.Channel) {
		_, _ = io.WriteString(ch, `{"ok":true,"id":"s1","name":"main"}`+"\n")
		_, _ = io.WriteString(ch, "\r\n[rift] session terminated, exit code 3\r\n")
		// SSH exit-status payload is a single big-endian uint32.
		_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 3})
		_ = ch.Close()
	}
	cl := a.dialClient(t)
	defer cl.Close()
	att, err := cl.Attach(context.Background(), "s1", 80, 23)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// Drain the stream so the channel reaches EOF (as the compositor would).
	go func() { _, _ = io.Copy(io.Discard, att) }()
	code, ok := att.Wait()
	if !ok {
		t.Fatal("Wait: want a known exit status")
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}

// TestSessionDropNoExitStatus proves the transport-drop path: the channel closes
// WITHOUT an exit-status (a dropped transport, not a real session end), so Wait()
// reports ok=false and the reconnect loop knows to re-dial.
func TestSessionDropNoExitStatus(t *testing.T) {
	a := newFakeAgent(t)
	a.handle = func(frame map[string]any, ch ssh.Channel) {
		_, _ = io.WriteString(ch, `{"ok":true,"id":"s1","name":"main"}`+"\n")
		_ = ch.Close() // no exit-status — simulates a drop
	}
	cl := a.dialClient(t)
	defer cl.Close()
	att, err := cl.Attach(context.Background(), "s1", 80, 23)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, att) }()
	if _, ok := att.Wait(); ok {
		t.Fatal("Wait: a drop without exit-status must report ok=false")
	}
}

func TestWrongHostKeyRejected(t *testing.T) {
	a := newFakeAgent(t)
	// Pin a DIFFERENT host key than the server presents.
	other := newFakeAgent(t)
	cConn, sConn := pipePair(t)
	go a.serve(t, sConn)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) { return cConn, nil }
	if _, err := Dial(context.Background(), dial, "10.0.0.1", 22, "dev", other.hostPub, false); err == nil {
		t.Fatal("dial must reject a mismatched host key")
	}
}

// TestKeepaliveClosesAfterConsecutiveMisses proves the dead-peer detection: a
// client whose keepalive probe keeps failing is Close()d after exactly
// keepaliveMaxFailures consecutive misses (so a blackholed transport unblocks the
// channel read and the reconnect loop re-dials). It drives runKeepalive directly
// with a manual tick channel and a stub probe so it costs no wall-clock time.
func TestKeepaliveClosesAfterConsecutiveMisses(t *testing.T) {
	closed := make(chan struct{})
	c := &Client{keepaliveDone: make(chan struct{})}
	// Detect the Close() by watching keepaliveDone (Close closes it via closeOnce).
	go func() {
		<-c.keepaliveDone
		close(closed)
	}()

	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		c.runKeepalive(tick, func() bool { return false }) // always miss
		close(done)
	}()

	// One short of the threshold must NOT close.
	for i := 0; i < keepaliveMaxFailures-1; i++ {
		tick <- time.Time{}
	}
	select {
	case <-closed:
		t.Fatalf("client closed after only %d misses (threshold %d)", keepaliveMaxFailures-1, keepaliveMaxFailures)
	case <-time.After(50 * time.Millisecond):
	}

	// The threshold-th consecutive miss closes the client and returns the loop.
	tick <- time.Time{}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("client not closed after threshold consecutive misses")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runKeepalive did not return after closing")
	}
}

// TestKeepaliveSuccessResetsMisses proves a successful probe clears the miss
// counter, so isolated drops don't accumulate toward a spurious Close.
func TestKeepaliveSuccessResetsMisses(t *testing.T) {
	c := &Client{keepaliveDone: make(chan struct{})}
	closed := make(chan struct{})
	go func() { <-c.keepaliveDone; close(closed) }()

	tick := make(chan time.Time)
	ok := make(chan bool)
	done := make(chan struct{})
	go func() {
		c.runKeepalive(tick, func() bool { return <-ok })
		close(done)
	}()

	// Miss threshold-1 times, then succeed — must reset, not close.
	for i := 0; i < keepaliveMaxFailures-1; i++ {
		tick <- time.Time{}
		ok <- false
	}
	tick <- time.Time{}
	ok <- true
	select {
	case <-closed:
		t.Fatal("client closed despite a success resetting the miss counter")
	case <-time.After(50 * time.Millisecond):
	}

	// Now a fresh run of threshold misses closes it.
	for i := 0; i < keepaliveMaxFailures; i++ {
		tick <- time.Time{}
		// The final miss closes inside runKeepalive before reading ok again, so a
		// non-blocking send avoids deadlocking the test on the last iteration.
		select {
		case ok <- false:
		case <-closed:
		}
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("client not closed after a fresh run of threshold misses")
	}
	<-done
}

// readSome reads from att until it has at least len(want) bytes or times out.
func readSome(t *testing.T, att *Attached, want string) string {
	t.Helper()
	var acc []byte
	deadline := time.Now().Add(3 * time.Second)
	buf := make([]byte, 256)
	for len(acc) < len(want) && time.Now().Before(deadline) {
		n, err := att.Read(buf)
		if n > 0 {
			acc = append(acc, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	if len(acc) > len(want) {
		acc = acc[:len(want)]
	}
	return string(acc)
}
