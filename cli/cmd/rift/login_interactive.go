package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/muesli/cancelreader"
	"golang.org/x/term"

	"github.com/fixed-labs/oss/cli/internal/client"
)

// errLoginCanceled is the sentinel returned when the user presses q/Ctrl-C
// during the interactive poll. main's `fmt.Fprintf(os.Stderr, "rift: %v\n")`
// renders it as "rift: login canceled" (exit 1); it is deliberately NOT wrapped
// so cmdLogin can detect it with errors.Is and return it unadorned.
var errLoginCanceled = errors.New("login canceled")

// pollInteractive runs the device-flow long-poll on the main goroutine while a
// background goroutine reads keys (c to copy the URL, q/Ctrl-C to cancel). It is
// called only when stdin AND stdout are a TTY, so it may assume interactivity.
//
// The long-poll is the source of truth and always runs to completion or
// cancellation; the key affordance is a strictly best-effort convenience. If raw
// mode or a cancelable reader is unavailable, it degrades to the plain
// PollUntilToken path — the printed URL remains the guaranteed way to log in.
func pollInteractive(pollCtx context.Context, cancel context.CancelFunc,
	c *client.Client, start *client.DeviceStart, url string) (*client.DeviceToken, error) {

	inFd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(inFd)
	if err != nil { // raw mode unavailable → degrade
		return c.PollUntilToken(pollCtx, start)
	}
	defer term.Restore(inFd, old)

	cr, err := cancelreader.NewReader(os.Stdin)
	if err != nil { // cancelable stdin unavailable → degrade
		return c.PollUntilToken(pollCtx, start) // deferred Restore still runs
	}
	defer cr.Close()

	fmt.Print("  Press c to copy the URL · q to cancel\r\n") // hint ONLY after raw mode is live

	var userCanceled bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		userCanceled = keyLoop(cr, os.Stdout, url, cancel) // `cancel` is pollCtx's CancelFunc
	}()

	tok, err := c.PollUntilToken(pollCtx, start) // poll call unchanged, on the MAIN goroutine
	cr.Cancel()                                  // unblock the reader's Read
	wg.Wait()                                    // join BEFORE the deferred Restore; also
	//                                              publishes userCanceled to this goroutine
	if userCanceled {
		return nil, errLoginCanceled
	}
	return tok, err
}

// keyLoop reads one byte at a time until Read errors (Cancel/EOF) or the user
// quits. Returns true iff the user pressed q/Ctrl-C. Testable off a real TTY:
// driven with a bytes.Reader, a bytes.Buffer, and a stub cancel func.
//
// Every write here happens after MakeRaw, so it uses \r\n: raw mode disables
// ONLCR, and a bare \n would stair-step.
func keyLoop(in io.Reader, out io.Writer, url string, cancel context.CancelFunc) bool {
	buf := make([]byte, 1)
	for {
		if _, e := in.Read(buf); e != nil {
			return false // unblocked by cr.Cancel(), or stdin closed
		}
		switch buf[0] {
		case 'c', 'C':
			writeClipboard(out, url)
			fmt.Fprint(out, "  Copied the URL to your clipboard\r\n")
		case 'q', 'Q', 0x03: // 0x03 = Ctrl-C (MakeRaw disabled ISIG)
			fmt.Fprint(out, "  Canceling…\r\n")
			cancel()
			return true
		}
	}
}

// openBrowser opens url in the platform's default browser, best-effort. It is a
// package-level var so tests can swap it. It Start()s but never Wait()s — a
// missing binary surfaces as a Start error, which the caller logs and ignores.
var openBrowser = func(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default: // linux and other unix
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start() // do NOT Wait; a missing binary surfaces as a Start error
}

// shouldAutoOpen decides whether to auto-open the browser. Pure and default-on,
// it suppresses on any doubt: not interactive, --no-browser, any SSH_* var set
// (remote — a browser would open on the wrong host), or headless linux (no
// DISPLAY/WAYLAND_DISPLAY for xdg-open to target). A wrong suppression is
// harmless (the URL and c remain); a wrong fire could launch a browser remotely.
func shouldAutoOpen(env func(string) string, interactive, noBrowser bool) bool {
	if !interactive || noBrowser {
		return false
	}
	if env("SSH_CONNECTION") != "" || env("SSH_TTY") != "" || env("SSH_CLIENT") != "" {
		return false // remote: a browser here would open on the wrong (remote) host
	}
	if runtime.GOOS == "linux" && env("DISPLAY") == "" && env("WAYLAND_DISPLAY") == "" {
		return false // headless linux: nothing for xdg-open to target
	}
	return true
}

// writeClipboard copies s to the terminal's clipboard via the OSC 52 escape,
// which reaches the LOCAL (laptop) clipboard even from a remote process over
// SSH. It is a package-level var so tests can swap it. Fire-and-forget: OSC 52
// has no acknowledgement, and terminals without OSC 52 write support silently
// no-op — the printed URL is the fallback.
var writeClipboard = func(w io.Writer, s string) {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\x07"
	if os.Getenv("TMUX") != "" {
		// tmux DCS passthrough: wrap, doubling every ESC inside.
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	io.WriteString(w, seq)
}
