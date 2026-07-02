package sessions

import (
	"bytes"
	"sync"
)

// ring is a fixed-size byte ring of raw terminal output (the scrollback buffer).
// It tracks the byte offset, in the LOGICAL (total-bytes-ever-written) stream,
// of the most recent full-screen clear / cursor-home so a reattach can replay
// from there — never mid-escape-sequence — and the child redraws cleanly. When
// no clear sits inside the window, replay is the whole ring (its start is
// read-aligned, so a stray partial escape at the very top scrolls away
// harmlessly).
//
// The clear markers tracked (ANSI): ESC[2J (erase entire screen), ESC[H (cursor
// home, no params), ESC[3J (erase scrollback). These are emitted by full-screen
// apps (and `clear`) at repaint; replaying from the last one yields a coherent
// frame without the agent modelling a terminal grid.
type ring struct {
	mu   sync.Mutex
	buf  []byte // the data, len ≤ cap == ringSize
	cap  int
	base int64 // logical offset of buf[0] (bytes evicted so far)
	// clearAt is the logical offset of the FIRST byte of the most-recent clear
	// sequence; -1 if none ever seen / none still in the window.
	clearAt int64
	// pending carries bytes of a clear escape that straddled the previous
	// append (so a marker split across two PTY reads is still detected).
	carry []byte
}

func newRing(capacity int) *ring {
	return &ring{buf: make([]byte, 0, capacity), cap: capacity, clearAt: -1}
}

// clearSeqs are the byte sequences that signal a full-screen clear / home.
var clearSeqs = [][]byte{
	[]byte("\x1b[2J"),
	[]byte("\x1b[3J"),
	[]byte("\x1b[H"),
}

// maxClearLen is the longest clear sequence — the carry window for split-read
// detection.
const maxClearLen = 4

// append adds p to the ring, evicting from the front past the cap, and updates
// the most-recent-clear marker (scanning carry+p so a marker split across two
// reads is still found). p is owned by the caller after this returns (we copy
// what we keep).
func (r *ring) append(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Logical offset where p begins in the total stream.
	pStart := r.base + int64(len(r.buf))

	// Scan carry+p for clear markers, keeping the MAX (most-recent) offset found
	// in this append. The carry holds up to maxClearLen-1 trailing bytes of the
	// previous append so a marker STRADDLING the read boundary is still caught.
	// A match whose LAST byte is still within the carry (end ≤ pStart) lies
	// entirely in the previous append and was already recorded — skip it; a
	// straddling match (starts in carry, ends in p) is new and must be kept.
	scan := append([]byte(nil), r.carry...)
	scan = append(scan, p...)
	scanStart := pStart - int64(len(r.carry))
	for _, seq := range clearSeqs {
		idx := 0
		for {
			rel := bytes.Index(scan[idx:], seq)
			if rel < 0 {
				break
			}
			at := scanStart + int64(idx+rel)
			end := at + int64(len(seq)) // logical offset just past the match
			if end > pStart && at > r.clearAt {
				r.clearAt = at
			}
			idx += rel + 1
		}
	}

	// Update carry to the last maxClearLen-1 bytes of p (for the next append).
	tailN := maxClearLen - 1
	if len(p) < tailN {
		tailN = len(p)
	}
	r.carry = append(r.carry[:0], p[len(p)-tailN:]...)

	// Append, then evict from the front to respect cap.
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		drop := len(r.buf) - r.cap
		r.buf = append(r.buf[:0], r.buf[drop:]...)
		r.base += int64(drop)
	}
	// If the clear marker scrolled out of the window, forget it (replay falls
	// back to the whole ring).
	if r.clearAt >= 0 && r.clearAt < r.base {
		r.clearAt = -1
	}
}

// replay returns the bytes to send to a newly-attached client: from the most
// recent in-window clear marker (inclusive) to the end, or the whole ring if no
// marker is in the window. Returns a fresh copy (safe to retain).
func (r *ring) replay() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := 0
	if r.clearAt >= r.base {
		start = int(r.clearAt - r.base)
	}
	if start > len(r.buf) {
		start = len(r.buf)
	}
	out := make([]byte, len(r.buf)-start)
	copy(out, r.buf[start:])
	return out
}
