//go:build unix

package compositor

import (
	"fmt"
	"io"
	"strings"
)

// PickItem is one row in the session picker.
type PickItem struct {
	ID    string
	Label string // human label (name + cmd/cwd + attached count)
}

// PickResult is the outcome of running the picker.
type PickResult struct {
	// Selected is the chosen item's ID. Empty when the user chose "new" or
	// aborted.
	Selected string
	// New is true when the user asked to create a new session.
	New bool
	// Aborted is true when the user pressed Esc / Ctrl-C / q without choosing.
	Aborted bool
}

// RunPicker renders an interactive, arrow-key session picker directly on the
// terminal (in/out must be a raw-mode TTY pair — the caller owns raw mode). It
// is used for the initial >1-session case and for the in-session `~s` switcher,
// running BETWEEN compositor sessions so the compositor's run loop stays bound to
// one session at a time (the inner-region↔session seam).
//
// Keys: ↑/↓ or j/k move, Enter selects, n creates a new session, q/Esc/Ctrl-C
// abort.
func RunPicker(in io.Reader, out io.Writer, items []PickItem, header string) (PickResult, error) {
	if len(items) == 0 {
		return PickResult{New: true}, nil
	}
	sel := 0
	draw := func() {
		var b strings.Builder
		b.WriteString("\x1b[2J\x1b[H") // clear + home
		if header != "" {
			b.WriteString(header)
			b.WriteString("\r\n\r\n")
		}
		for i, it := range items {
			marker := "  "
			line := it.Label
			if i == sel {
				marker = "\x1b[7m> " // reverse video for the cursor row
				line = line + "\x1b[0m"
			}
			b.WriteString(marker)
			b.WriteString(line)
			b.WriteString("\r\n")
		}
		b.WriteString("\r\n")
		b.WriteString("↑/↓ move · Enter attach · n new · q quit\r\n")
		_, _ = io.WriteString(out, b.String())
	}
	draw()

	buf := make([]byte, 16)
	for {
		n, err := in.Read(buf)
		if err != nil {
			return PickResult{Aborted: true}, err
		}
		for i := 0; i < n; i++ {
			switch buf[i] {
			case 0x03, 'q', 0x1b: // Ctrl-C, q — abort. (Esc handled below if a bare ESC.)
				if buf[i] == 0x1b {
					// Could be the start of an arrow-key CSI; peek the rest of this read.
					if i+2 < n && buf[i+1] == '[' {
						switch buf[i+2] {
						case 'A': // up
							if sel > 0 {
								sel--
							}
							i += 2
							draw()
							continue
						case 'B': // down
							if sel < len(items)-1 {
								sel++
							}
							i += 2
							draw()
							continue
						}
					}
				}
				return PickResult{Aborted: true}, nil
			case 'k':
				if sel > 0 {
					sel--
				}
				draw()
			case 'j':
				if sel < len(items)-1 {
					sel++
				}
				draw()
			case 'n', 'N':
				return PickResult{New: true}, nil
			case '\r', '\n':
				return PickResult{Selected: items[sel].ID}, nil
			}
		}
	}
}

// FormatPickLabel builds a one-line picker label from session fields.
func FormatPickLabel(name, cmd, cwd string, attached int) string {
	parts := []string{name}
	if cmd != "" {
		parts = append(parts, cmd)
	}
	if cwd != "" {
		parts = append(parts, cwd)
	}
	if attached > 0 {
		parts = append(parts, fmt.Sprintf("(%d attached)", attached))
	}
	return strings.Join(parts, "  ")
}
