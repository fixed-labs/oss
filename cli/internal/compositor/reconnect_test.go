//go:build linux

package compositor

import (
	"bytes"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

// TestReconnectScreenRendersCard asserts the disconnect overlay paints the chrome
// header and a centered "Disconnected" card carrying the status line and the
// cancel hint. (No Fd on a bytes.Buffer, so colorprofile is no-color — we assert
// on the text, which survives.)
func TestReconnectScreenRendersCard(t *testing.T) {
	var buf bytes.Buffer
	rs := newReconnectScreen(&buf, uv.Environ{"TERM=xterm-256color"}, "rift: ws-test / s1 ", 80, 24)
	rs.setStatus("Will reconnect automatically… (attempt 2)")
	rs.render()

	out := buf.String()
	for _, want := range []string{
		"rift: ws-test / s1", // chrome header
		"Disconnected",       // card title
		"attempt 2",          // status line
		"Press Ctrl-C to cancel",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("reconnect frame missing %q;\nframe=%q", want, out)
		}
	}
}

// TestReconnectScreenResizeRedrawsCard asserts the card is repainted after a
// resize (it stays centered as the terminal changes size).
func TestReconnectScreenResizeRedrawsCard(t *testing.T) {
	var buf bytes.Buffer
	rs := newReconnectScreen(&buf, uv.Environ{"TERM=xterm-256color"}, "ws ", 80, 24)
	rs.render()

	rs.resize(50, 16)
	buf.Reset()
	rs.render()
	if !strings.Contains(buf.String(), "Disconnected") {
		t.Fatalf("card not redrawn after resize;\nframe=%q", buf.String())
	}
}

// TestReconnectScreenDefaultStatus asserts an empty status falls back to the
// default headline rather than rendering a blank line.
func TestReconnectScreenDefaultStatus(t *testing.T) {
	var buf bytes.Buffer
	rs := newReconnectScreen(&buf, uv.Environ{"TERM=xterm-256color"}, "ws ", 80, 24)
	rs.setStatus("")
	rs.render()
	if !strings.Contains(buf.String(), "Will reconnect automatically") {
		t.Fatalf("empty status did not fall back to the default headline;\nframe=%q", buf.String())
	}
}

func TestContainsAbort(t *testing.T) {
	if !containsAbort([]byte{0x03}) {
		t.Fatal("Ctrl-C (ETX) not detected as abort")
	}
	if !containsAbort([]byte("ab\x04c")) {
		t.Fatal("Ctrl-D (EOT) not detected as abort")
	}
	if containsAbort([]byte("hello world")) {
		t.Fatal("plain text flagged as abort")
	}
	if containsAbort(nil) {
		t.Fatal("empty input flagged as abort")
	}
}
