package table

import (
	"bytes"
	"strings"
	"testing"
)

func TestElasticAlignsColumns(t *testing.T) {
	var b bytes.Buffer
	New(&b, "NAME", "V").Row("longvalue", "x").Row("y", "z").Flush()
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %v", lines)
	}
	// The second column starts at the same offset on every line (auto-width).
	off := strings.Index(lines[0], "V")
	if off <= 0 {
		t.Fatalf("no V column: %q", lines[0])
	}
	for _, l := range lines {
		if strings.TrimSpace(l[off:]) == "" {
			t.Fatalf("column not aligned at %d: %q", off, l)
		}
	}
}

func TestWithMaxWidthsTruncates(t *testing.T) {
	var b bytes.Buffer
	New(&b, "REPO").WithMaxWidths(5).Row("abcdefgh").Flush()
	out := b.String()
	if !strings.Contains(out, "abcd…") { // capped to 5 runes: 4 + "…"
		t.Fatalf("want truncated 'abcd…', got %q", out)
	}
	if strings.Contains(out, "abcdefgh") {
		t.Fatalf("must not contain full value: %q", out)
	}
}

func TestWithMaxWidthsRuneAware(t *testing.T) {
	var b bytes.Buffer
	New(&b, "X").WithMaxWidths(3).Row("café").Flush() // 4 runes → 2 + "…"
	if !strings.Contains(b.String(), "ca…") {
		t.Fatalf("rune-aware truncation failed: %q", b.String())
	}
}

func TestWithRightAlignJustifies(t *testing.T) {
	var b bytes.Buffer
	New(&b, "NAME", "N").WithRightAlign(1).Row("a", "1").Row("bb", "100").Flush()
	out := b.String()
	// Column 1's width is 3 ("100"); "1" right-justified → "  1", header "N" → "  N".
	if !strings.Contains(out, "  1") || !strings.Contains(out, "  N") {
		t.Fatalf("right-aligned column not justified: %q", out)
	}
	if !strings.Contains(out, "100") {
		t.Fatalf("missing widest value 100: %q", out)
	}
}
