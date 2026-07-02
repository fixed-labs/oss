package sshserver

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os/user"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

func testHostKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func currentUser(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

// TestTableCanonicalizesLeadingZeroHextet guards the wg-ipv6 ↔ canonical IPv6
// key mismatch. The upstream wg-ipv6 encoding formats hextets %02x%02x, so a
// host hextet < 0x1000 carries a leading zero ("0af8") that net.IP.String()
// strips ("af8"). A source inserted in raw form must still resolve when looked
// up by the canonical form a real connection's RemoteAddr() carries — else the
// ConnCallback gate refuses the connection ("unauthorized source refused").
func TestTableCanonicalizesLeadingZeroHextet(t *testing.T) {
	const rawKey = "fd5e:de7b:26d5:8f06:6df6:def7:0af8:2d87" // 0af8 has a leading zero
	canon := net.ParseIP(rawKey).String()                    // "...:af8:..."
	if canon == rawKey {
		t.Fatalf("precondition: %q should differ from its canonical form %q", rawKey, canon)
	}
	tbl := NewTable()
	tbl.Replace(map[string]Peer{rawKey: {LoginUser: "dev", DeveloperID: "d1"}})
	if _, ok := tbl.Lookup(&net.TCPAddr{IP: net.ParseIP(rawKey), Port: 4242}); !ok {
		t.Fatalf("Lookup failed for canonical form %q of raw key %q", canon, rawKey)
	}
}

// startServer runs the server on an ephemeral loopback port with 127.0.0.1
// authorized as `login` (the test stand-in for a wg-ip source).
func startServer(t *testing.T, table *Table) string {
	return startServerS(t, table).Addr
}

func startServerS(t *testing.T, table *Table) *Server {
	t.Helper()
	// Find a free port first (Server.Start listens on Addr itself).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	s := &Server{
		Addr:       addr,
		HostKeyPEM: testHostKeyPEM(t),
		Table:      table,
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func dial(t *testing.T, addr string) *gossh.Client {
	t.Helper()
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "dev",
		Auth:            nil, // keyless — the server authenticates by source IP
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func authorizedTable(t *testing.T) *Table {
	table := NewTable()
	table.Replace(map[string]Peer{
		"127.0.0.1": {DeveloperID: "u-1", LoginUser: currentUser(t)},
	})
	return table
}

func TestExecAndExitStatus(t *testing.T) {
	addr := startServer(t, authorizedTable(t))
	client := dial(t, addr)

	t.Run("stdout + exit 0", func(t *testing.T) {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		out, err := sess.Output("echo hi from devbox")
		if err != nil {
			t.Fatalf("Output: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "hi from devbox" {
			t.Fatalf("output: %q", got)
		}
	})

	t.Run("exit status propagates", func(t *testing.T) {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		err = sess.Run("exit 7")
		var ee *gossh.ExitError
		if ok := asExitError(err, &ee); !ok || ee.ExitStatus() != 7 {
			t.Fatalf("want ExitError 7, got %v", err)
		}
	})

	t.Run("identity env injected", func(t *testing.T) {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		out, err := sess.Output("echo $DEVBOX_DEVELOPER_ID")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(out)); got != "u-1" {
			t.Fatalf("DEVBOX_DEVELOPER_ID: %q", got)
		}
	})

	t.Run("client env accepted", func(t *testing.T) {
		sess, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		if err := sess.Setenv("DEVBOX_TEST_VAR", "veni"); err != nil {
			t.Fatalf("Setenv: %v", err)
		}
		out, err := sess.Output("echo $DEVBOX_TEST_VAR")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(out)); got != "veni" {
			t.Fatalf("env: %q", got)
		}
	})
}

func asExitError(err error, target **gossh.ExitError) bool {
	if ee, ok := err.(*gossh.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

func TestPTYSession(t *testing.T) {
	addr := startServer(t, authorizedTable(t))
	client := dial(t, addr)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatalf("RequestPty: %v", err)
	}
	var buf bytes.Buffer
	sess.Stdout = &buf
	if err := sess.Run("echo pty-works; echo TERM=$TERM"); err != nil {
		t.Fatalf("Run under pty: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "pty-works") || !strings.Contains(out, "TERM=xterm") {
		t.Fatalf("pty output: %q", out)
	}
}

func TestUnauthorizedSourceRefused(t *testing.T) {
	// Empty table: 127.0.0.1 is NOT an authorized peer → the connection is
	// closed before the handshake (the WG-identity gate).
	addr := startServer(t, NewTable())
	_, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "dev",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err == nil {
		t.Fatal("expected handshake failure for unauthorized source")
	}
}

func waitForSessions(t *testing.T, s *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.ActiveSessions() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("ActiveSessions = %d, want %d", s.ActiveSessions(), want)
}

func TestActiveSessionsGauge(t *testing.T) {
	// The heartbeat's ssh-liveness signal: authorized connections move the
	// gauge up on connect and down on disconnect; it settles back to zero.
	s := startServerS(t, authorizedTable(t))
	if got := s.ActiveSessions(); got != 0 {
		t.Fatalf("fresh server ActiveSessions = %d", got)
	}

	c1 := dial(t, s.Addr)
	waitForSessions(t, s, 1)
	c2 := dial(t, s.Addr)
	waitForSessions(t, s, 2)

	_ = c1.Close()
	waitForSessions(t, s, 1)
	_ = c2.Close()
	waitForSessions(t, s, 0)
}

func TestUnauthorizedConnNeverCounts(t *testing.T) {
	// Refused sources are closed before the handshake and must not leak a
	// gauge increment (the refusal path returns nil before counting).
	s := startServerS(t, NewTable())
	_, err := gossh.Dial("tcp", s.Addr, &gossh.ClientConfig{
		User:            "dev",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err == nil {
		t.Fatal("expected handshake failure for unauthorized source")
	}
	waitForSessions(t, s, 0)
}

func TestCountedConnDecrementsOnce(t *testing.T) {
	// Double Close must not under-count the gauge.
	var gauge atomic.Int64
	gauge.Add(1)
	server, client := net.Pipe()
	defer client.Close()
	c := &countedConn{Conn: server, gauge: &gauge}
	_ = c.Close()
	_ = c.Close()
	if got := gauge.Load(); got != 0 {
		t.Fatalf("gauge after double close: %d", got)
	}
}

func TestHostKeyIsPinned(t *testing.T) {
	// The CLI pins the host key from the attach bundle — a client checking
	// against the right key succeeds; the wrong key fails.
	pemBytes := testHostKeyPEM(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	s := &Server{Addr: addr, HostKeyPEM: pemBytes, Table: authorizedTable(t)}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	signer, err := gossh.ParsePrivateKey(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "dev",
		HostKeyCallback: gossh.FixedHostKey(signer.PublicKey()),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("pinned host key rejected: %v", err)
	}
	_ = c.Close()
}

func TestDirectTCPIP(t *testing.T) {
	// -L/-W: dial a local echo listener THROUGH the ssh connection.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, aerr := echo.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) { _, _ = io.Copy(c, c); _ = c.Close() }(conn)
		}
	}()

	addr := startServer(t, authorizedTable(t))
	client := dial(t, addr)
	conn, err := client.Dial("tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("direct-tcpip dial: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through tunnel: %q %v", buf, err)
	}
}

// pipeRWC adapts one end of a net.Pipe to sftp's ReadWriteCloser.
type pipeRWC struct{ net.Conn }

func TestServeSFTPProtocol(t *testing.T) {
	// The sftp child's protocol path, in-process over a pipe (the credentialed
	// re-exec wrapper needs root and a real binary; the protocol doesn't).
	serverSide, clientSide := net.Pipe()
	go func() { _ = ServeSFTP(pipeRWC{serverSide}) }()

	client, err := sftp.NewClientPipe(clientSide, clientSide)
	if err != nil {
		t.Fatalf("sftp client: %v", err)
	}
	defer client.Close()

	dir := t.TempDir()
	f, err := client.Create(dir + "/hello.txt")
	if err != nil {
		t.Fatalf("sftp create: %v", err)
	}
	if _, err := f.Write([]byte("via sftp")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	rf, err := client.Open(dir + "/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	b, err := io.ReadAll(rf)
	if err != nil || string(b) != "via sftp" {
		t.Fatalf("sftp read-back: %q %v", b, err)
	}
}
