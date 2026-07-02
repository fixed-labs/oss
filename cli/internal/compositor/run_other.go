//go:build !unix

// Package compositor on non-unix platforms is a passthrough: the chrome
// compositor needs a unix pty and SIGWINCH, so there we just bridge the remote
// session to the real stdio with no chrome. The Outcome is OutcomeChildExit on
// inner EOF, OutcomeClientGone on stdin EOF — detach/switch are unavailable
// without the escape state machine (which lives in the unix build).
package compositor

import (
	"fmt"
	"io"
	"os"
)

// Outcome mirrors the unix build's typed result.
type Outcome int

const (
	OutcomeChildExit Outcome = iota
	OutcomeClientGone
	OutcomeDetach
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

// Inner mirrors the unix build's interface.
type Inner interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

// Label formats the chrome text: workspace id and session id.
func Label(workspaceID, sessionID string) string {
	return "rift: " + workspaceID + " / " + sessionID + " "
}

// InitialSize reports a default size and tty=false (no compositing here).
func InitialSize() (cols, rows int, tty bool) { return 80, 24, false }

// PostChromeSize is identity on non-unix (no chrome reserved).
func PostChromeSize(cols, rows int) (int, int) { return cols, rows }

// PickItem / PickResult mirror the unix build's picker types so the connect flow
// compiles cross-platform.
type PickItem struct {
	ID    string
	Label string
}

type PickResult struct {
	Selected string
	New      bool
	Aborted  bool
}

// RunPickerTTY on non-unix has no interactive picker — default to the first
// item (or "new" when empty). The unix build renders the real picker.
func RunPickerTTY(items []PickItem, header string) (PickResult, error) {
	_ = header
	if len(items) > 0 {
		return PickResult{Selected: items[0].ID}, nil
	}
	return PickResult{New: true}, nil
}

// FormatPickLabel mirrors the unix build's helper.
func FormatPickLabel(name, cmd, cwd string, attached int) string {
	_ = cmd
	_ = cwd
	_ = attached
	return name
}

// Reconnecting mirrors the unix overlay's surface. With no compositor on non-unix,
// it degrades to a stderr line per status update.
type Reconnecting struct{}

// ShowReconnecting on non-unix has no terminal overlay; status updates print to
// stderr. onAbort is unused — Ctrl-C still raises SIGINT in the cooked terminal.
func ShowReconnecting(label string, onAbort func()) *Reconnecting {
	_, _ = label, onAbort
	return &Reconnecting{}
}

// SetStatus prints the disconnect status to stderr.
func (*Reconnecting) SetStatus(s string) {
	fmt.Fprintf(os.Stderr, "rift: disconnected — %s\n", s)
}

// Close is a no-op on non-unix.
func (*Reconnecting) Close() {}

// Run bridges inner ⇄ real stdio with no chrome.
func Run(inner Inner, label string) Outcome {
	_ = label
	done := make(chan Outcome, 2)
	go func() {
		_, _ = io.Copy(os.Stdout, inner)
		done <- OutcomeChildExit
	}()
	go func() {
		_, _ = io.Copy(inner, os.Stdin)
		done <- OutcomeClientGone
	}()
	o := <-done
	_ = inner.Close()
	return o
}
