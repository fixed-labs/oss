//go:build unix

package compositor

import (
	"bytes"
	"strconv"
	"strings"
)

// Client-side escapes (OUT-OF-BAND): session control is handled
// on the laptop, never in the terminal byte stream and never bound to an in-shell
// key. The escape is the prefix byte '~' at LINE START, followed by a command
// byte:
//
//	~d, ~.  detach   (leave the session running on the box)
//	~s      switch   (overlay switcher; re-select another session)
//	~?      help     (overlay help)
//	~~      literal  (forward a single '~')
//
// "Line start" means the previous byte the client sent was '\r' or '\n', or it
// is the very first byte of the connection. Any other byte disarms the escape
// until the next newline — so a '~' typed mid-line (e.g. a path) is forwarded
// untouched, matching ssh/OpenSSH's own escape discipline.
//
// Escapes are suppressed inside a bracketed paste (ESC[200~ … ESC[201~): pasted
// text must reach the shell verbatim, even a leading '~' after a newline.
//
// The scanner ALSO fixes the mouse-coordinate offset: when an inner app turns on
// mouse reporting, the user's terminal reports absolute screen rows, but the
// inner grid starts at row chromeRows — so reported rows are off by chromeRows.
// We parse SGR (ESC[<…M/m) and X10 (ESC[M) mouse sequences and subtract
// chromeRows from the row before forwarding. (Out-of-range rows — a click in the
// chrome — are clamped to the inner grid's first row.)

// escEvent is a control intent the scanner detected from the input stream.
type escEvent int

const (
	escNone escEvent = iota
	escDetach
	escSwitch
	escHelp
)

// escState is the line-start escape machine's state.
type escState int

const (
	stLineStart  escState = iota // armed: a '~' here begins an escape
	stMidLine                    // disarmed until the next newline
	stAfterTilde                 // saw '~' at line start; awaiting the command byte
)

// escapeScanner consumes client input bytes and produces (forward, event):
// the bytes to forward to the inner session and an optional control event. It
// also rewrites mouse-report rows by chromeRows.
type escapeScanner struct {
	st       escState
	paste    bool   // inside a bracketed paste (escapes suppressed)
	pasteBuf []byte // partial ESC[200~/201~ match across reads (and partial mouse)
	out      []byte // scratch forward buffer, reused across feed calls
}

func newEscapeScanner() *escapeScanner {
	return &escapeScanner{st: stLineStart}
}

// feed processes one read of client input. It returns the bytes to forward to
// the inner session (a slice valid until the next feed call) and the LAST
// control event seen in this chunk (escNone if none). A detected detach/switch
// event stops forwarding the rest of the chunk — the caller tears down.
func (s *escapeScanner) feed(in []byte) ([]byte, escEvent) {
	s.out = s.out[:0]
	ev := escNone
	for i := 0; i < len(in); i++ {
		b := in[i]

		// Inside a bracketed paste: forward everything verbatim, only watching
		// for the END marker. A leading '~' in pasted text is NOT an escape.
		if s.paste {
			s.out = append(s.out, b)
			s.trackPasteEnd(b)
			continue
		}

		switch s.st {
		case stAfterTilde:
			// We swallowed a line-start '~'; this byte is the command.
			switch b {
			case 'd', '.':
				s.st = stMidLine
				return s.out, escDetach
			case 's':
				s.st = stMidLine
				return s.out, escSwitch
			case '?':
				s.st = stMidLine
				ev = escHelp
			case '~':
				// ~~ → forward a single literal '~'.
				s.out = append(s.out, '~')
				s.st = stMidLine
			default:
				// Unrecognized: ssh forwards both the '~' and the byte. Match that
				// so an unknown sequence isn't silently eaten.
				s.out = append(s.out, '~')
				s.out = append(s.out, b)
				s.st = atLineStartAfter(b)
			}
			continue

		case stLineStart:
			if b == '~' {
				// Armed: hold the '~' pending the command byte. Don't forward yet.
				s.st = stAfterTilde
				continue
			}
			s.forward(b)
			s.st = atLineStartAfter(b)

		case stMidLine:
			s.forward(b)
			s.st = atLineStartAfter(b)
		}
	}
	// Flush any incomplete CSI capture at the chunk boundary. Terminals emit a
	// mouse/paste sequence atomically within a single kernel read, so an
	// unfinished capture here is NOT a split sequence — it's a lone ESC (the
	// Escape key, or an Alt-chord) or a sequence we don't rewrite. Holding it
	// would stall a bare Escape keypress until the next byte; flush it instead.
	s.flushPartialCSI()
	return s.out, ev
}

// flushPartialCSI emits any bytes left in the CSI capture buffer (a lone ESC or
// an unrecognized partial) verbatim, so they aren't held past the read that
// produced them. No-op when paste mode owns pasteBuf or it's empty.
func (s *escapeScanner) flushPartialCSI() {
	if s.paste || len(s.pasteBuf) == 0 {
		return
	}
	s.out = append(s.out, s.pasteBuf...)
	s.pasteBuf = s.pasteBuf[:0]
}

// forward appends b to the out buffer, intercepting mouse-report sequences to
// fix the row offset and bracketed-paste START to enter paste mode. Most bytes
// pass straight through.
func (s *escapeScanner) forward(b byte) {
	// Continue an in-progress CSI capture (paste marker or mouse sequence).
	if len(s.pasteBuf) > 0 {
		s.pasteBuf = append(s.pasteBuf, b)
		s.tryCSI()
		return
	}
	if b == 0x1b { // ESC — begin capturing a possible CSI we care about
		s.pasteBuf = append(s.pasteBuf[:0], b)
		return
	}
	s.out = append(s.out, b)
}

// tryCSI inspects the captured ESC… buffer. It recognizes:
//   - ESC[200~  bracketed-paste START → enter paste mode (forward the marker)
//   - ESC[<…M / ESC[<…m  SGR mouse → rewrite the row, then forward
//   - ESC[M b cx cy  X10 mouse → rewrite the row byte, then forward
//
// Anything else (or once it's clearly not one of these) is flushed verbatim.
func (s *escapeScanner) tryCSI() {
	buf := s.pasteBuf
	// Need at least ESC [ to decide.
	if len(buf) < 2 {
		return
	}
	if buf[1] != '[' {
		// Not a CSI we track — flush and stop capturing.
		s.flushCSI()
		return
	}
	// X10 mouse: ESC [ M b cx cy  (6 bytes, cy = row+32).
	if len(buf) >= 3 && buf[2] == 'M' {
		if len(buf) >= 6 {
			cy := int(buf[5]) - 32
			cy -= chromeRows
			if cy < 1 {
				cy = 1
			}
			buf[5] = byte(cy + 32)
			s.flushCSI()
		}
		return // wait for all 6 bytes
	}
	// SGR mouse: ESC [ < Cb ; Cx ; Cy (M|m). Capture until the final M/m.
	if len(buf) >= 3 && buf[2] == '<' {
		last := buf[len(buf)-1]
		if last == 'M' || last == 'm' {
			s.flushCSI() // rewriteSGRMouse runs inside flushCSI's SGR branch
			return
		}
		// Guard against an unbounded capture on malformed input.
		if len(buf) > 32 {
			s.flushCSI()
		}
		return
	}
	// Bracketed-paste START: ESC [ 2 0 0 ~
	pasteStart := []byte("\x1b[200~")
	if bytes.HasPrefix(buf, pasteStart) {
		s.out = append(s.out, buf...)
		s.pasteBuf = s.pasteBuf[:0]
		s.paste = true
		return
	}
	// Could still be growing toward ESC[200~ or a partial; keep capturing up to
	// the length of the longest marker we track, else flush.
	if len(buf) < len(pasteStart) && bytes.HasPrefix(pasteStart, buf) {
		return
	}
	// Not a sequence we rewrite — flush verbatim.
	if isFinalCSIByte(buf[len(buf)-1]) || len(buf) > 32 {
		s.flushCSI()
	}
}

// flushCSI emits the captured CSI buffer to out. If it is an SGR mouse sequence
// it rewrites the row first; otherwise it passes through verbatim.
func (s *escapeScanner) flushCSI() {
	buf := s.pasteBuf
	if len(buf) >= 4 && buf[1] == '[' && buf[2] == '<' {
		last := buf[len(buf)-1]
		if last == 'M' || last == 'm' {
			s.out = append(s.out, rewriteSGRMouseRow(buf)...)
			s.pasteBuf = s.pasteBuf[:0]
			return
		}
	}
	s.out = append(s.out, buf...)
	s.pasteBuf = s.pasteBuf[:0]
}

// trackPasteEnd watches the byte stream (already forwarded) for the bracketed-
// paste END marker ESC[201~, exiting paste mode when complete. On exit it sets
// the line-start state from the LAST PAYLOAD byte (the byte before the marker):
// a paste ending in a newline re-arms a line-start escape, matching the user's
// intent (the shell is on a fresh prompt line), while a paste ending mid-line
// leaves the next byte disarmed.
func (s *escapeScanner) trackPasteEnd(b byte) {
	end := []byte("\x1b[201~")
	if b == end[len(s.pasteBuf)] {
		s.pasteBuf = append(s.pasteBuf, b)
		if len(s.pasteBuf) == len(end) {
			s.paste = false
			s.pasteBuf = s.pasteBuf[:0]
			// The last len(end) bytes of out are the marker; the one before it is
			// the last payload byte.
			s.st = stMidLine
			if n := len(s.out); n > len(end) {
				s.st = atLineStartAfter(s.out[n-len(end)-1])
			}
		}
		return
	}
	// Mismatch: reset, but re-test this byte against the marker's first byte so
	// "ESC ESC [ 2 0 1 ~" still matches.
	s.pasteBuf = s.pasteBuf[:0]
	if b == end[0] {
		s.pasteBuf = append(s.pasteBuf, b)
	}
}

// rewriteSGRMouseRow parses ESC[<Cb;Cx;Cy(M|m) and returns the sequence with Cy
// reduced by chromeRows (clamped to >= 1). On any parse failure it returns the
// sequence untouched.
func rewriteSGRMouseRow(buf []byte) []byte {
	// strip ESC [ < … final
	body := string(buf[3 : len(buf)-1])
	final := buf[len(buf)-1]
	parts := strings.Split(body, ";")
	if len(parts) != 3 {
		return buf
	}
	cy, err := strconv.Atoi(parts[2])
	if err != nil {
		return buf
	}
	cy -= chromeRows
	if cy < 1 {
		cy = 1
	}
	out := make([]byte, 0, len(buf))
	out = append(out, '\x1b', '[', '<')
	out = append(out, parts[0]...)
	out = append(out, ';')
	out = append(out, parts[1]...)
	out = append(out, ';')
	out = append(out, strconv.Itoa(cy)...)
	out = append(out, final)
	return out
}

// atLineStartAfter reports the scanner state after forwarding byte b: armed at
// line start iff b ended a line.
func atLineStartAfter(b byte) escState {
	if b == '\r' || b == '\n' {
		return stLineStart
	}
	return stMidLine
}

// isFinalCSIByte reports whether b terminates a CSI sequence (the "final byte"
// range 0x40–0x7e).
func isFinalCSIByte(b byte) bool {
	return b >= 0x40 && b <= 0x7e
}
