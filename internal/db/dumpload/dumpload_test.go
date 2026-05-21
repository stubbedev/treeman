package dumpload

import (
	"context"
	"strings"
	"testing"
)

// statementsFrom collects every non-blank statement streamStatements
// yields from `s`. Test-only helper — the production path streams
// to a database/sql handle and never materializes the slice.
func statementsFrom(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	_, err := streamStatements(context.Background(), strings.NewReader(s), func(stmt string) error {
		out = append(out, stmt)
		return nil
	})
	if err != nil {
		t.Fatalf("streamStatements: %v", err)
	}
	return out
}

func TestSplitsSimpleStatements(t *testing.T) {
	v := statementsFrom(t, "SELECT 1; SELECT 2; SELECT 3")
	if len(v) != 3 {
		t.Errorf("got %d statements: %v", len(v), v)
	}
}

func TestSemicolonInsideStringDoesNotSplit(t *testing.T) {
	v := statementsFrom(t, "INSERT INTO t VALUES ('a;b'); SELECT 1;")
	if len(v) != 2 {
		t.Errorf("got %d statements: %v", len(v), v)
	}
	if !strings.Contains(v[0], "a;b") {
		t.Errorf("first stmt missing literal: %q", v[0])
	}
}

func TestLineCommentsSkipped(t *testing.T) {
	v := statementsFrom(t, "-- comment;\nSELECT 1;")
	if len(v) != 1 {
		t.Errorf("got %d statements: %v", len(v), v)
	}
}

func TestBlockCommentsSkipped(t *testing.T) {
	v := statementsFrom(t, "/* multi;\nline */ SELECT 1; /* trailing */ SELECT 2;")
	if len(v) != 2 {
		t.Errorf("got %d statements: %v", len(v), v)
	}
}

func TestBacktickIdentifierWithSemicolon(t *testing.T) {
	v := statementsFrom(t, "CREATE TABLE `weird;name` (id INT); SELECT 1;")
	if len(v) != 2 {
		t.Errorf("got %d statements: %v", len(v), v)
	}
}
