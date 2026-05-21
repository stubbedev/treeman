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
