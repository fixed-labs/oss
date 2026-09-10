package sshserver

// T8b(b–e) / T8c — banner delivery and Route caller-construction through the
// REAL SSH surfaces. The bootstrap fallback-path NS fixture (Bootstrap true,
// sd-pid nonexistent) routes to the runnable local Legacy spawn + banner (no
// root/nsenter needed); the T8c fixture has a LIVE sd-pid so handleSFTP's
// in-target command construction is observable via the seam-(vi) captured-Spec
// hook. The legacy byte-parity cases share the same server construction with
// ONLY the NS axis nil-ed.

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/nsexec"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// TestMain: the routed-sftp test re-execs THIS test binary with argv[1] ==
// "sftp-subsystem" — mirroring cmd/devboxes-agent's special invocation — so
// the child serves the sftp protocol on stdio and never runs the test suite.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sftp-subsystem" {
		if err := ServeSFTP(stdioTestRWC{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type stdioTestRWC struct{}

func (stdioTestRWC) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdioTestRWC) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdioTestRWC) Close() error                { return nil }

// The no-client banner's identifying token (§3.6); it cannot occur in any
// fixture command's output.
const tokNoClient = "client system not running"

// startServerCfg starts a server on an ephemeral loopback port after applying
// cfg (NS, SFTPExec, Sessions, …).
func startServerCfg(t *testing.T, table *Table, cfg func(*Server)) *Server {
	t.Helper()
	s := &Server{
		HostKeyPEM: testHostKeyPEM(t),
		Table:      table,
	}
	if cfg != nil {
		cfg(s)
	}
	// Find a free loopback port first (Server.Start listens on Addr itself).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.Addr = l.Addr().String()
	_ = l.Close()
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fallbackNS is the bootstrap fallback-path fixture (sd-pid nonexistent).
func fallbackNS(t *testing.T) *nsexec.NS {
	t.Helper()
	miss := filepath.Join(t.TempDir(), "absent")
	return &nsexec.NS{
		Bootstrap:        true,
		SDPidFile:        filepath.Join(miss, "sd-pid"),
		RecoveryBootFile: filepath.Join(miss, "recovery-boot"),
		BootRungFile:     filepath.Join(miss, "boot-rung"),
	}
}

// specCapture records every Spec Route receives (seam vi).
type specCapture struct {
	mu    sync.Mutex
	specs []nsexec.Spec
}

func captureRouteSpecs(t *testing.T) *specCapture {
	t.Helper()
	c := &specCapture{}
	nsexec.SetRouteSpecObserver(func(s nsexec.Spec) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.specs = append(c.specs, s)
	})
	t.Cleanup(func() { nsexec.SetRouteSpecObserver(nil) })
	return c
}

// waitLast polls until at least one Spec was captured and returns the last.
func (c *specCapture) waitLast(t *testing.T) nsexec.Spec {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.specs)
		c.mu.Unlock()
		if n > 0 {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.specs[n-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no Spec captured by the Route observer")
	return nsexec.Spec{}
}

// lockedBuf is a race-safe accumulating writer (SSH stderr/stdout arrive on
// server-driven goroutines).
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func assertNoTERM(t *testing.T, env []string) {
	t.Helper()
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			t.Fatalf("Spec.Env carries a TERM entry on a non-PTY exec: %q", env)
		}
	}
}

// T8b(b), exec variant: a pty'd exec's output BEGINS with the banner, and the
// caller passes Login/RawCommand/TERM through the Spec (the caller-side
// construction Route-level tests cannot see).
func TestRoutedPTYExecBannerFirstAndSpec(t *testing.T) {
	rc := captureRouteSpecs(t)
	t.Setenv("RIFT_LOGIN_SHELL", "/bin/sh")
	srv := startServerCfg(t, authorizedTable(t), func(s *Server) { s.NS = fallbackNS(t) })
	client := dial(t, srv.Addr)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// Sentinel TERM: cannot occur ambiently, so the captured Env entry is
	// provably the pty-req's, not the test process's.
	if err := sess.RequestPty("rift-term-sentinel-b", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sess.Stdout = &out
	if err := sess.Run("printf RIFT_PTYEXEC_OUT_3"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := out.String()
	idxB := strings.Index(got, tokNoClient)
	idxO := strings.Index(got, "RIFT_PTYEXEC_OUT_3")
	if idxB < 0 || idxO < 0 || idxB > idxO {
		t.Fatalf("banner must precede the command output (banner %d, output %d): %q", idxB, idxO, got)
	}
	// "Begins with": only the banner's warning-glyph prefix may precede the token.
	if idxB > 8 {
		t.Fatalf("output does not BEGIN with the banner (token at %d): %q", idxB, got)
	}

	sp := rc.waitLast(t)
	if sp.Login != currentUser(t) {
		t.Fatalf("Spec.Login = %q, want the resolved login user %q", sp.Login, currentUser(t))
	}
	if sp.Command != "printf RIFT_PTYEXEC_OUT_3" {
		t.Fatalf("Spec.Command = %q, want the raw command", sp.Command)
	}
	if len(sp.Env) != 1 || sp.Env[0] != "TERM=rift-term-sentinel-b" {
		t.Fatalf("Spec.Env = %q, want exactly [TERM=rift-term-sentinel-b]", sp.Env)
	}
}

// T8b(b), shell variant (Sessions nil ⇒ the legacy one-shot PTY path): the
// interactive shell's stream begins with the banner; the Spec carries an
// EMPTY Command (a dropped RawCommand-for-exec regression is the inverse case
// above) and the pty-req TERM.
func TestRoutedBarePTYShellBannerAndSpec(t *testing.T) {
	rc := captureRouteSpecs(t)
	t.Setenv("RIFT_LOGIN_SHELL", "/bin/sh")
	srv := startServerCfg(t, authorizedTable(t), func(s *Server) { s.NS = fallbackNS(t) })
	client := dial(t, srv.Addr)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestPty("rift-term-shell-b", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &lockedBuf{}
	sess.Stdout = out
	if err := sess.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), tokNoClient) {
		time.Sleep(5 * time.Millisecond)
	}
	got := out.String()
	idxB := strings.Index(got, tokNoClient)
	if idxB < 0 {
		t.Fatalf("shell stream never showed the banner: %q", got)
	}
	if idxB > 8 {
		t.Fatalf("shell stream does not BEGIN with the banner (token at %d): %q", idxB, got)
	}
	_, _ = stdin.Write([]byte("exit\n"))

	sp := rc.waitLast(t)
	if sp.Login != currentUser(t) {
		t.Fatalf("Spec.Login = %q, want %q", sp.Login, currentUser(t))
	}
	if sp.Command != "" {
		t.Fatalf("Spec.Command = %q, want empty for an interactive shell", sp.Command)
	}
	if len(sp.Env) != 1 || sp.Env[0] != "TERM=rift-term-shell-b" {
		t.Fatalf("Spec.Env = %q, want exactly [TERM=rift-term-shell-b]", sp.Env)
	}
}

// T8b(c): non-PTY exec — banner on stderr, stdout BYTE-CLEAN for the
// command's own output; and the captured Spec carries NO TERM entry (there is
// no terminal; a spurious TERM injection would itself be the regression —
// the sibling PTY cases above prove the same construction DOES carry one).
func TestRoutedNonPTYExecBannerOnStderrStdoutByteClean(t *testing.T) {
	rc := captureRouteSpecs(t)
	t.Setenv("RIFT_LOGIN_SHELL", "/bin/sh")
	srv := startServerCfg(t, authorizedTable(t), func(s *Server) { s.NS = fallbackNS(t) })
	client := dial(t, srv.Addr)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var so, se bytes.Buffer
	sess.Stdout = &so
	sess.Stderr = &se
	if err := sess.Run("printf RIFT_EXEC_OUT_9"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if so.String() != "RIFT_EXEC_OUT_9" {
		t.Fatalf("stdout = %q, want exactly %q (byte-clean)", so.String(), "RIFT_EXEC_OUT_9")
	}
	if !strings.Contains(se.String(), tokNoClient) {
		t.Fatalf("stderr = %q, want the banner", se.String())
	}

	sp := rc.waitLast(t)
	if sp.Login != currentUser(t) {
		t.Fatalf("Spec.Login = %q, want %q", sp.Login, currentUser(t))
	}
	if sp.Command != "printf RIFT_EXEC_OUT_9" {
		t.Fatalf("Spec.Command = %q, want the raw command", sp.Command)
	}
	assertNoTERM(t, sp.Env)
}

// T8b(d): sftp on the fallback path still FUNCTIONS — the protocol rides
// stdout (a real create/read round-trip via the re-exec'd child), the banner
// rides stderr only.
func TestRoutedSFTPFallbackBannerOnStderr(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	srv := startServerCfg(t, authorizedTable(t), func(s *Server) {
		s.NS = fallbackNS(t)
		s.SFTPExec = self
	})
	client := dial(t, srv.Addr)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// RequestSubsystem never starts the Stderr copy goroutine, so read the
	// extended-data stream directly (like the stdout/stdin pipes).
	stderr, err := sess.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	se := &lockedBuf{}
	go func() { _, _ = io.Copy(se, stderr) }()
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("subsystem: %v", err)
	}
	sc, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		t.Fatalf("sftp client (protocol corrupted by the banner?): %v", err)
	}
	defer sc.Close()

	// Positive anchor: the protocol works end-to-end over stdout.
	dir := t.TempDir()
	f, err := sc.Create(dir + "/hello.txt")
	if err != nil {
		t.Fatalf("sftp create: %v", err)
	}
	if _, err := f.Write([]byte("via routed sftp")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	rf, err := sc.Open(dir + "/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rf)
	_ = rf.Close()
	if err != nil || string(b) != "via routed sftp" {
		t.Fatalf("sftp read-back: %q %v", b, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(se.String(), tokNoClient) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(se.String(), tokNoClient) {
		t.Fatalf("stderr = %q, want the banner", se.String())
	}
}

// T8b(e): with NO nsexec wired (nil NS — every deployed legacy agent), the
// SSH surface is byte-identical to today: the exec output is EXACTLY the
// independently pinned sentinel stream, nothing prepended or injected. The
// routed siblings above share this construction modulo the NS axis, arming
// these negatives.
func TestLegacyByteParityAtSSHSurface(t *testing.T) {
	t.Setenv("RIFT_LOGIN_SHELL", "/bin/sh")
	srv := startServerCfg(t, authorizedTable(t), nil) // NS nil, Sessions nil
	client := dial(t, srv.Addr)

	t.Run("non-pty exec", func(t *testing.T) {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		var so, se bytes.Buffer
		sess.Stdout = &so
		sess.Stderr = &se
		if err := sess.Run("printf RIFT_PARITY_STREAM_e5"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if so.String() != "RIFT_PARITY_STREAM_e5" {
			t.Fatalf("stdout = %q, want exactly the sentinel stream", so.String())
		}
		if se.Len() != 0 {
			t.Fatalf("stderr = %q, want empty (no spurious banner writes)", se.String())
		}
	})

	t.Run("pty exec", func(t *testing.T) {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		if err := sess.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		sess.Stdout = &out
		if err := sess.Run("printf RIFT_PARITY_PTY_e6"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if out.String() != "RIFT_PARITY_PTY_e6" {
			t.Fatalf("pty stream = %q, want exactly the sentinel stream", out.String())
		}
	})
}

// T8c: with a live client system, handleSFTP passes Route the IN-TARGET
// command (the self-copied persist agent), never s.SFTPExec — the fixture
// keeps the two distinct so the outcomes are distinguishable (T11's stated
// hazard: rung 1 binds the volume store over the image store, so the agent's
// own path does not resolve after nsenter -m).
func TestHandleSFTPRoutesInTargetCommand(t *testing.T) {
	rc := captureRouteSpecs(t)
	dir := t.TempDir()
	sd := filepath.Join(dir, "sd-pid")
	if err := os.WriteFile(sd, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ns := &nsexec.NS{
		Bootstrap:        true,
		SDPidFile:        sd,
		RecoveryBootFile: filepath.Join(dir, "recovery-boot"),
		BootRungFile:     filepath.Join(dir, "boot-rung"),
		RunuserPath:      "/run/current-system/sw/bin/runuser",
		PersistAgentPath: "/rift-test/persist-agent",
	}
	srv := startServerCfg(t, authorizedTable(t), func(s *Server) {
		s.NS = ns
		s.SFTPExec = "/rift-test/sftp-exec-distinct"
	})
	if ns.InTargetSFTPCommand() == srv.SFTPExec {
		t.Fatal("fixture broken: the in-target command must differ from SFTPExec")
	}
	client := dial(t, srv.Addr)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// The nsenter spawn itself fails without root — irrelevant: the seam
	// captures what the caller passed BEFORE the spawn.
	_ = sess.RequestSubsystem("sftp")

	sp := rc.waitLast(t)
	if sp.Command != "/rift-test/persist-agent sftp-subsystem" {
		t.Fatalf("Spec.Command = %q, want the in-target persist-agent invocation", sp.Command)
	}
	if sp.Command == srv.SFTPExec {
		t.Fatal("handleSFTP routed the image-store SFTPExec path into the target")
	}
	if sp.Login != currentUser(t) {
		t.Fatalf("Spec.Login = %q, want %q", sp.Login, currentUser(t))
	}
}
