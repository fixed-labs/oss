//go:build linux

package compositor

import (
	"bytes"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// safeBuf is a concurrency-safe accumulator for the byte streams the test polls.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, b *safeBuf, sub, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains([]byte(b.String()), []byte(sub)) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: never saw %q in stream:\n%q", msg, sub, b.String())
}

// mockInner is a compositor.Inner driven by the test: serverWrite feeds bytes
// the compositor renders (server→client output), and everything the compositor
// writes (client→server keystrokes) is captured. Resize records the last size.
type mockInner struct {
	mu sync.Mutex

	toClient   chan []byte // bytes the compositor reads (server output)
	fromClient safeBuf     // bytes the compositor wrote (keystrokes)
	closed     chan struct{}
	closeOnce  sync.Once

	lastCols, lastRows int
	resizes            int
}

func newMockInner() *mockInner {
	return &mockInner{
		toClient: make(chan []byte, 256),
		closed:   make(chan struct{}),
	}
}

func (m *mockInner) Read(p []byte) (int, error) {
	select {
	case b, ok := <-m.toClient:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, b)
		return n, nil
	case <-m.closed:
		return 0, io.EOF
	}
}

func (m *mockInner) Write(p []byte) (int, error) {
	select {
	case <-m.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	return m.fromClient.Write(p)
}

func (m *mockInner) Resize(cols, rows int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastCols, m.lastRows = cols, rows
	m.resizes++
	return nil
}

func (m *mockInner) Close() error {
	m.closeOnce.Do(func() { close(m.closed) })
	return nil
}

func (m *mockInner) size() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastCols, m.lastRows
}

// serverOut queues bytes for the compositor to render.
func (m *mockInner) serverOut(b []byte) { m.toClient <- b }

// ttyClient is a fake "user terminal": input from a channel, output captured.
// It is NOT a tty (Fd is unused by composite — only run_unix's clientIO needs
// the real Fd), so colorprofile stays in no-color mode, which is fine here.
type ttyClient struct {
	in  chan []byte
	out *safeBuf
	rem []byte
}

func (c *ttyClient) Read(p []byte) (int, error) {
	if len(c.rem) == 0 {
		b, ok := <-c.in
		if !ok {
			return 0, io.EOF
		}
		c.rem = b
	}
	n := copy(p, c.rem)
	c.rem = c.rem[n:]
	return n, nil
}

func (c *ttyClient) Write(p []byte) (int, error) { return c.out.Write(p) }

// TestResizeRedrawsRetainedContent guards the "resizes after the first paint the
// screen with background-colored (black) blanks" bug: vt clears its touched set
// on resize and only redraws touched lines, so a resize blanked the retained
// grid until the box app repainted. The compositor must re-touch the grid so the
// retained content is redrawn after EVERY resize, not just the first.
func TestResizeRedrawsRetainedContent(t *testing.T) {
	inner := newMockInner()
	clientIn := make(chan []byte, 16)
	var clientOut safeBuf
	client := &ttyClient{in: clientIn, out: &clientOut}
	resize := make(chan [2]int, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		composite(client, inner, resize, 80, 24,
			uv.Environ{"TERM=xterm-256color"}, "rift: ws ", nil)
	}()
	waitFor(t, &clientOut, "rift: ws", "chrome")
	inner.serverOut([]byte("ZZ_PERSIST_ZZ line one\r\n"))
	waitFor(t, &clientOut, "ZZ_PERSIST_ZZ", "initial content")

	waitInner := func(cols, rows int) {
		t.Helper()
		d := time.Now().Add(2 * time.Second)
		for time.Now().Before(d) {
			if c, r := inner.size(); c == cols && r == rows {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("inner never resized to %dx%d", cols, rows)
	}

	resize <- [2]int{100, 30}
	waitInner(100, 30-chromeRows)

	// The second resize is the regression: the retained "ZZ_PERSIST_ZZ" must be
	// REDRAWN in the post-resize frame(s), not blanked to a bg-colored block.
	snap := len(clientOut.String())
	resize <- [2]int{90, 28}
	waitInner(90, 28-chromeRows)
	d := time.Now().Add(3 * time.Second)
	for time.Now().Before(d) {
		if bytes.Contains([]byte(clientOut.String()[snap:]), []byte("ZZ_PERSIST_ZZ")) {
			return // content redrawn after the 2nd resize
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("retained content not redrawn after 2nd resize (blanked); post-resize stream:\n%q",
		clientOut.String()[snap:])
}

// TestCursorAdvancesOnCursorOnlyChange guards the off-by-one cursor regression: a
// keystroke that only moves the emulator cursor without changing a cell (typing a
// space at the line end, or a backspace) must still move the VISIBLE cursor in the
// same frame. uv strands a content-less cursor move in the renderer's buffer for a
// frame; render()'s second Render()+Flush() flushes it now. (Expected escape is
// uv's absolute CUP in alt-screen: row 2 = chrome row 1 + 1, col 3 = cursor X 2 + 1.)
func TestCursorAdvancesOnCursorOnlyChange(t *testing.T) {
	inner := newMockInner()
	clientIn := make(chan []byte, 16)
	var clientOut safeBuf
	client := &ttyClient{in: clientIn, out: &clientOut}
	resize := make(chan [2]int, 4)
	go composite(client, inner, resize, 80, 24,
		uv.Environ{"TERM=xterm-256color"}, "ws ", nil)
	waitFor(t, &clientOut, "ws", "chrome")

	feed := func(b string) string {
		snap := len(clientOut.String())
		inner.serverOut([]byte(b))
		d := time.Now().Add(2 * time.Second)
		for time.Now().Before(d) {
			if len(clientOut.String()) > snap {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(80 * time.Millisecond) // settle frame coalescing + 2nd pass
		return clientOut.String()[snap:]
	}

	feed("a") // 'a' at col 1; cursor advances to col 2
	// A space advances the emulator cursor (to col 3) but changes no cell — the
	// frame must still reposition the visible cursor.
	if d := feed(" "); !bytes.Contains([]byte(d), []byte("\x1b[2;3H")) {
		t.Fatalf("space did not advance the cursor to row2/col3; frame=%q", d)
	}
	feed("b") // 'b' at col 3; cursor advances to col 4
	// First backspace moves the emulator cursor back to col 3 (no cell change).
	if d := feed("\b"); !bytes.Contains([]byte(d), []byte("\x1b[2;3H")) {
		t.Fatalf("backspace did not move the cursor back to row2/col3; frame=%q", d)
	}
}

// TestCursorHiddenDuringRepaint guards the "cursor jumps around as it repaints"
// regression: a frame that changes cells must hide the terminal cursor BEFORE
// emitting the diff and show it AFTER, so the cursor never walks across the
// changed cells on a terminal that ignores synchronized output (mode 2026).
// The bug was scr.SetSynchronizedUpdates(true), which made ultraviolet wrap the
// frame in 2026 but skip the cursor hide/show — leaving the cursor visible while
// the renderer moved it cell by cell. We must also still emit the 2026 wrap so
// capable terminals get atomic frames.
func TestCursorHiddenDuringRepaint(t *testing.T) {
	inner := newMockInner()
	clientIn := make(chan []byte, 16)
	var clientOut safeBuf
	client := &ttyClient{in: clientIn, out: &clientOut}
	go composite(client, inner, make(chan [2]int, 4), 80, 24,
		uv.Environ{"TERM=xterm-256color"}, "ws ", nil)
	waitFor(t, &clientOut, "ws", "chrome")

	snap := len(clientOut.String())
	inner.serverOut([]byte("REPAINT_ME_NOW"))
	waitFor(t, &clientOut, "REPAINT_ME_NOW", "inner content")
	time.Sleep(80 * time.Millisecond) // settle frame coalescing + 2nd pass
	frame := clientOut.String()[snap:]

	const (
		hideSeq = "\x1b[?25l"
		showSeq = "\x1b[?25h"
	)
	content := strings.Index(frame, "REPAINT_ME_NOW")
	hide := strings.LastIndex(frame[:content], hideSeq) // hide closest before the content
	show := strings.Index(frame[content:], showSeq)     // show after the content
	if hide < 0 {
		t.Fatalf("cursor not hidden during repaint — the diff is emitted with the cursor visible "+
			"(regression: synchronized-output-only path). frame=%q", frame)
	}
	if show < 0 {
		t.Fatalf("cursor not re-shown after repaint; frame=%q", frame)
	}
	// And capable terminals still get an atomic frame.
	if !strings.Contains(frame, ansiSyncStart) {
		t.Fatalf("frame not wrapped in synchronized output (mode 2026); frame=%q", frame)
	}
}

// TestCompositeLayersAndInnerSize asserts the three-layer screen manager: the
// chrome header is painted, the inner region runs chromeRows shorter, and inner
// output is composited below the chrome.
func TestCompositeLayersAndInnerSize(t *testing.T) {
	inner := newMockInner()
	clientIn := make(chan []byte, 16)
	var clientOut safeBuf
	client := &ttyClient{in: clientIn, out: &clientOut}

	resize := make(chan [2]int, 1)
	compDone := make(chan struct{})
	var outcome Outcome
	go func() {
		defer close(compDone)
		outcome = composite(client, inner, resize, 80, 24,
			uv.Environ{"TERM=xterm-256color"}, "rift: ws-test ", nil)
	}()

	// 1. Chrome painted on the first frame.
	waitFor(t, &clientOut, "rift: ws-test", "chrome header not rendered")

	// 2. The inner region's reported size is the POST-chrome size (24-chromeRows).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, r := inner.size(); c == 80 && r == 24-chromeRows {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c, r := inner.size(); c != 80 || r != 24-chromeRows {
		t.Fatalf("inner size = %dx%d, want %dx%d", c, r, 80, 24-chromeRows)
	}

	// 3. Inner output is composited.
	inner.serverOut([]byte("hello-from-box\r\n"))
	waitFor(t, &clientOut, "hello-from-box", "inner output not composited")

	// Resize: inner shrinks by chromeRows.
	resize <- [2]int{100, 40}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, r := inner.size(); c == 100 && r == 40-chromeRows {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c, r := inner.size(); c != 100 || r != 40-chromeRows {
		t.Fatalf("after resize inner size = %dx%d, want %dx%d", c, r, 100, 40-chromeRows)
	}

	// Inner EOF (server end) → OutcomeChildExit.
	inner.Close()
	select {
	case <-compDone:
	case <-time.After(5 * time.Second):
		t.Fatal("composite did not tear down on inner EOF")
	}
	if outcome != OutcomeChildExit {
		t.Fatalf("inner EOF outcome = %v, want OutcomeChildExit", outcome)
	}
}

// TestCompositeForwardsKeystrokes asserts client input reaches the inner session.
func TestCompositeForwardsKeystrokes(t *testing.T) {
	inner := newMockInner()
	clientIn := make(chan []byte, 16)
	var clientOut safeBuf
	client := &ttyClient{in: clientIn, out: &clientOut}

	compDone := make(chan struct{})
	go func() {
		defer close(compDone)
		_ = composite(client, inner, make(chan [2]int, 1), 80, 24,
			uv.Environ{"TERM=xterm-256color"}, "rift: ws ", nil)
	}()
	waitFor(t, &clientOut, "rift: ws", "chrome not rendered")

	clientIn <- []byte("probe-input")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains([]byte(inner.fromClient.String()), []byte("probe-input")) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !bytes.Contains([]byte(inner.fromClient.String()), []byte("probe-input")) {
		t.Fatalf("keystrokes not forwarded to inner: %q", inner.fromClient.String())
	}
	inner.Close()
	<-compDone
}

// TestCompositeDetachOutcome asserts a ~d escape at line start tears down with
// OutcomeDetach (not OutcomeChildExit), and the escape bytes are NOT forwarded.
func TestCompositeDetachOutcome(t *testing.T) {
	inner := newMockInner()
	clientIn := make(chan []byte, 16)
	var clientOut safeBuf
	client := &ttyClient{in: clientIn, out: &clientOut}

	var outcome Outcome
	compDone := make(chan struct{})
	go func() {
		defer close(compDone)
		outcome = composite(client, inner, make(chan [2]int, 1), 80, 24,
			uv.Environ{"TERM=xterm-256color"}, "rift: ws ", nil)
	}()
	waitFor(t, &clientOut, "rift: ws", "chrome not rendered")

	// First byte of the connection is line start; ~d detaches.
	clientIn <- []byte("~d")
	select {
	case <-compDone:
	case <-time.After(5 * time.Second):
		t.Fatal("composite did not tear down on ~d")
	}
	if outcome != OutcomeDetach {
		t.Fatalf("~d outcome = %v, want OutcomeDetach", outcome)
	}
	if bytes.Contains([]byte(inner.fromClient.String()), []byte("~d")) {
		t.Fatalf("detach escape leaked to the inner session: %q", inner.fromClient.String())
	}
}

// TestCompositeSwitchOutcome asserts ~s tears down with OutcomeSwitch.
func TestCompositeSwitchOutcome(t *testing.T) {
	inner := newMockInner()
	clientIn := make(chan []byte, 16)
	var clientOut safeBuf
	client := &ttyClient{in: clientIn, out: &clientOut}

	var outcome Outcome
	compDone := make(chan struct{})
	go func() {
		defer close(compDone)
		outcome = composite(client, inner, make(chan [2]int, 1), 80, 24,
			uv.Environ{"TERM=xterm-256color"}, "rift: ws ", nil)
	}()
	waitFor(t, &clientOut, "rift: ws", "chrome not rendered")
	clientIn <- []byte("~s")
	select {
	case <-compDone:
	case <-time.After(5 * time.Second):
		t.Fatal("composite did not tear down on ~s")
	}
	if outcome != OutcomeSwitch {
		t.Fatalf("~s outcome = %v, want OutcomeSwitch", outcome)
	}
}

// TestCompositeClientGone asserts a client EOF (local terminal closed) tears
// down with OutcomeClientGone.
func TestCompositeClientGone(t *testing.T) {
	inner := newMockInner()
	clientConn, testConn := net.Pipe()
	var clientOut safeBuf
	go func() { _, _ = io.Copy(&clientOut, testConn) }()

	var outcome Outcome
	compDone := make(chan struct{})
	go func() {
		defer close(compDone)
		outcome = composite(clientConn, inner, make(chan [2]int, 1), 80, 24,
			uv.Environ{"TERM=xterm-256color"}, "rift: ws ",
			func() { _ = clientConn.Close() })
	}()
	waitFor(t, &clientOut, "rift: ws", "chrome not rendered")

	// Close the client's input side → composite's input reader hits EOF.
	_ = testConn.Close()
	select {
	case <-compDone:
	case <-time.After(5 * time.Second):
		t.Fatal("composite did not tear down on client EOF")
	}
	if outcome != OutcomeClientGone {
		t.Fatalf("client EOF outcome = %v, want OutcomeClientGone", outcome)
	}
}

// TestCompositeHelpOverlay asserts ~? raises the help overlay (drawn over the
// inner region) and the next keystroke dismisses it without reaching the inner
// session.
func TestCompositeHelpOverlay(t *testing.T) {
	inner := newMockInner()
	clientIn := make(chan []byte, 16)
	var clientOut safeBuf
	client := &ttyClient{in: clientIn, out: &clientOut}

	compDone := make(chan struct{})
	go func() {
		defer close(compDone)
		_ = composite(client, inner, make(chan [2]int, 1), 80, 24,
			uv.Environ{"TERM=xterm-256color"}, "rift: ws ", nil)
	}()
	waitFor(t, &clientOut, "rift: ws", "chrome not rendered")

	clientIn <- []byte("~?")
	waitFor(t, &clientOut, "escapes", "help overlay not rendered")

	// A dismiss keystroke must NOT reach the inner session.
	clientIn <- []byte("x")
	time.Sleep(100 * time.Millisecond)
	if bytes.Contains([]byte(inner.fromClient.String()), []byte("x")) {
		t.Fatalf("dismiss keystroke leaked to inner session: %q", inner.fromClient.String())
	}
	inner.Close()
	<-compDone
}

// TestCompositeMouseOffsetEndToEnd asserts an SGR mouse press from the client is
// forwarded to the inner session with the row reduced by chromeRows.
func TestCompositeMouseOffsetEndToEnd(t *testing.T) {
	inner := newMockInner()
	clientIn := make(chan []byte, 16)
	var clientOut safeBuf
	client := &ttyClient{in: clientIn, out: &clientOut}

	compDone := make(chan struct{})
	go func() {
		defer close(compDone)
		_ = composite(client, inner, make(chan [2]int, 1), 80, 24,
			uv.Environ{"TERM=xterm-256color"}, "rift: ws ", nil)
	}()
	waitFor(t, &clientOut, "rift: ws", "chrome not rendered")

	clientIn <- []byte("\x1b[<0;5;10M")
	want := "\x1b[<0;5;" + itoa(10-chromeRows) + "M"
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains([]byte(inner.fromClient.String()), []byte(want)) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !bytes.Contains([]byte(inner.fromClient.String()), []byte(want)) {
		t.Fatalf("mouse row not offset end-to-end: got %q, want substr %q", inner.fromClient.String(), want)
	}
	inner.Close()
	<-compDone
}
