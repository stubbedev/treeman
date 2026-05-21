package binlog

import (
	"reflect"
	"testing"
)

func TestBuildInsert(t *testing.T) {
	meta := &tableMeta{Columns: []string{"id", "name"}, PKCols: []int{0}}
	q, args := buildInsert("clone_db", "users", meta, []any{int64(1), "alice"})
	want := "INSERT INTO `clone_db`.`users` (`id`, `name`) VALUES (?, ?)"
	if q != want {
		t.Errorf("query=\n%q\nwant\n%q", q, want)
	}
	if !reflect.DeepEqual(args, []any{int64(1), "alice"}) {
		t.Errorf("args=%v", args)
	}
}

func TestBuildDeleteWithPK(t *testing.T) {
	meta := &tableMeta{Columns: []string{"id", "name"}, PKCols: []int{0}}
	q, args := buildDelete("c", "users", meta, []any{int64(7), "bob"})
	want := "DELETE FROM `c`.`users` WHERE `id`=? LIMIT 1"
	if q != want {
		t.Errorf("query=%q want %q", q, want)
	}
	if !reflect.DeepEqual(args, []any{int64(7)}) {
		t.Errorf("args=%v want [7]", args)
	}
}

func TestBuildDeleteNoPKFallsBackToFullRow(t *testing.T) {
	meta := &tableMeta{Columns: []string{"a", "b"}, PKCols: nil}
	q, args := buildDelete("c", "kv", meta, []any{"x", nil})
	want := "DELETE FROM `c`.`kv` WHERE `a` <=> ? AND `b` <=> ? LIMIT 1"
	if q != want {
		t.Errorf("query=%q", q)
	}
	if !reflect.DeepEqual(args, []any{"x", nil}) {
		t.Errorf("args=%v", args)
	}
}

func TestBuildUpdate(t *testing.T) {
	meta := &tableMeta{Columns: []string{"id", "name", "email"}, PKCols: []int{0}}
	before := []any{int64(5), "old-name", "old@x"}
	after := []any{int64(5), "new-name", "new@x"}
	q, args := buildUpdate("c", "u", meta, before, after)
	want := "UPDATE `c`.`u` SET `id`=?, `name`=?, `email`=? WHERE `id`=? LIMIT 1"
	if q != want {
		t.Errorf("query=%q want %q", q, want)
	}
	if !reflect.DeepEqual(args, []any{int64(5), "new-name", "new@x", int64(5)}) {
		t.Errorf("args=%v", args)
	}
}

func TestQuoteIdentEscapesBackticks(t *testing.T) {
	if got := quoteIdent("a`b"); got != "`a``b`" {
		t.Errorf("got %q", got)
	}
}

func TestMetaCacheInvalidateScope(t *testing.T) {
	c := newMetaCache()
	c.put("dbA.t1", &tableMeta{})
	c.put("dbA.t2", &tableMeta{})
	c.put("dbB.t1", &tableMeta{})
	c.invalidate("dbA")
	if _, ok := c.get("dbA.t1"); ok {
		t.Error("dbA.t1 should have been invalidated")
	}
	if _, ok := c.get("dbB.t1"); !ok {
		t.Error("dbB.t1 should NOT have been invalidated")
	}
}

func TestIndicesOf(t *testing.T) {
	got := indicesOf([]string{"a", "b", "c"}, []string{"c", "a"})
	if !reflect.DeepEqual(got, []int{2, 0}) {
		t.Errorf("got %v want [2 0]", got)
	}
}

func TestBuildInsertSkipsGeneratedColumns(t *testing.T) {
	// `extension` is a STORED generated column on the Laravel `files`
	// table; MySQL rejects any explicit value for it with error 3105.
	// The replayer must drop it from both the column list and the
	// row-value projection.
	meta := &tableMeta{
		Columns:  []string{"id", "name", "extension"},
		PKCols:   []int{0},
		Writable: []int{0, 1},
	}
	q, args := buildInsert("c", "files", meta, []any{int64(1), "doc.pdf", "pdf"})
	wantQ := "INSERT INTO `c`.`files` (`id`, `name`) VALUES (?, ?)"
	if q != wantQ {
		t.Errorf("query=%q want %q", q, wantQ)
	}
	if !reflect.DeepEqual(args, []any{int64(1), "doc.pdf"}) {
		t.Errorf("args=%v want [1 doc.pdf]", args)
	}
}

func TestBuildUpdateSkipsGeneratedColumns(t *testing.T) {
	// UPDATE … SET on a generated column triggers the same MySQL
	// error 3105. The WHERE on PK is unaffected (PK indices point
	// into the full Columns slice).
	meta := &tableMeta{
		Columns:  []string{"id", "name", "extension"},
		PKCols:   []int{0},
		Writable: []int{0, 1},
	}
	before := []any{int64(5), "a.txt", "txt"}
	after := []any{int64(5), "a.pdf", "pdf"}
	q, args := buildUpdate("c", "files", meta, before, after)
	wantQ := "UPDATE `c`.`files` SET `id`=?, `name`=? WHERE `id`=? LIMIT 1"
	if q != wantQ {
		t.Errorf("query=%q want %q", q, wantQ)
	}
	if !reflect.DeepEqual(args, []any{int64(5), "a.pdf", int64(5)}) {
		t.Errorf("args=%v want [5 a.pdf 5]", args)
	}
}

func TestWritableMask(t *testing.T) {
	cols := []string{"id", "name", "extension"}
	got := writableMask(cols, []string{"extension"})
	if !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("got %v want [0 1]", got)
	}
	if writableMask(cols, nil) != nil {
		t.Error("nil generated → nil sentinel expected")
	}
}
