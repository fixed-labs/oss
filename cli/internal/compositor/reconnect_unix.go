//go:build unix

package compositor

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	uv "github.com/charmbracelet/ultraviolet"
	xterm "github.com/charmbracelet/x/term"
	"github.com/muesli/cancelreader"
	"golang.org/x/term"
)

// reconnectDefaultStatus is the card's status line before the loop reports an
// attempt number — shown immediately so the first frame is never blank.
const reconnectDefaultStatus = "Will reconnect automatically…"

// Reconnecting is the transient overlay the reconnect loop raises while a dropped
// connection is re-established. It owns the terminal (raw mode + alt screen) for
// its lifetime, so the gap between sessions shows a clean "Disconnected" card
// instead of a frozen session, a reverted shell, or echoed keystrokes.
//
// Lifecycle, driven from run_session.go on a SINGLE goroutine:
//
//	r := ShowReconnecting(label, cancel) // a transport drop was detected
//	r.SetStatus("…(attempt 2)")          // each reconnect attempt
//	r.Close()                            // the live session is about to repaint
//
// Ctrl-C / Ctrl-D while the card is up invokes onAbort once (the loop cancels and
// stops cleanly) — necessary because raw mode means a tty Ctrl-C no longer raises
// SIGINT on its own. On a non-tty / chrome-disabled stdout it degrades to a single
// stderr line per status update; the connection still reconnects, there's just no
// terminal to draw on.
type Reconnecting struct {
	// fallback is set when there's no terminal to draw on: SetStatus prints to
	// stderr and Close is a no-op.
	fallback bool

	statusCh  chan string
	doneCh    chan struct{}
	stopped   chan struct{}
	cancelIn  func() // unblocks the abort watcher at Close (nil if uncancelable)
	closeOnce sync.Once
	abortOnce sync.Once
}

// ShowReconnecting takes over the terminal and draws the disconnect overlay. label
// is the chrome text (compositor.Label(...)); onAbort, if non-nil, is invoked once
// when the user presses Ctrl-C / Ctrl-D while the card is up. It never returns nil.
func ShowReconnecting(label string, onAbort func()) *Reconnecting {
	cols, rows, tty := InitialSize()
	if !tty {
		return &Reconnecting{fallback: true}
	}
	inFd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(inFd)
	if err != nil {
		return &Reconnecting{fallback: true}
	}

	r := &Reconnecting{
		statusCh: make(chan string, 4),
		doneCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
	}

	env := uv.Environ(os.Environ())
	rs := newReconnectScreen(reconnectOut{}, env, label, cols, rows)

	// Cancelable stdin so the abort watcher can be unblocked at Close instead of
	// lingering on os.Stdin into the next session.
	var reader io.Reader = os.Stdin
	if cr, crErr := cancelreader.NewReader(os.Stdin); crErr == nil {
		reader = cr
		r.cancelIn = func() { cr.Cancel() }
	}

	// Mirror SIGWINCH into a resize channel so the card stays centered.
	resize := make(chan [2]int, 1)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	winchDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-winchDone:
				return
			case <-winch:
				if w, h, gerr := term.GetSize(inFd); gerr == nil && w > 0 && h > 0 {
					select { // latest-wins
					case <-resize:
					default:
					}
					select {
					case resize <- [2]int{w, h}:
					default:
					}
				}
			}
		}
	}()

	// Abort watcher: a Ctrl-C / Ctrl-D cancels the reconnect. Returns when the
	// cancelable reader is canceled at Close (or on any read error).
	if onAbort != nil {
		go func() {
			buf := make([]byte, 256)
			for {
				n, rerr := reader.Read(buf)
				if n > 0 && containsAbort(buf[:n]) {
					r.abortOnce.Do(onAbort)
				}
				if rerr != nil {
					return
				}
			}
		}()
	}

	// Render loop — the single owner of the screen, so no layer is touched off
	// this goroutine. It restores the terminal (alt screen + raw mode) on Close.
	go func() {
		defer close(r.stopped)
		rs.render()
		for {
			select {
			case <-r.doneCh:
				signal.Stop(winch)
				close(winchDone)
				rs.restore()
				_ = term.Restore(inFd, old)
				return
			case s := <-r.statusCh:
				rs.setStatus(s)
				rs.render()
			case sz := <-resize:
				rs.resize(sz[0], sz[1])
				rs.render()
			}
		}
	}()
	return r
}

// SetStatus updates the card's status line (e.g. the attempt number). On the
// fallback (non-tty) path it prints the status to stderr instead.
func (r *Reconnecting) SetStatus(s string) {
	if r.fallback {
		fmt.Fprintf(os.Stderr, "rift: disconnected — %s\n", s)
		return
	}
	select {
	case r.statusCh <- s:
	case <-r.doneCh:
	}
}

// Close restores the terminal (raw mode + alt screen) and stops the overlay,
// blocking until the render goroutine has fully torn down so the caller can safely
// repaint the live session next. Safe to call more than once.
func (r *Reconnecting) Close() {
	if r.fallback {
		return
	}
	r.closeOnce.Do(func() {
		if r.cancelIn != nil {
			r.cancelIn()
		}
		close(r.doneCh)
	})
	<-r.stopped
}

// reconnectScreen renders the disconnect overlay — the chrome bar plus a centered
// "Disconnected" card. It's split from the terminal plumbing in ShowReconnecting
// so the rendering is unit-testable against an in-memory writer.
type reconnectScreen struct {
	scr    *uv.TerminalScreen
	w      io.Writer
	label  string
	status string
	cols   int
	rows   int
}

func newReconnectScreen(w io.Writer, env uv.Environ, label string, cols, rows int) *reconnectScreen {
	if cols < 1 {
		cols = 1
	}
	if rows < chromeRows+1 {
		rows = chromeRows + 1
	}
	scr := uv.NewTerminalScreen(w, env)
	scr.EnterAltScreen()
	scr.Resize(cols, rows)
	scr.HideCursor()
	return &reconnectScreen{
		scr:    scr,
		w:      w,
		label:  sanitizeLabel(label),
		status: reconnectDefaultStatus,
		cols:   cols,
		rows:   rows,
	}
}

func (r *reconnectScreen) setStatus(s string) {
	if s == "" {
		s = reconnectDefaultStatus
	}
	r.status = s
}

func (r *reconnectScreen) resize(cols, rows int) {
	if cols < 1 || rows < chromeRows+1 {
		return
	}
	r.cols, r.rows = cols, rows
	r.scr.Resize(cols, rows)
}

// render paints the chrome bar and a centered "Disconnected" card over a blanked
// inner region. Bracketed in synchronized output like the live compositor's frames
// (see compositor.go's ansiSyncStart note) so a capable terminal applies it atomically.
func (r *reconnectScreen) render() {
	_, _ = io.WriteString(r.w, ansiSyncStart)
	defer func() { _, _ = io.WriteString(r.w, ansiSyncEnd) }()

	// Chrome bar up top, matching the live session's header for continuity.
	uv.NewStyledString(chromeBar(r.label, r.cols)).Draw(r.scr, uv.Rect(0, 0, r.cols, chromeRows))
	// Blank the inner region so no prior frame bleeds around the card.
	blank := uv.EmptyCell
	for y := chromeRows; y < r.rows; y++ {
		for x := 0; x < r.cols; x++ {
			r.scr.SetCell(x, y, &blank)
		}
	}
	// Centered card over the inner region, reusing the help overlay's box drawing.
	ov := &overlay{
		title: "Disconnected",
		lines: []string{r.status, "", "Press Ctrl-C to cancel."},
	}
	ov.draw(r.scr, r.cols, r.rows)
	r.scr.HideCursor()
	r.scr.Render()
	_ = r.scr.Flush()
}

func (r *reconnectScreen) restore() {
	r.scr.ShowCursor()
	r.scr.ExitAltScreen()
	r.scr.Render()
	_ = r.scr.Flush()
}

// containsAbort reports whether b carries an interrupt the card treats as "cancel
// the reconnect": Ctrl-C (ETX) or Ctrl-D (EOT).
func containsAbort(b []byte) bool {
	for _, c := range b {
		if c == 0x03 || c == 0x04 {
			return true
		}
	}
	return false
}

// reconnectOut adapts stdout into the writer the card renders to. Fd lets
// ultraviolet detect the terminal's color profile (without it the chrome's
// background color is silently stripped — see clientIO). Read/Close satisfy
// xterm.File but are unused: the card only ever writes.
type reconnectOut struct{}

var _ xterm.File = reconnectOut{}

func (reconnectOut) Read([]byte) (int, error)    { return 0, io.EOF }
func (reconnectOut) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (reconnectOut) Close() error                { return nil }
func (reconnectOut) Fd() uintptr                 { return os.Stdout.Fd() }
