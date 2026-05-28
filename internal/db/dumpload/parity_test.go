package dumpload

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// streamStatementsRef is a verbatim copy of the original byte-by-byte
// streamStatements implementation (bufio.ReadByte + Peek/Discard). It
// is the oracle FuzzStreamStatementsParity pins the chunked rewrite
// to: any input where the new scanner disagrees with this reference
// is a regression. Kept here, in the test binary only, so production
// carries a single implementation.
func streamStatementsRef(ctx context.Context, r io.Reader, onStmt func(stmt string) error) (uint64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var buf bytes.Buffer
	var inSingle, inDouble, inBacktick bool
	var applied uint64
	flush := func() error {
		stmt := buf.String()
		buf.Reset()
		if isBlank(stmt) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := onStmt(stmt); err != nil {
			return fmt.Errorf("apply dump stmt #%d: %w", applied, err)
		}
		applied++
		return nil
	}
	skipUntil := func(sep byte) error {
		for {
			b, err := br.ReadByte()
			if err != nil {
				return err
			}
			if b == sep {
				return nil
			}
		}
	}
	skipBlockComment := func() error {
		for {
			b, err := br.ReadByte()
			if err != nil {
				return err
			}
			if b == '*' {
				peek, _ := br.Peek(1)
				if len(peek) == 1 && peek[0] == '/' {
					_, _ = br.Discard(1)
					return nil
				}
			}
		}
	}
	for {
		b, err := br.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return applied, err
		}
		if !(inSingle || inDouble || inBacktick) {
			if b == '-' {
				if peek, _ := br.Peek(1); len(peek) == 1 && peek[0] == '-' {
					_, _ = br.Discard(1)
					if err := skipUntil('\n'); err != nil && !errors.Is(err, io.EOF) {
						return applied, err
					}
					continue
				}
			}
			if b == '/' {
				if peek, _ := br.Peek(1); len(peek) == 1 && peek[0] == '*' {
					_, _ = br.Discard(1)
					if err := skipBlockComment(); err != nil && !errors.Is(err, io.EOF) {
						return applied, err
					}
					continue
				}
			}
		}
		switch b {
		case '\'':
			if !inDouble && !inBacktick {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle && !inBacktick {
				inDouble = !inDouble
			}
		case '`':
			if !inSingle && !inDouble {
				inBacktick = !inBacktick
			}
		case ';':
			if !inSingle && !inDouble && !inBacktick {
				if err := flush(); err != nil {
					return applied, err
				}
				continue
			}
		}
		buf.WriteByte(b)
	}
	if err := flush(); err != nil {
		return applied, err
	}
	return applied, nil
}

// syntheticDump builds a mysqldump-shaped blob: comments, a CREATE,
// and many extended-INSERT statements carrying quoted string literals
// with embedded semicolons — the byte mix that dominates real dumps.
func syntheticDump(targetBytes int) string {
	var b strings.Builder
	b.WriteString("-- MySQL dump\n/*!40101 SET NAMES utf8 */;\n")
	b.WriteString("CREATE TABLE `t` (`id` INT, `v` TEXT);\n")
	row := "INSERT INTO `t` VALUES (1,'a;b\"c`d, lorem ipsum dolor sit amet');\n"
	for b.Len() < targetBytes {
		b.WriteString(row)
	}
	return b.String()
}

func benchStream(b *testing.B, fn func(context.Context, io.Reader, func(string) error) (uint64, error)) {
	dump := syntheticDump(8 << 20) // ~8 MiB
	b.SetBytes(int64(len(dump)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fn(context.Background(), strings.NewReader(dump), func(string) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamStatements(b *testing.B)    { benchStream(b, streamStatements) }
func BenchmarkStreamStatementsRef(b *testing.B) { benchStream(b, streamStatementsRef) }

func collectStatements(t *testing.T, fn func(context.Context, io.Reader, func(string) error) (uint64, error), r io.Reader) ([]string, uint64) {
	t.Helper()
	var out []string
	applied, err := fn(context.Background(), r, func(stmt string) error {
		out = append(out, stmt)
		return nil
	})
	if err != nil {
		t.Fatalf("stream returned error: %v", err)
	}
	return out, applied
}

// FuzzStreamStatementsParity asserts the production chunked scanner
// yields exactly the same statements (and applied count) as the
// reference byte-by-byte implementation, for both a normal reader and
// a one-byte-at-a-time reader. The latter forces every two-character
// lookahead (`--`, `/*`, `*/`) to straddle a Read boundary, exercising
// the dashSeen / slashSeen / starSeen carry-over state.
func FuzzStreamStatementsParity(f *testing.F) {
	seeds := []string{
		"SELECT 1; SELECT 2; SELECT 3",
		"INSERT INTO t VALUES ('a;b'); SELECT 1;",
		"-- comment;\nSELECT 1;",
		"/* multi;\nline */ SELECT 1; /* trailing */ SELECT 2;",
		"CREATE TABLE `weird;name` (id INT); SELECT 1;",
		`INSERT INTO t VALUES ('a\'b', 'c;d');`,
		"SELECT '\"'; SELECT \"';\";",
		"SELECT 1 -",
		"SELECT 1 /",
		"a--b\nc;",
		"a-/b;",
		"/**/;SELECT 1;",
		"`a``b`;",
		"''';';",
		"-- only a comment no newline",
		"/* unterminated block comment",
		"'unterminated string ; literal",
		"SELECT 1;\n\n   \n;SELECT 2;",
		"",
		";;;",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		wantStmts, wantApplied := collectStatements(t, streamStatementsRef, strings.NewReader(in))

		gotStmts, gotApplied := collectStatements(t, streamStatements, strings.NewReader(in))
		if gotApplied != wantApplied || !reflect.DeepEqual(gotStmts, wantStmts) {
			t.Fatalf("parity mismatch (normal reader)\n input:  %q\n want(%d): %#v\n got (%d): %#v",
				in, wantApplied, wantStmts, gotApplied, gotStmts)
		}

		// Force every chunk boundary to land mid-lookahead.
		obStmts, obApplied := collectStatements(t, streamStatements, iotest.OneByteReader(strings.NewReader(in)))
		if obApplied != wantApplied || !reflect.DeepEqual(obStmts, wantStmts) {
			t.Fatalf("parity mismatch (one-byte reader)\n input:  %q\n want(%d): %#v\n got (%d): %#v",
				in, wantApplied, wantStmts, obApplied, obStmts)
		}
	})
}
