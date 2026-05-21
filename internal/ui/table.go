package ui

import (
	"fmt"
	"io"
	"strings"
)

// Table renders a tabular view to Out. ANSI-aware width math so
// colored cells don't break alignment.
type Table struct {
	headers []string
	rows    [][]string
	gap     string
}

// NewTable builds a table with the given column headers. The first
// row of the rendered output is the bolded header row.
func NewTable(headers ...string) *Table {
	return &Table{headers: headers, gap: "  "}
}

// Row appends one row. Pass cells as styled strings (Green/Cyan/etc.)
// — the renderer strips ANSI before measuring width.
func (t *Table) Row(cells ...string) {
	t.rows = append(t.rows, cells)
}

// Render writes the table to w (defaults to ui.Out when nil).
func (t *Table) Render(w io.Writer) {
	if w == nil {
		w = Out
	}
	if len(t.rows) == 0 && len(t.headers) == 0 {
		return
	}
	widths := make([]int, len(t.headers))
	for i, h := range t.headers {
		widths[i] = Width(h)
	}
	for _, row := range t.rows {
		for i, c := range row {
			if i >= len(widths) {
				continue
			}
			if w := Width(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	// Header
	for i, h := range t.headers {
		writeCell(w, Bold(h), widths[i])
		if i < len(t.headers)-1 {
			fmt.Fprint(w, t.gap)
		}
	}
	fmt.Fprintln(w)
	// Separator (dim)
	for i, wi := range widths {
		fmt.Fprint(w, Dim(strings.Repeat("─", wi)))
		if i < len(widths)-1 {
			fmt.Fprint(w, t.gap)
		}
	}
	fmt.Fprintln(w)
	// Body
	for _, row := range t.rows {
		for i, cell := range row {
			if i >= len(widths) {
				continue
			}
			writeCell(w, cell, widths[i])
			if i < len(row)-1 && i < len(widths)-1 {
				fmt.Fprint(w, t.gap)
			}
		}
		fmt.Fprintln(w)
	}
}

// writeCell prints cell followed by enough trailing spaces to fill
// the column width. Padding is computed against the ANSI-stripped
// width so colored cells don't drift.
func writeCell(w io.Writer, cell string, width int) {
	fmt.Fprint(w, cell)
	pad := width - Width(cell)
	if pad > 0 {
		fmt.Fprint(w, strings.Repeat(" ", pad))
	}
}
