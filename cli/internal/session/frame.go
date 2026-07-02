package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// maxFrame caps a single control/response frame so a misbehaving server can't
// make the CLI read an unbounded line into memory.
const maxFrame = 1 << 20

// writeFrame marshals v and writes it as one newline-terminated JSON line.
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// readFrameCtx reads one newline-terminated JSON frame from r into v, bounded by
// ctx. Used where r is read once and discarded (List).
func readFrameCtx(ctx context.Context, r io.Reader, v any) error {
	return readFrameBuffered(ctx, bufio.NewReader(r), v)
}

// readFrameBuffered reads one newline-terminated JSON frame from br into v,
// bounded by ctx. The bufio.Reader is retained by the caller so any raw bytes
// already buffered after the frame's newline aren't lost (the attach path reads
// the raw PTY stream through the same reader).
func readFrameBuffered(ctx context.Context, br *bufio.Reader, v any) error {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := readLineLimited(br, maxFrame)
		ch <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if err := json.Unmarshal(res.line, v); err != nil {
			return fmt.Errorf("decode frame %q: %w", truncate(res.line), err)
		}
		return nil
	}
}

// readLineLimited reads up to (and including) the next '\n' from br, returning
// the line WITHOUT the trailing newline, capped at max bytes.
func readLineLimited(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return line, nil // last line without a trailing newline
			}
			return nil, err
		}
		if b == '\n' {
			return line, nil
		}
		line = append(line, b)
		if len(line) >= max {
			return nil, fmt.Errorf("frame exceeds %d bytes", max)
		}
	}
}

func truncate(b []byte) string {
	const n = 200
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
