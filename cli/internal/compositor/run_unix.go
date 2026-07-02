//go:build unix

package compositor

import (
	"io"
	"os"
	"os/signal"
	"syscall"

	uv "github.com/charmbracelet/ultraviolet"
	xterm "github.com/charmbracelet/x/term"
	"github.com/muesli/cancelreader"
	"golang.org/x/term"
)

// clientIO adapts the process's stdin/stdout into the single io.ReadWriter the
// compositor uses as "the client" (the user's real terminal). It implements
// charmbracelet's term.File (ReadWriteCloser + Fd) so ultraviolet's color-profile
// detection sees a real TTY — without Fd, colorprofile.Detect can't confirm a
// terminal and falls back to a no-color profile, which silently strips the
// chrome's background color (attributes like reverse video survive, colors don't).
// Fd reports the OUTPUT descriptor, since that's the surface being styled; in is
// the (cancelable) input reader so the compositor's input goroutine can be
// unblocked at teardown.
type clientIO struct{ in io.Reader }

var _ xterm.File = clientIO{}

func (c clientIO) Read(p []byte) (int, error) { return c.in.Read(p) }
func (clientIO) Write(p []byte) (int, error)  { return os.Stdout.Write(p) }
func (clientIO) Close() error                 { return nil } // we don't own the real stdio
func (clientIO) Fd() uintptr                  { return os.Stdout.Fd() }

// InitialSize returns the current terminal's (cols, rows) and whether stdin and
// stdout are both real terminals (so the caller knows whether to drive the
// compositor at all). On a non-tty it returns a sane default size.
func InitialSize() (cols, rows int, tty bool) {
	cols, rows = 80, 24
	inFd := int(os.Stdin.Fd())
	outFd := int(os.Stdout.Fd())
	if !term.IsTerminal(inFd) || !term.IsTerminal(outFd) || Disabled() {
		return cols, rows, false
	}
	if w, h, err := term.GetSize(inFd); err == nil && w > 0 && h > 0 {
		cols, rows = w, h
	}
	return cols, rows, true
}

// PostChromeSize returns the inner (post-chrome) size the compositor would
// report as the SSH window size for the given full-terminal size. The CLI uses
// this to send the initial pty-req Cols/Rows to the agent before the attach
// frame.
func PostChromeSize(cols, rows int) (int, int) {
	if rows < chromeRows+1 {
		rows = chromeRows + 1
	}
	return cols, rows - chromeRows
}

// Run drives one session (inner) under the chrome compositor, compositing onto
// the current terminal, and returns the typed Outcome. label is the chrome text
// (compositor.Label(id)).
//
// It falls back to a plain attached bridge — inner I/O copied raw to/from the
// real stdio with no chrome — when the compositor can't run (chrome disabled,
// stdin/stdout not a terminal, raw mode unavailable). In that fallback the
// escape state machine and chrome are absent, so the session ends only on inner
// EOF or stdin EOF (OutcomeChildExit / OutcomeClientGone); detach/switch are
// unavailable. The compositor is purely additive and never breaks a working
// connect.
func Run(inner Inner, label string) Outcome {
	cols, rows, tty := InitialSize()
	if !tty {
		return runAttached(inner)
	}

	inFd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(inFd)
	if err != nil {
		return runAttached(inner)
	}
	defer func() { _ = term.Restore(inFd, old) }()

	// Cancelable stdin so the input reader can be unblocked at teardown and not
	// linger on os.Stdin into a subsequent session. If it can't be created, run
	// without cancellation (the compositor then won't join the input reader).
	var input io.Reader = os.Stdin
	var cancelInput func()
	if cr, crErr := cancelreader.NewReader(os.Stdin); crErr == nil {
		input = cr
		cancelInput = func() { cr.Cancel() }
		defer cr.Close()
	}

	// Mirror the real terminal's window-change events into the compositor. The
	// watcher exits via winchDone so it doesn't leak past the session.
	resize := make(chan [2]int, 1)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	winchDone := make(chan struct{})
	defer func() { signal.Stop(winch); close(winchDone) }()
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

	// Pass the full environment (not just TERM) so colorprofile honors COLORTERM
	// (truecolor), NO_COLOR, CLICOLOR_FORCE, and terminfo.
	env := uv.Environ(os.Environ())
	return composite(clientIO{in: input}, inner, resize, cols, rows, env, label, cancelInput)
}

// RunPickerTTY runs the interactive session picker on the real terminal,
// entering raw mode for the duration so arrow keys / Enter are read unbuffered.
// It restores the terminal and clears the picker before returning. Used by the
// connect flow for the >1-session case and the in-session `~s` switcher (which
// runs between compositor sessions).
func RunPickerTTY(items []PickItem, header string) (PickResult, error) {
	inFd := int(os.Stdin.Fd())
	if !term.IsTerminal(inFd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		// No TTY — caller handles the non-interactive default.
		if len(items) > 0 {
			return PickResult{Selected: items[0].ID}, nil
		}
		return PickResult{New: true}, nil
	}
	old, err := term.MakeRaw(inFd)
	if err != nil {
		if len(items) > 0 {
			return PickResult{Selected: items[0].ID}, nil
		}
		return PickResult{New: true}, nil
	}
	defer func() {
		_ = term.Restore(inFd, old)
		_, _ = os.Stdout.WriteString("\x1b[2J\x1b[H") // clear the picker
	}()
	return RunPicker(os.Stdin, os.Stdout, items, header)
}

// runAttached is the no-chrome fallback: copy inner ⇄ real stdio raw, with no
// emulator, chrome, or escapes. Ends on the first EOF in either direction.
func runAttached(inner Inner) Outcome {
	done := make(chan Outcome, 2)
	go func() {
		_, _ = io.Copy(os.Stdout, inner) // inner → screen
		done <- OutcomeChildExit
	}()
	go func() {
		_, _ = io.Copy(inner, os.Stdin) // keystrokes → inner
		done <- OutcomeClientGone
	}()
	o := <-done
	_ = inner.Close()
	return o
}

// Disabled reports whether the chrome compositor is turned off via env.
func Disabled() bool {
	switch os.Getenv("RIFT_NO_BANNER") {
	case "1", "true", "yes":
		return true
	}
	switch os.Getenv("RIFT_NO_CHROME") {
	case "1", "true", "yes":
		return true
	}
	return false
}
