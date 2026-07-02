//go:build unix

package compositor

import (
	"fmt"
	"strings"
)

// Label formats the chrome text — the single source of truth for the bar's
// wording. It shows BOTH ids: the workspace id (the box, matches `devbox ls`) and
// the session id (which shell you're attached to). chromeBar truncates from the
// left on a narrow terminal, so the workspace id is shown first.
func Label(workspaceID, sessionID string) string {
	return fmt.Sprintf("rift: %s / %s ", workspaceID, sessionID)
}

// Chrome colors: white text on a calm green bar (GitHub's success green,
// #2ea043). Emitted as truecolor SGR, which the screen's color profile
// down-converts for 256/16-color terminals. Tweak the bar's look here.
const (
	chromeFG = "\x1b[38;2;255;255;255m" // white
	chromeBG = "\x1b[48;2;46;160;67m"   // green #2ea043
)

// chromeBar renders label as a full-width bar of exactly cols display columns.
// The returned string carries SGR escapes, which uv.NewStyledString parses back
// into styled cells. The "~? for help" hint is appended on the right when it
// fits, so the escape menu is discoverable without cluttering a narrow terminal.
func chromeBar(label string, cols int) string {
	if cols < 1 {
		return ""
	}
	const hint = "~? help "
	left := []rune(label)
	right := []rune(hint)
	// Drop the hint if it would collide with the label.
	if len(left)+len(right)+1 > cols {
		right = nil
	}
	if len(left) > cols {
		left = left[:cols]
	}
	var b strings.Builder
	b.WriteString(chromeBG)
	b.WriteString(chromeFG)
	b.WriteString(string(left))
	pad := cols - len(left) - len(right)
	for i := 0; i < pad; i++ {
		b.WriteByte(' ') // pad to full width; padding inherits the green bg
	}
	b.WriteString(string(right))
	b.WriteString("\x1b[0m")
	return b.String()
}
