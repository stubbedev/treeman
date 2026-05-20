package dumpload

import "testing"

func TestSplitsSimpleStatements(t *testing.T) {
	s := "SELECT 1; SELECT 2; SELECT 3"
	v := iterStatements(s)
	if len(v) != 3 {
		t.Errorf("got %d statements: %v", len(v), v)
	}
}

func TestSemicolonInsideStringDoesNotSplit(t *testing.T) {
	s := "INSERT INTO t VALUES ('a;b'); SELECT 1;"
	v := iterStatements(s)
	if len(v) != 2 {
		t.Errorf("got %d statements: %v", len(v), v)
	}
	if !contains(v[0], "a;b") {
		t.Errorf("first stmt missing literal: %q", v[0])
	}
}

func TestLineCommentsSkipped(t *testing.T) {
	s := "-- comment;\nSELECT 1;"
	v := iterStatements(s)
	if len(v) != 1 {
		t.Errorf("got %d statements: %v", len(v), v)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
