//go:build unix

package compositor

import (
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
)

// overlay is a transient layer drawn over the inner region: connection toasts
// and the help card today. (The interactive session picker/switcher runs as a
// standalone Picker before/between compositor sessions — see picker.go — so the
// compositor's run loop stays single-purpose; this overlay is the in-session
// presentation layer.)
type overlay struct {
	title string
	lines []string
}

func helpOverlay() *overlay {
	return &overlay{
		title: "rift session — escapes",
		lines: []string{
			"~d  or  ~.   detach — leave the session running; `rift connect` resumes it",
			"~s           switch sessions",
			"~?           this help",
			"~~           type a literal ~",
			"",
			"leaving, and what happens to your session:",
			"  detach (~d) or close this window  → session keeps running on the box",
			"  `exit` or Ctrl-D                  → ENDS the session (shell + state gone)",
			"  box stops (idle-suspend/resume)   → session does NOT survive; you get a new one",
			"",
			"press any key to dismiss",
		},
	}
}

// draw renders the overlay as a centered card over the inner rows (below the
// chrome). It paints a bordered box; cells outside the box keep the inner grid
// (already drawn underneath this call).
func (o *overlay) draw(scr *uv.TerminalScreen, cols, rows int) {
	innerTop := chromeRows
	innerHeight := rows - chromeRows
	if innerHeight < 3 || cols < 4 {
		return
	}

	body := make([]string, 0, len(o.lines)+2)
	if o.title != "" {
		body = append(body, o.title, "")
	}
	body = append(body, o.lines...)

	// Box width: widest line + padding, clamped to the terminal.
	w := 0
	for _, l := range body {
		if n := len([]rune(l)); n > w {
			w = n
		}
	}
	w += 4 // 1 border + 1 pad each side
	if w > cols {
		w = cols
	}
	h := len(body) + 2 // borders
	if h > innerHeight {
		h = innerHeight
		body = body[:h-2]
	}

	x0 := (cols - w) / 2
	y0 := innerTop + (innerHeight-h)/2

	style := uv.NewStyledString
	put := func(x, y int, s string) {
		ss := style(s)
		ss.Draw(scr, uv.Rect(x, y, len([]rune(s)), 1))
	}

	// Top border.
	put(x0, y0, "\x1b[48;2;30;30;30m\x1b[97m"+"┌"+strings.Repeat("─", w-2)+"┐"+"\x1b[0m")
	for i, l := range body {
		runes := []rune(l)
		if len(runes) > w-4 {
			runes = runes[:w-4]
		}
		text := string(runes)
		pad := w - 4 - len([]rune(text))
		row := "\x1b[48;2;30;30;30m\x1b[97m" + "│ " + text + strings.Repeat(" ", pad) + " │" + "\x1b[0m"
		put(x0, y0+1+i, row)
	}
	// Bottom border.
	put(x0, y0+h-1, "\x1b[48;2;30;30;30m\x1b[97m"+"└"+strings.Repeat("─", w-2)+"┘"+"\x1b[0m")
}
