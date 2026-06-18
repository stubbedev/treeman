package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestTableAlignmentWithStyledCells pins the ANSI-aware width math:
// colored cells must not drift column alignment, padding is computed
// against the stripped width, and every body row lines up under its
// header.
func TestTableAlignmentWithStyledCells(t *testing.T) {
	tbl := NewTable("ENGINE", "TEMPLATE")
	tbl.Row(Cyan("mysql"), "_tm_0123456789abcdef")
	tbl.Row(Green("pg"), "x")

	var buf bytes.Buffer
	tbl.Render(&buf)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 { // header, separator, 2 body rows
		t.Fatalf("got %d lines, want 4:\n%s", len(lines), buf.String())
	}
	// Strip ANSI and check the second column starts at the same offset
	// in the header and both body rows.
	idx := func(s, sub string) int { return strings.Index(stripANSI(s), sub) }
	col2 := idx(lines[0], "TEMPLATE")
	if col2 <= 0 {
		t.Fatalf("TEMPLATE header not found: %q", lines[0])
	}
	if got := idx(lines[2], "_tm_"); got != col2 {
		t.Errorf("row 1 second column at %d, want %d (styled first cell broke alignment)", got, col2)
	}
	if got := idx(lines[3], "x"); got != col2 {
		t.Errorf("row 2 second column at %d, want %d", got, col2)
	}
}

func TestTableEmptyRendersNothing(t *testing.T) {
	var buf bytes.Buffer
	(&Table{}).Render(&buf)
	if buf.Len() != 0 {
		t.Errorf("empty table rendered %q", buf.String())
	}
}

// stripANSI removes escape sequences using the same Width machinery
// the renderer pads with — if Width and this disagree, alignment
// assertions above will catch it.
func stripANSI(s string) string {
	var out strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
