// Package table renders aligned columnar output over text/tabwriter for the
// fixed-labs CLIs — replacing hand-rolled fixed-width %-Ns formatting that
// misaligns when a value overflows its column. Elastic: columns auto-size to
// their widest cell. Optional per-column max-width (truncating with "…") and
// right-alignment. Stdlib only.
package table

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

type Table struct {
	w         io.Writer
	headers   []string
	rows      [][]string
	maxWidths []int
	rightCols map[int]bool
}

func New(w io.Writer, headers ...string) *Table {
	return &Table{w: w, headers: headers, rightCols: map[int]bool{}}
}

// WithMaxWidths caps column widths (truncating with "…"); a 0 entry (or a
// missing trailing entry) means no cap for that column.
func (t *Table) WithMaxWidths(max ...int) *Table {
	t.maxWidths = max
	return t
}

// WithRightAlign right-justifies the given 0-based column indices (numeric
// columns). text/tabwriter has no per-column alignment, so Flush pre-justifies
// these cells to the computed column width.
func (t *Table) WithRightAlign(cols ...int) *Table {
	for _, c := range cols {
		t.rightCols[c] = true
	}
	return t
}

func (t *Table) Row(cells ...string) *Table {
	t.rows = append(t.rows, cells)
	return t
}

// Flush writes the header and rows aligned, then flushes the tabwriter.
func (t *Table) Flush() error {
	tw := tabwriter.NewWriter(t.w, 0, 0, 2, ' ', 0)
	all := append([][]string{t.headers}, t.rows...)

	// Column display widths (post-cap) so right-aligned columns can be
	// pre-justified — tabwriter offers no per-column alignment.
	widths := map[int]int{}
	for _, row := range all {
		for i, c := range row {
			if w := len([]rune(t.cap(i, c))); w > widths[i] {
				widths[i] = w
			}
		}
	}
	for _, row := range all {
		out := make([]string, len(row))
		for i, c := range row {
			c = t.cap(i, c)
			if t.rightCols[i] {
				if pad := widths[i] - len([]rune(c)); pad > 0 {
					c = strings.Repeat(" ", pad) + c
				}
			}
			out[i] = c
		}
		fmt.Fprintln(tw, strings.Join(out, "\t"))
	}
	return tw.Flush()
}

func (t *Table) cap(col int, s string) string {
	if col < len(t.maxWidths) && t.maxWidths[col] > 0 {
		if r := []rune(s); len(r) > t.maxWidths[col] {
			return string(r[:t.maxWidths[col]-1]) + "…"
		}
	}
	return s
}
