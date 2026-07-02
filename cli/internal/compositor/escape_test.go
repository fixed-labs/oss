//go:build unix

package compositor

import (
	"bytes"
	"testing"
)

// feedAll runs the scanner over in (optionally split across multiple feed calls
// to exercise cross-read state) and returns the concatenated forwarded bytes and
// the LAST event seen.
func feedAll(s *escapeScanner, chunks ...[]byte) ([]byte, escEvent) {
	var out []byte
	ev := escNone
	for _, c := range chunks {
		fwd, e := s.feed(c)
		out = append(out, fwd...)
		if e != escNone {
			ev = e
		}
	}
	return out, ev
}

func TestEscapeLineStartDetach(t *testing.T) {
	s := newEscapeScanner()
	// First byte of the connection is a line start, so ~d there detaches.
	out, ev := feedAll(s, []byte("~d"))
	if ev != escDetach {
		t.Fatalf("want escDetach, got %v", ev)
	}
	if len(out) != 0 {
		t.Fatalf("detach must forward nothing, got %q", out)
	}
}

func TestEscapeDetachAfterNewline(t *testing.T) {
	s := newEscapeScanner()
	out, ev := feedAll(s, []byte("echo hi\r~."))
	if ev != escDetach {
		t.Fatalf("want escDetach after newline, got %v", ev)
	}
	if string(out) != "echo hi\r" {
		t.Fatalf("forwarded %q, want the pre-escape line", out)
	}
}

func TestEscapeNotAtLineStartIsLiteral(t *testing.T) {
	s := newEscapeScanner()
	// A '~' mid-line (e.g. a path) is forwarded untouched; the following 'd' too.
	out, ev := feedAll(s, []byte("cd ~docs"))
	if ev != escNone {
		t.Fatalf("mid-line ~ must not arm an escape, got %v", ev)
	}
	if string(out) != "cd ~docs" {
		t.Fatalf("forwarded %q, want verbatim", out)
	}
}

func TestEscapeTildeTildeLiteral(t *testing.T) {
	s := newEscapeScanner()
	out, ev := feedAll(s, []byte("~~"))
	if ev != escNone {
		t.Fatalf("~~ must produce no event, got %v", ev)
	}
	if string(out) != "~" {
		t.Fatalf("~~ must forward a single ~, got %q", out)
	}
	// After ~~, we're mid-line; a following ~d is literal.
	out, ev = feedAll(s, []byte("~d"))
	if ev != escNone || string(out) != "~d" {
		t.Fatalf("after ~~ the line is disarmed: out=%q ev=%v", out, ev)
	}
}

func TestEscapeSwitchAndHelp(t *testing.T) {
	s := newEscapeScanner()
	if _, ev := feedAll(s, []byte("~s")); ev != escSwitch {
		t.Fatalf("~s want escSwitch, got %v", ev)
	}
	s = newEscapeScanner()
	out, ev := feedAll(s, []byte("~?"))
	if ev != escHelp {
		t.Fatalf("~? want escHelp, got %v", ev)
	}
	if len(out) != 0 {
		t.Fatalf("~? forwards nothing, got %q", out)
	}
}

func TestEscapeUnknownCommandForwardsBoth(t *testing.T) {
	s := newEscapeScanner()
	// ~x at line start: not a known command → forward both bytes (ssh behavior).
	out, ev := feedAll(s, []byte("~x"))
	if ev != escNone {
		t.Fatalf("unknown command want no event, got %v", ev)
	}
	if string(out) != "~x" {
		t.Fatalf("unknown ~x must forward both bytes, got %q", out)
	}
}

func TestEscapeSplitAcrossReads(t *testing.T) {
	// The '~' arrives in one read, the command byte in the next — the pending
	// state must persist across feed calls.
	s := newEscapeScanner()
	out1, ev1 := s.feed([]byte("~"))
	if ev1 != escNone || len(out1) != 0 {
		t.Fatalf("first read: pending ~ should forward nothing yet, out=%q ev=%v", out1, ev1)
	}
	_, ev2 := s.feed([]byte("d"))
	if ev2 != escDetach {
		t.Fatalf("second read: want escDetach, got %v", ev2)
	}
}

func TestEscapeSuppressedInBracketedPaste(t *testing.T) {
	s := newEscapeScanner()
	// A paste whose content begins (after a newline) with ~d must NOT detach.
	const start = "\x1b[200~"
	const end = "\x1b[201~"
	payload := "line1\n~d still pasted"
	out, ev := feedAll(s, []byte(start+payload+end))
	if ev != escNone {
		t.Fatalf("escapes must be suppressed inside a paste, got %v", ev)
	}
	want := start + payload + end
	if string(out) != want {
		t.Fatalf("paste must be forwarded verbatim:\n got %q\nwant %q", out, want)
	}
}

func TestEscapeArmedAfterPasteEnd(t *testing.T) {
	s := newEscapeScanner()
	const start = "\x1b[200~"
	const end = "\x1b[201~"
	// Paste ends mid-line (no trailing newline), so the next ~d is literal.
	_, _ = feedAll(s, []byte(start+"x"+end))
	out, ev := feedAll(s, []byte("~d"))
	if ev != escNone || string(out) != "~d" {
		t.Fatalf("after a paste ending mid-line, ~d is literal: out=%q ev=%v", out, ev)
	}
	// But a paste containing a trailing newline re-arms line start.
	s = newEscapeScanner()
	_, _ = feedAll(s, []byte(start+"x\n"+end))
	out, ev = feedAll(s, []byte("~d"))
	if ev != escDetach {
		t.Fatalf("after a paste ending in newline, ~d at line start detaches: out=%q ev=%v", out, ev)
	}
}

func TestMouseSGRRowOffset(t *testing.T) {
	s := newEscapeScanner()
	// SGR mouse press at row 10 → forwarded with row 10-chromeRows.
	in := []byte("\x1b[<0;5;10M")
	out, _ := feedAll(s, in)
	want := []byte("\x1b[<0;5;" + itoa(10-chromeRows) + "M")
	if !bytes.Equal(out, want) {
		t.Fatalf("SGR mouse row not offset:\n got %q\nwant %q", out, want)
	}
}

func TestMouseSGRRowClampedToInner(t *testing.T) {
	s := newEscapeScanner()
	// A click in the chrome row (row 1, with chromeRows=1) → clamped to inner
	// row 1, not 0 or negative.
	in := []byte("\x1b[<0;3;1m")
	out, _ := feedAll(s, in)
	want := []byte("\x1b[<0;3;1m")
	if !bytes.Equal(out, want) {
		t.Fatalf("chrome-row click not clamped:\n got %q\nwant %q", out, want)
	}
}

// TestMouseSGRWithinOneRead: a mouse sequence arrives atomically in one kernel
// read (the realistic case), and is rewritten.
func TestMouseSGRWithinOneRead(t *testing.T) {
	s := newEscapeScanner()
	out, _ := feedAll(s, []byte("\x1b[<0;5;12M"))
	want := []byte("\x1b[<0;5;" + itoa(12-chromeRows) + "M")
	if !bytes.Equal(out, want) {
		t.Fatalf("SGR mouse not handled:\n got %q\nwant %q", out, want)
	}
}

// TestLoneEscapeFlushedAtChunkEnd: a bare ESC (the Escape key) is forwarded at
// the end of the read that produced it, not held until the next byte.
func TestLoneEscapeFlushedAtChunkEnd(t *testing.T) {
	s := newEscapeScanner()
	out, _ := feedAll(s, []byte("\x1b"))
	if !bytes.Equal(out, []byte("\x1b")) {
		t.Fatalf("lone ESC not flushed at chunk end: got %q", out)
	}
}

// TestAltChordForwarded: ESC followed by a letter (Alt-x) is forwarded verbatim.
func TestAltChordForwarded(t *testing.T) {
	s := newEscapeScanner()
	out, _ := feedAll(s, []byte("\x1bx"))
	if !bytes.Equal(out, []byte("\x1bx")) {
		t.Fatalf("Alt-chord not forwarded: got %q", out)
	}
}

func TestMouseX10RowOffset(t *testing.T) {
	s := newEscapeScanner()
	// X10 mouse: ESC[M b cx cy with cy = row+32. Row 10 → byte 32+10.
	in := []byte{0x1b, '[', 'M', 32 + 0, 32 + 5, 32 + 10}
	out, _ := feedAll(s, in)
	want := []byte{0x1b, '[', 'M', 32 + 0, 32 + 5, byte(32 + 10 - chromeRows)}
	if !bytes.Equal(out, want) {
		t.Fatalf("X10 mouse row not offset:\n got %v\nwant %v", out, want)
	}
}

func TestNonMouseCSIPassthrough(t *testing.T) {
	s := newEscapeScanner()
	// An arrow key (ESC[A) and a colored SGR must pass through untouched.
	in := []byte("\x1b[A\x1b[1;31mhi")
	out, ev := feedAll(s, in)
	if ev != escNone {
		t.Fatalf("CSI passthrough must not arm escapes, got %v", ev)
	}
	if string(out) != string(in) {
		t.Fatalf("non-mouse CSI not passed through:\n got %q\nwant %q", out, in)
	}
}

func TestEscapeAfterCRLF(t *testing.T) {
	s := newEscapeScanner()
	// CRLF then ~d: still a line start.
	if _, ev := feedAll(s, []byte("x\r\n~d")); ev != escDetach {
		t.Fatalf("~d after CRLF must detach, got %v", ev)
	}
}

// itoa is strconv.Itoa without importing strconv into the test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
