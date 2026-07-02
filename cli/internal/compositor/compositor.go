//go:build unix

// Package compositor runs a remote interactive session (the devbox-session SSH
// subsystem's raw PTY channel) under a client-side chrome bar, compositing it
// onto the user's terminal.
//
// It is a three-layer screen manager:
//
//   - chrome   — the rows reserved at the top of the terminal (the devbox-id
//     header today; status/footer later). The compositor owns these rows; the
//     remote stream has no coordinate for them.
//   - inner    — one emulated child grid (a vt.SafeEmulator), bound to ONE
//     session. The remote shell and everything it spawns (vim, htop, less,
//     tmux — arriving as bytes over the SSH channel) render here, faithfully:
//     colour, paste, OSC, and alt-screen are preserved because the inner stream
//     is interpreted by a real terminal emulator (a byte-spatial pass-through
//     could not reserve the chrome rows and survive an alt-screen switch).
//   - overlay  — a transient layer drawn over the inner region: the session
//     picker, the switcher, connection toasts. When present it replaces the
//     inner content for those rows; dismissing it redraws the inner grid.
//
// Switching the inner region between sessions is instant (rebind + replay), with
// no teardown of the compositor — the inner-region↔session binding is the seam.
//
// This deliberately lives on the CLIENT (the devbox CLI), not the box. The
// chrome is a property of `devbox connect`, so it is never imposed on a direct
// ssh, scp, rsync, or IDE remote-ssh session into the box — those get a plain
// shell.
package compositor

import (
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
	"github.com/muesli/cancelreader"
)

// chromeRows is how many rows at the top of the terminal the chrome occupies.
// The inner region runs in the remaining (rows - chromeRows) rows. Today this
// is a single header row; it stays a named constant so a status/footer can grow
// it without hunting offsets.
const chromeRows = 1

// frameInterval caps the render rate and coalesces output bursts: after the
// first dirty signal we collect further output for this long, then paint one
// frame. ~8ms keeps keystroke echo imperceptibly fast while batching a `cat` of
// a large file into a handful of frames instead of thousands.
const frameInterval = 8 * time.Millisecond

// ansiSyncStart/ansiSyncEnd bracket each frame in synchronized output (DECSET
// 2026) so a terminal that supports it applies the whole frame as one atomic
// update — no tearing, and the cursor hide/show pair inside nets out invisibly.
// We emit these ourselves rather than via scr.SetSynchronizedUpdates because
// ultraviolet suppresses its OWN cursor-hide wrapping whenever sync updates are
// enabled (see render()), which is exactly the wrapping a non-2026 terminal
// relies on. Terminals that don't support 2026 ignore these sequences.
const (
	ansiSyncStart = "\x1b[?2026h"
	ansiSyncEnd   = "\x1b[?2026l"
)

// Outcome is the typed result of a composite() run — what ended the session,
// so the reconnect loop can branch without inferring intent from an ssh exit
// code.
type Outcome int

const (
	// OutcomeChildExit — the remote session ended on its own (the channel
	// closed, e.g. the agent's "session terminated, exit code N" then EOF, or
	// the transport dropped). The caller inspects the exit detail.
	OutcomeChildExit Outcome = iota
	// OutcomeClientGone — the user's local terminal/stdin went away (EOF). Stop
	// cleanly; this is not a transport failure.
	OutcomeClientGone
	// OutcomeDetach — the user pressed a detach escape (~d / ~.). The session is
	// left running on the box; stop the client.
	OutcomeDetach
	// OutcomeSwitch — the user pressed the switcher escape (~s). The caller
	// re-selects a session and rebinds, without re-dialling.
	OutcomeSwitch
)

func (o Outcome) String() string {
	switch o {
	case OutcomeChildExit:
		return "child-exit"
	case OutcomeClientGone:
		return "client-gone"
	case OutcomeDetach:
		return "detach"
	case OutcomeSwitch:
		return "switch"
	default:
		return "unknown"
	}
}

// Inner is the remote session the compositor binds its inner region to: a raw
// PTY byte stream (Read = server→client output, Write = client→server input)
// and a Resize hook the compositor calls with the POST-chrome size whenever the
// window changes (so the agent issues an SSH window-change for the channel).
//
// Read returning io.EOF (or any error) means the session/channel ended — the
// compositor tears down with OutcomeChildExit. Close is called at teardown for
// the detach/client-gone paths (it closes the channel = a clean detach on the
// wire); a child-exit needs no close (the channel is already gone).
type Inner interface {
	io.ReadWriteCloser
	// Resize tells the remote side the inner region is now cols x rows.
	Resize(cols, rows int) error
}

// composite bridges a remote session (inner) to the user's terminal (client),
// reserving the top chromeRows rows for the chrome bar. It blocks until the
// session ends (inner Read EOF/error), the client disconnects, or the user
// fires a detach/switch escape, then tears every goroutine down. It returns the
// typed Outcome describing why it stopped.
//
// cancelInput unblocks the client input reader (the escape state machine) at
// teardown so it can't outlive the session and steal stdin from a subsequent
// attach. It may be nil in tests.
func composite(client io.ReadWriter, inner Inner, resize <-chan [2]int, cols, rows int, env uv.Environ, label string, cancelInput func()) Outcome {
	if cols < 1 {
		cols = 1
	}
	if rows < chromeRows+1 {
		rows = chromeRows + 1 // guarantee at least one inner row
	}
	// Strip control bytes from the label once, up front: the chrome string is
	// fed to an SGR-parsing StyledString, so an ESC/SGR sequence in the devbox
	// id would otherwise hijack the bar's styling.
	label = sanitizeLabel(label)

	scr := uv.NewTerminalScreen(client, env)
	scr.EnterAltScreen() // own a clean full-screen buffer; restored on exit
	// NOTE: deliberately NOT scr.SetSynchronizedUpdates(true). That flag makes
	// uv's Flush wrap frames in mode 2026 but stop hiding the cursor around the
	// diff — so on a terminal that ignores 2026 the cursor is left visible while
	// the renderer walks it across every changed cell ("cursor jumps around as it
	// repaints"). Instead render() keeps uv in its cursor-hiding mode and wraps
	// each frame in 2026 itself (ansiSyncStart/End), getting both: cursor hidden
	// during the repaint everywhere, atomic frames where 2026 is supported.
	scr.Resize(cols, rows)
	scr.ShowCursor()

	mgr := newScreenManager(scr, client, label, cols, rows)
	emu := mgr.emu
	_ = inner.Resize(cols, rows-chromeRows)

	done := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(done) }) }

	dirty := make(chan struct{}, 1)
	markDirty := func() {
		select {
		case dirty <- struct{}{}:
		default:
		}
	}

	var outcome atomic.Int32
	outcome.Store(int32(OutcomeChildExit)) // default: the inner stream ended
	setOutcome := func(o Outcome) { outcome.Store(int32(o)) }

	aDone := make(chan struct{})      // inner-output reader fully stopped
	bDone := make(chan struct{})      // input reader fully stopped
	cDone := make(chan struct{})      // response drainer fully stopped
	cQuit := make(chan struct{})      // tells the response drainer to stop
	renderDone := make(chan struct{}) // render loop fully stopped

	// overlayCmd carries overlay show/hide requests from B to the render loop,
	// which owns the screen manager (so the overlay is never mutated off the
	// render goroutine). true = show help, false = dismiss.
	overlayCmd := make(chan bool, 4)

	// A — inner output → emulator. Inner Read EOF/error (session/channel end)
	// tears the session down (outcome stays OutcomeChildExit unless B already
	// set detach/switch/client-gone).
	go func() {
		defer close(aDone)
		defer stop()
		buf := make([]byte, 32*1024)
		for {
			n, err := inner.Read(buf)
			if n > 0 {
				_, _ = emu.Write(buf[:n])
				markDirty()
			}
			if err != nil {
				return
			}
		}
	}()

	// B — client input → inner, through the escape state machine. An escape sets
	// the typed outcome (detach/switch) and tears the session down BEFORE any
	// further bytes reach the inner stream; a plain EOF/error means the user's
	// terminal went away (OutcomeClientGone). A teardown-initiated cancel
	// (ErrCanceled) is none of those — leave the default outcome.
	go func() {
		defer close(bDone)
		esc := newEscapeScanner()
		helpUp := false
		buf := make([]byte, 32*1024)
		for {
			n, rerr := client.Read(buf)
			if n > 0 {
				// While the help overlay is up, the next input dismisses it and is
				// swallowed (not forwarded) — "press any key to dismiss".
				if helpUp {
					helpUp = false
					overlayCmd <- false
					continue
				}
				fwd, ev := esc.feed(buf[:n])
				if len(fwd) > 0 {
					if _, werr := inner.Write(fwd); werr != nil {
						return
					}
				}
				if ev != escNone {
					switch ev {
					case escDetach:
						setOutcome(OutcomeDetach)
						stop()
						return
					case escSwitch:
						setOutcome(OutcomeSwitch)
						stop()
						return
					case escHelp:
						helpUp = true
						overlayCmd <- true
					}
				}
			}
			if rerr != nil {
				if !errors.Is(rerr, cancelreader.ErrCanceled) {
					setOutcome(OutcomeClientGone)
					stop()
				}
				return
			}
		}
	}()

	// C — emulator query responses (DSR/DA/…) → inner. The emulator answers
	// terminal queries the inner stream emits; those replies belong on the
	// session's stdin, not the user's screen. Backed by a synchronous io.Pipe,
	// so this must drain continuously or a reply generated mid-Write in A would
	// deadlock. Stops when teardown closes cQuit and wakes the blocked Read with
	// a throwaway reply (see the teardown note for why we don't Close the emu).
	go func() {
		defer close(cDone)
		buf := make([]byte, 4096)
		innerOpen := true
		for {
			n, rerr := emu.Read(buf)
			if rerr != nil {
				return
			}
			select {
			case <-cQuit:
				return
			default:
			}
			if n > 0 && innerOpen {
				if _, werr := inner.Write(buf[:n]); werr != nil {
					innerOpen = false
				}
			}
		}
	}()

	// D — render loop. Single owner of the screen manager, the inner size, and
	// the emulator's Draw/Resize. Everything that mutates cols/rows lives here so
	// it needs no locking.
	go func() {
		defer close(renderDone)
		mgr.render()
		for {
			select {
			case <-done:
				mgr.restore()
				return
			case sz := <-resize:
				if mgr.applyResize(sz[0], sz[1], inner) {
					mgr.render()
				}
			case show := <-overlayCmd:
				if show {
					mgr.showHelp()
				} else {
					mgr.clearOverlay()
				}
				mgr.render()
			case <-dirty:
				timer := time.NewTimer(frameInterval)
			collect:
				for {
					select {
					case <-done:
						timer.Stop()
						mgr.restore()
						return
					case sz := <-resize:
						mgr.applyResize(sz[0], sz[1], inner)
					case show := <-overlayCmd:
						if show {
							mgr.showHelp()
						} else {
							mgr.clearOverlay()
						}
					case <-dirty:
						// keep coalescing this burst
					case <-timer.C:
						break collect
					}
				}
				mgr.render()
			}
		}
	}()

	<-done
	// Teardown. A is (or was) blocked reading the inner channel; an inner Close
	// unblocks it (the SSH channel Read returns EOF), so unlike the old PTY
	// master we can close it directly. For a child-exit the channel is already
	// gone — Close is then a harmless no-op. For detach/switch/client-gone, Close
	// is the clean detach signal on the wire (the agent preserves the session).
	_ = inner.Close()
	select {
	case <-aDone:
	case <-time.After(2 * time.Second):
		// The inner Read didn't unblock (a wedged transport); proceed anyway —
		// the goroutine is process-bounded once we return.
	}
	// Join the render loop, but bounded: restore() does a blocking client write
	// (scr.Flush), and a wedged terminal must not hang teardown forever.
	select {
	case <-renderDone:
	case <-time.After(2 * time.Second):
	}
	// Stop the input reader so it can't outlive the session (and steal stdin from
	// a subsequent attach). Done after the render loop so it no longer writes to
	// the client. Only join when we can actually unblock it — a nil cancelInput
	// (no cancelable reader available) means B may still be blocked on Read, so
	// joining would hang; accept the (rare) leak instead.
	if cancelInput != nil {
		cancelInput()
		select {
		case <-bDone:
		case <-time.After(2 * time.Second):
		}
	}
	// Stop the response drainer (C). We deliberately do NOT call
	// vt.Emulator.Close: it writes the emulator's `closed` flag without
	// synchronizing against C's in-flight Read of that same flag — a benign
	// one-way latch, but the race detector (rightly) flags it. Instead, tell C
	// to quit and wake its blocked Read with a throwaway DSR query the emulator
	// will answer. A and D have already stopped, so this is the only emulator
	// access left, and the io.Pipe behind Read is internally synchronized.
	close(cQuit)
	_, _ = emu.Write([]byte("\x1b[6n"))
	<-cDone
	return Outcome(outcome.Load())
}

// screenManager is the three-layer screen state, owned exclusively by the render
// loop (no locking). It holds the chrome bar, the inner emulator grid, and the
// (optional) overlay.
type screenManager struct {
	scr   *uv.TerminalScreen
	w     io.Writer // the client; per-frame synchronized-output escapes go here
	emu   *vt.SafeEmulator
	label string
	cols  int
	rows  int

	chromeSS *uv.StyledString

	// overlay, when non-nil, is drawn over the inner region.
	overlay *overlay
}

func newScreenManager(scr *uv.TerminalScreen, w io.Writer, label string, cols, rows int) *screenManager {
	m := &screenManager{
		scr:   scr,
		w:     w,
		emu:   vt.NewSafeEmulator(cols, rows-chromeRows),
		label: label,
		cols:  cols,
		rows:  rows,
	}
	m.rebuildChrome()
	return m
}

func (m *screenManager) rebuildChrome() {
	m.chromeSS = uv.NewStyledString(chromeBar(m.label, m.cols))
}

// render paints all three layers. The chrome fills the top chromeRows; the inner
// emulator fills the rest; an active overlay is drawn last, on top of the inner
// rows. Together they cover every cell each frame, so the screen's diff emits
// only what changed.
func (m *screenManager) render() {
	// Bracket the whole frame (both Render/Flush passes below) in synchronized
	// output. On a 2026-capable terminal everything between applies atomically;
	// elsewhere it's ignored and uv's own cursor hide/show (emitted by Flush
	// because we left sync updates OFF) keeps the cursor invisible during the
	// repaint. See ansiSyncStart and composite()'s NOTE.
	_, _ = io.WriteString(m.w, ansiSyncStart)
	defer func() { _, _ = io.WriteString(m.w, ansiSyncEnd) }()

	m.chromeSS.Draw(m.scr, uv.Rect(0, 0, m.cols, chromeRows))
	m.drawInner()
	if m.overlay != nil {
		m.overlay.draw(m.scr, m.cols, m.rows)
		m.scr.HideCursor() // the overlay owns focus; no inner cursor while it's up
	} else {
		// Position the cursor, but do NOT ShowCursor() here. The cursor is already
		// in uv's "visible" state (set once at startup, re-armed in clearOverlay),
		// so uv's Flush brackets each frame's diff with hide/show on its own.
		// Calling ShowCursor() every frame would queue a stray show ahead of the
		// diff, re-exposing the cursor walk on terminals that ignore mode 2026.
		pos := m.emu.CursorPosition()
		cy := pos.Y + chromeRows
		if cy >= m.rows {
			cy = m.rows - 1
		}
		m.scr.SetCursorPosition(pos.X, cy)
	}
	m.scr.Render()
	_ = m.scr.Flush()
	// uv's TerminalScreen buffers in two stages: Render() flushes cell content to
	// the screen's write buffer, but Flush()'s cursor MoveTo writes into the
	// *renderer's* buffer AFTER that — so the cursor move isn't emitted until the
	// NEXT frame's Render() flushes the renderer buffer. For a cursor-ONLY change
	// (typing a space, or a backspace — no cell content changes) that one-frame lag
	// leaves the visible cursor un-moved until the next keystroke, ending up
	// off-by-one. A second Render()+Flush() flushes the stranded cursor move in
	// THIS frame. It's cheap: when the cursor already sits where the content left
	// it (the common steady-output case) the second pass emits nothing.
	m.scr.Render()
	_ = m.scr.Flush()
}

func (m *screenManager) restore() {
	m.scr.ShowCursor()
	m.scr.ExitAltScreen()
	m.scr.Render()
	_ = m.scr.Flush()
}

// applyResize resizes every layer to the new client window and re-pushes the
// post-chrome size to the inner session. Returns true if the size actually
// changed (so the caller renders).
func (m *screenManager) applyResize(w, h int, inner Inner) bool {
	if w < 1 || h < chromeRows+1 {
		return false
	}
	if w == m.cols && h == m.rows {
		return false
	}
	m.cols, m.rows = w, h
	_ = inner.Resize(m.cols, m.rows-chromeRows)
	m.emu.Resize(m.cols, m.rows-chromeRows)
	m.scr.Resize(m.cols, m.rows)
	m.rebuildChrome()
	return true
}

// drawInner paints the emulator grid into the inner region. It deliberately does
// NOT use vt.Emulator.Draw, which only repaints the emulator's *touched* lines
// (filling the rest of the area with the background color). vt clears the touched
// set on resize, so Emulator.Draw would blank the *retained* grid to a solid
// background-colored block (black for a dark box theme) on every resize after the
// first — and even a one-shot repaint would be re-blanked by the next
// touched-only Draw. Instead we repaint the FULL grid every frame from the cells
// vt already holds; uv's TerminalScreen.Render still diffs to the wire, so the
// emitted bytes stay minimal. This mirrors Emulator.Draw's per-cell logic
// (bg-fill, Clone, wide-cell stride, default fg/bg) minus the touched filter.
func (m *screenManager) drawInner() {
	w, h := m.cols, m.rows-chromeRows
	// Blank fill with NO explicit bg/fg, so empty cells take the user's own
	// terminal theme. (Deliberately NOT emu.BackgroundColor(): vt's defaultBg is
	// color.Black, so filling with it paints empty rows solid black instead of
	// letting the outer terminal show through — the "black band after resize".)
	// Cells the box actually styled carry their own colors via CellAt below.
	blank := uv.EmptyCell
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.scr.SetCell(x, chromeRows+y, &blank)
		}
	}
	// Copy every cell the emulator holds, verbatim — no fg/bg defaulting. A cell
	// the box styled (incl. a full-screen app's own background) keeps its colors;
	// an unstyled/blank cell stays bg/fg-nil so the outer terminal theme shows.
	for y := 0; y < h; y++ {
		for x := 0; x < w; {
			stride := 1
			if cell := m.emu.CellAt(x, y); cell != nil {
				cell = cell.Clone()
				if cell.Width > 1 {
					stride = cell.Width
				}
				m.scr.SetCell(x, chromeRows+y, cell)
			}
			x += stride
		}
	}
}

func (m *screenManager) showHelp() {
	m.overlay = helpOverlay()
}

func (m *screenManager) clearOverlay() {
	m.overlay = nil
	// The overlay branch of render() hid the cursor (uv HideCursor sets its
	// "hidden" flag); re-arm it so render() resumes positioning the inner cursor
	// and uv's Flush resumes its hide/show-around-diff wrapping.
	m.scr.ShowCursor()
}

// sanitizeLabel removes ESC and C0/C1 control bytes so a devbox id can't inject
// SGR (or any escape) into the chrome bar when it's parsed by uv.NewStyledString.
func sanitizeLabel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) { // C0 (incl. ESC), DEL, C1
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
