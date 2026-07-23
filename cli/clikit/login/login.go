// Package login is the shared device-flow login UI + URL-resolution guard for
// the fixed-labs CLIs: the FIX-246 4-source URL precedence, browser auto-open,
// OSC-52 clipboard copy, and the raw-mode keyloop. Each CLI keeps its own thin
// cmdLogin wiring these with its own flag names and wording, so the audited
// output strings stay per-CLI. Depends on golang.org/x/term +
// muesli/cancelreader — the only dep-carrying clikit package.
package login

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

	"github.com/fixed-labs/oss/cli/clikit/deviceflow"
	"github.com/fixed-labs/oss/cli/clikit/httpx"
)

// ErrNoURLForEnv is the non-prod guard sentinel: a named session with no API
// URL from any source. It carries NO user-facing wording — each CLI's cmdLogin
// detects it via errors.Is and renders its own message (naming the tool).
var ErrNoURLForEnv = errors.New("no API URL saved for env")

// ResolveURL implements the FIX-246 URL precedence: the typed --api flag
// (flagVal) → the override var (envURL) → the active env's saved-config URL
// (savedURL) → defaultURL, the last ONLY when env == "prod". A non-prod env
// with none of the first three returns ErrNoURLForEnv (the caller renders the
// wording). url == "" iff err != nil; fromOverrideVar is true when envURL won
// (the one case the success line must disclose — the only path that can seed a
// wrong-plane URL+token into a non-prod profile while the guard is satisfied).
func ResolveURL(flagVal, envURL, savedURL, env, defaultURL string) (url string, fromOverrideVar bool, err error) {
	if flagVal != "" {
		return flagVal, false, nil
	}
	if envURL != "" {
		return envURL, true, nil
	}
	if savedURL != "" {
		return savedURL, false, nil
	}
	if env != "prod" {
		return "", false, ErrNoURLForEnv
	}
	return defaultURL, false, nil
}

// ErrLoginCanceled is the sentinel returned when the user presses q/Ctrl-C
// during the interactive poll. It is deliberately NOT wrapped so a caller can
// detect it with errors.Is and render its own "login canceled" line.
var ErrLoginCanceled = errors.New("login canceled")

// PollInteractive runs the device-flow long-poll on the main goroutine while a
// background goroutine reads keys (c to copy the URL, q/Ctrl-C to cancel). Call
// only when stdin AND stdout are a TTY. The long-poll is the source of truth;
// the key affordance is best-effort — if raw mode or a cancelable reader is
// unavailable it degrades to a plain PollUntilToken.
func PollInteractive(pollCtx context.Context, cancel context.CancelFunc,
	c *httpx.Client, start *deviceflow.DeviceStart, url string) (*deviceflow.DeviceToken, error) {

	inFd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(inFd)
	if err != nil { // raw mode unavailable → degrade
		return deviceflow.PollUntilToken(pollCtx, c, start)
	}
	defer term.Restore(inFd, old)

	cr, err := cancelreader.NewReader(os.Stdin)
	if err != nil { // cancelable stdin unavailable → degrade
		return deviceflow.PollUntilToken(pollCtx, c, start)
	}
	defer cr.Close()

	fmt.Print("  Press c to copy the URL · q to cancel\r\n") // hint ONLY after raw mode is live

	var userCanceled bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		userCanceled = keyLoop(cr, os.Stdout, url, cancel)
	}()

	tok, err := deviceflow.PollUntilToken(pollCtx, c, start)
	cr.Cancel()
	wg.Wait()
	if userCanceled {
		return nil, ErrLoginCanceled
	}
	return tok, err
}

// keyLoop reads one byte at a time until Read errors (Cancel/EOF) or the user
// quits. Returns true iff the user pressed q/Ctrl-C. Every write here happens
// after MakeRaw, so it uses \r\n (raw mode disables ONLCR).
func keyLoop(in io.Reader, out io.Writer, url string, cancel context.CancelFunc) bool {
	buf := make([]byte, 1)
	for {
		if _, e := in.Read(buf); e != nil {
			return false // unblocked by cr.Cancel(), or stdin closed
		}
		switch buf[0] {
		case 'c', 'C':
			WriteClipboard(out, url)
			fmt.Fprint(out, "  Copied the URL to your clipboard\r\n")
		case 'q', 'Q', 0x03: // 0x03 = Ctrl-C (MakeRaw disabled ISIG)
			fmt.Fprint(out, "  Canceling…\r\n")
			cancel()
			return true
		}
	}
}

// OpenBrowser opens url in the platform's default browser, best-effort. A
// package var so tests can swap it. It Start()s but never Wait()s.
var OpenBrowser = func(url string) error {
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

// ShouldAutoOpen decides whether to auto-open the browser. Pure and default-on,
// it suppresses on any doubt: not interactive, --no-browser, any SSH_* var set
// (remote), or headless linux (no DISPLAY/WAYLAND_DISPLAY).
func ShouldAutoOpen(getenv func(string) string, interactive, noBrowser bool) bool {
	if !interactive || noBrowser {
		return false
	}
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" || getenv("SSH_CLIENT") != "" {
		return false // remote: a browser here would open on the wrong host
	}
	if runtime.GOOS == "linux" && getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
		return false // headless linux: nothing for xdg-open to target
	}
	return true
}

// WriteClipboard copies s to the terminal's clipboard via the OSC 52 escape,
// which reaches the LOCAL clipboard even over SSH. A package var so tests can
// swap it. Fire-and-forget; terminals without OSC 52 write support silently no-op.
var WriteClipboard = func(w io.Writer, s string) {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\x07"
	if os.Getenv("TMUX") != "" {
		// tmux DCS passthrough: wrap, doubling every ESC inside.
		seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	io.WriteString(w, seq)
}
