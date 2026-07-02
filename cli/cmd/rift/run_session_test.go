package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/compositor"
	"github.com/fixed-labs/oss/cli/internal/session"
	"github.com/fixed-labs/oss/cli/internal/tunnel"
	gossh "golang.org/x/crypto/ssh"
)

// stubLoopDeps swaps the three network/terminal seams runSessionLoop drives so
// the loop's control flow (notably the consecutive-failure-counter reset) is
// testable without a real tunnel, SSH dial, or PTY. It restores them on cleanup.
func stubLoopDeps(t *testing.T, attach func() (compositor.Outcome, bool, int, bool, error)) (redials *int) {
	t.Helper()
	pd, pa, pb, ps := dialSessionFn, attachOnceFn, backoffAndProbeFn, showReconnectingFn
	t.Cleanup(func() {
		dialSessionFn, attachOnceFn, backoffAndProbeFn, showReconnectingFn = pd, pa, pb, ps
	})

	n := 0
	dialSessionFn = func(_ context.Context, _ *tunnel.Tunnel, _ *client.AttachBundle, _ bool) (*session.Client, error) {
		n++
		return &session.Client{}, nil // non-nil sentinel; attachOnceFn never uses it
	}
	attachOnceFn = func(_ context.Context, _ *session.Client, _ string, _ *string, _ string, onAttached func()) (compositor.Outcome, bool, int, bool, error) {
		o, at, code, known, err := attach()
		// Mirror the real attachOnce: a live attach tears down the disconnect overlay
		// just before the compositor repaints.
		if at && onAttached != nil {
			onAttached()
		}
		return o, at, code, known, err
	}
	backoffAndProbeFn = func(_ context.Context, _ *tunnel.Tunnel, _ string, _ int, _, _ time.Duration) bool {
		return true
	}
	// No real terminal in tests: hand the loop an inert overlay by default. Tests
	// that assert overlay behavior install their own recording factory afterward.
	showReconnectingFn = func(_ string, _ func()) reconnectScreen { return &fakeRecon{} }
	return &n
}

// fakeRecon is an inert reconnectScreen the loop tests drive instead of a real
// terminal overlay; it records status updates and closes.
type fakeRecon struct {
	statuses []string
	closed   int
}

func (f *fakeRecon) SetStatus(s string) { f.statuses = append(f.statuses, s) }
func (f *fakeRecon) Close()             { f.closed++ }

// TestRunSessionLoopRaisesDisconnectOverlay asserts a transport drop raises the
// disconnect overlay (with a status) and the next successful attach tears it down
// — one overlay per drop, each closed before the live session repaints.
func TestRunSessionLoopRaisesDisconnectOverlay(t *testing.T) {
	bundle := &client.AttachBundle{WorkspaceWgIP: "10.0.0.1"}

	// Two successful attach→drop cycles, then the local terminal closes.
	calls := 0
	attach := func() (compositor.Outcome, bool, int, bool, error) {
		calls++
		if calls > 2 {
			return compositor.OutcomeClientGone, true, 0, false, nil
		}
		return compositor.OutcomeChildExit, true, 0, false, errors.New("transport dropped")
	}
	stubLoopDeps(t, attach)

	// Record every overlay the loop raises (overrides stubLoopDeps' inert default).
	var raised []*fakeRecon
	showReconnectingFn = func(_ string, _ func()) reconnectScreen {
		f := &fakeRecon{}
		raised = append(raised, f)
		return f
	}

	if err := runSessionLoop(context.Background(), nil, bundle, "ws", "s1", "", false); err != nil {
		t.Fatalf("loop returned err: %v", err)
	}
	if len(raised) != 2 {
		t.Fatalf("raised %d overlays, want 2 (one per drop)", len(raised))
	}
	for i, f := range raised {
		if len(f.statuses) == 0 {
			t.Fatalf("overlay %d got no status update", i)
		}
		if f.closed == 0 {
			t.Fatalf("overlay %d was never closed before the live session repainted", i)
		}
	}
}

// TestRunSessionLoopConsecutiveFailureReset asserts the
// maxConsecutiveFailures cap counts CONSECUTIVE transport drops, NOT the
// cumulative total over the connect lifetime. A box that reattaches cleanly
// many times must never be abandoned, because each SUCCESSFUL attach resets the
// counter. The mirror case: drops with NO intervening successful attach do stop
// at the cap.
func TestRunSessionLoopConsecutiveFailureReset(t *testing.T) {
	bundle := &client.AttachBundle{WorkspaceWgIP: "10.0.0.1"}

	t.Run("many successful reattach→drop cycles never give up", func(t *testing.T) {
		// Far more drops than maxConsecutiveFailures (6) — but EVERY cycle has a
		// successful attach (attached=true) before the drop, so the counter keeps
		// resetting and the loop keeps going. After the cycles, the local terminal
		// closes (OutcomeClientGone) → clean stop.
		const cycles = 20
		calls := 0
		attach := func() (compositor.Outcome, bool, int, bool, error) {
			calls++
			if calls > cycles {
				return compositor.OutcomeClientGone, true, 0, false, nil
			}
			// Successful attach (attached=true) then a mid-session transport drop
			// (no exit status) → classifyChildExit → childExitReconnect.
			return compositor.OutcomeChildExit, true, 0, false, errors.New("transport dropped")
		}
		stubLoopDeps(t, attach)

		err := runSessionLoop(context.Background(), nil, bundle, "ws", "s1", "", false)
		if err != nil {
			t.Fatalf("loop should survive %d successful reattach→drop cycles, got err: %v", cycles, err)
		}
		if calls <= cycles {
			t.Fatalf("loop exited early after %d attach calls (want > %d)", calls, cycles)
		}
	})

	t.Run("consecutive failures with no successful attach stop at the cap", func(t *testing.T) {
		// Every attach FAILS before reaching the compositor (attached=false) with a
		// transport error → childExitReconnect, no reset. The 6th increment trips
		// maxConsecutiveFailures and the loop returns "session lost".
		calls := 0
		attach := func() (compositor.Outcome, bool, int, bool, error) {
			calls++
			return compositor.OutcomeChildExit, false, 0, false, errors.New("attach: unreachable")
		}
		stubLoopDeps(t, attach)

		err := runSessionLoop(context.Background(), nil, bundle, "ws", "s1", "", false)
		if err == nil {
			t.Fatal("loop with no successful attach must give up at the cap, got nil err")
		}
		if !strings.Contains(err.Error(), "session lost") {
			t.Fatalf("err = %v, want a 'session lost' cap error", err)
		}
		// maxConsecutiveFailures is 6: the loop stops on the 6th failed attach.
		if calls != 6 {
			t.Fatalf("attach called %d times, want exactly 6 (the consecutive cap)", calls)
		}
	})
}

// TestClassifyChildExit covers the three terminal cases a
// child-exit produces: each must map to the right loop action. The bug was that a
// no-exit-status drop classified as a clean stop (transportErr nil) and the
// loop never reconnected.
func TestClassifyChildExit(t *testing.T) {
	t.Run("clean exit 0 → stop cleanly", func(t *testing.T) {
		act := classifyChildExit(0, true, nil)
		if act.kind != childExitCleanStop {
			t.Fatalf("exit 0: kind = %v, want childExitCleanStop", act.kind)
		}
	})
	t.Run("non-zero exit → stop with code", func(t *testing.T) {
		act := classifyChildExit(7, true, nil)
		if act.kind != childExitStopWithCode || act.exitCode != 7 {
			t.Fatalf("exit 7: act = %+v, want stop-with-code 7", act)
		}
	})
	t.Run("no exit status (drop) → reconnect", func(t *testing.T) {
		act := classifyChildExit(0, false, nil)
		if act.kind != childExitReconnect {
			t.Fatalf("no status: kind = %v, want childExitReconnect", act.kind)
		}
	})
	t.Run("attach error (transportErr) → reconnect", func(t *testing.T) {
		act := classifyChildExit(0, false, io.ErrUnexpectedEOF)
		if act.kind != childExitReconnect {
			t.Fatalf("transportErr: kind = %v, want childExitReconnect", act.kind)
		}
	})
}

// --- minimal fake devbox-session agent (mirrors internal/session's test infra) ---

type fakeAgent struct {
	hostSigner gossh.Signer
	hostPub    string
	handle     func(frame map[string]any, ch gossh.Channel)
}

func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeAgent{hostSigner: signer, hostPub: string(gossh.MarshalAuthorizedKey(signer.PublicKey()))}
}

func (a *fakeAgent) serve(t *testing.T, netConn net.Conn) {
	cfg := &gossh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(a.hostSigner)
	sconn, chans, reqs, err := gossh.NewServerConn(netConn, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go gossh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(gossh.UnknownChannelType, "only session")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go a.handleChannel(ch, creqs)
	}
}

func (a *fakeAgent) handleChannel(ch gossh.Channel, reqs <-chan *gossh.Request) {
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			_ = req.Reply(true, nil)
		case "window-change":
		case "subsystem":
			_ = req.Reply(true, nil)
			a.dispatch(ch)
			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func (a *fakeAgent) dispatch(ch gossh.Channel) {
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
	accepted := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		accepted <- res{c, err}
	}()
	cConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-accepted
	if r.err != nil {
		t.Fatal(r.err)
	}
	return cConn, r.c
}

func (a *fakeAgent) dialClient(t *testing.T) *session.Client {
	t.Helper()
	cConn, sConn := pipePair(t)
	go a.serve(t, sConn)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) { return cConn, nil }
	cl, err := session.Dial(context.Background(), dial, "10.0.0.1", 22, "dev", a.hostPub, false)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

// TestAttachOnceClassifiesTerminalCases drives the REAL attachOnce (with the
// compositor stubbed to return OutcomeChildExit) against a fake agent through
// the real session.Client, proving the classification end-to-end: an agent exit-status of N
// surfaces as (exitKnown, code=N); a channel close with NO exit-status surfaces
// as a non-nil transportErr so runSessionLoop reconnects.
func TestAttachOnceClassifiesTerminalCases(t *testing.T) {
	// Stub the compositor: it always reports the child exited (the inner stream
	// ended), so attachOnce proceeds to classify via att.Wait().
	prev := runCompositor
	runCompositor = func(inner compositor.Inner, _ string) compositor.Outcome {
		// Drain so the channel reaches EOF (as the real compositor would on inner
		// stream end), then report child exit.
		go func() { _, _ = io.Copy(io.Discard, inner) }()
		return compositor.OutcomeChildExit
	}
	t.Cleanup(func() { runCompositor = prev })

	cases := []struct {
		name         string
		handle       func(frame map[string]any, ch gossh.Channel)
		wantKind     childExitKind
		wantExitCode int
	}{
		{
			name: "clean exit 0",
			handle: func(_ map[string]any, ch gossh.Channel) {
				_, _ = io.WriteString(ch, `{"ok":true,"id":"s1","name":"main"}`+"\n")
				_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
				_ = ch.Close()
			},
			wantKind: childExitCleanStop,
		},
		{
			name: "non-zero exit",
			handle: func(_ map[string]any, ch gossh.Channel) {
				_, _ = io.WriteString(ch, `{"ok":true,"id":"s1","name":"main"}`+"\n")
				_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 5})
				_ = ch.Close()
			},
			wantKind:     childExitStopWithCode,
			wantExitCode: 5,
		},
		{
			name: "no exit status → transport drop → reconnect",
			handle: func(_ map[string]any, ch gossh.Channel) {
				_, _ = io.WriteString(ch, `{"ok":true,"id":"s1","name":"main"}`+"\n")
				_ = ch.Close() // NO exit-status: a mid-session drop
			},
			wantKind: childExitReconnect,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newFakeAgent(t)
			a.handle = c.handle
			sc := a.dialClient(t)
			id := "s1"
			outcome, attached, code, known, terr := attachOnce(context.Background(), sc, "ws-test", &id, "", nil)
			if outcome != compositor.OutcomeChildExit {
				t.Fatalf("outcome = %v, want OutcomeChildExit", outcome)
			}
			// Every case here reaches the compositor, so the attach succeeded —
			// attached must be true (drives the consecutive-failure-counter reset).
			if !attached {
				t.Fatal("attachOnce reached the compositor; attached must be true")
			}
			act := classifyChildExit(code, known, terr)
			if act.kind != c.wantKind {
				t.Fatalf("kind = %v, want %v (code=%d known=%v terr=%v)", act.kind, c.wantKind, code, known, terr)
			}
			if c.wantKind == childExitStopWithCode && act.exitCode != c.wantExitCode {
				t.Fatalf("exitCode = %d, want %d", act.exitCode, c.wantExitCode)
			}
			if c.wantKind == childExitReconnect && terr == nil {
				t.Fatal("a no-status drop must yield a non-nil transportErr")
			}
		})
	}
}
