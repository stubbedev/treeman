package mcp

import (
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

func TestAssertReadOnlySQL_Accepts(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"select * from users",
		"  SHOW TABLES",
		"EXPLAIN SELECT * FROM users",
		"DESCRIBE users",
		"DESC users",
		"WITH cte AS (SELECT 1) SELECT * FROM cte",
	}
	for _, q := range cases {
		if err := assertReadOnlySQL(q); err != nil {
			t.Errorf("expected %q to be accepted, got %v", q, err)
		}
	}
}

func TestAssertReadOnlySQL_Rejects(t *testing.T) {
	cases := []string{
		"INSERT INTO users VALUES (1)",
		"UPDATE users SET x = 1",
		"DELETE FROM users",
		"DROP TABLE users",
		"TRUNCATE users",
		"CREATE TABLE x (id INT)",
		"GRANT SELECT ON users TO bob",
		"",
		"   ",
	}
	for _, q := range cases {
		if err := assertReadOnlySQL(q); err == nil {
			t.Errorf("expected %q to be rejected, got nil err", q)
		}
	}
}

func TestAssertReadOnlyRedis_Accepts(t *testing.T) {
	cases := []string{
		"GET key",
		"get key", // case-insensitive
		"SCAN 0 MATCH foo:*",
		"DBSIZE",
		"PING",
		"INFO replication",
		"TYPE somekey",
	}
	for _, q := range cases {
		if err := assertReadOnlyRedis(q); err != nil {
			t.Errorf("expected %q to be accepted, got %v", q, err)
		}
	}
}

func TestAssertReadOnlyRedis_Rejects(t *testing.T) {
	cases := []string{
		"SET key value",
		"DEL key",
		"FLUSHDB",
		"FLUSHALL",
		"RENAME a b",
		"LPUSH q x",
		"HSET h k v",
	}
	for _, q := range cases {
		if err := assertReadOnlyRedis(q); err == nil {
			t.Errorf("expected %q to be rejected, got nil", q)
		}
	}
}

func TestFirstWord(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM x": "SELECT",
		"  SELECT 1":      "SELECT",
		"GET key":         "GET",
		"single":          "single",
		"":                "",
		"   ":             "",
		"a\tb":            "a",
		"a\nb":            "a",
	}
	for in, want := range cases {
		got := firstWord(in)
		if got != want {
			t.Errorf("firstWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSnapshotRecordToMap_AllFieldsExposed(t *testing.T) {
	rec := &store.SnapshotRecord{
		Fingerprint:    "fp",
		Engine:         "mysql",
		EngineVersion:  "8.0.36",
		SourceDB:       "src",
		TemplateName:   "tpl",
		MigrationsHash: "m",
		DumpHash:       "d",
		LockfileHashes: map[string]string{"composer.lock": "abc"},
		SizeBytes:      1234,
		CreatedAt:      1,
		LastUsedAt:     2,
		UseCount:       3,
		RepoID:         42,
	}
	got := snapshotRecordToMap(rec)
	// Sanity-check that every field round-trips. If a future refactor
	// reorders SnapshotRecord and forgets to update this map, the
	// missing field shows up here.
	want := map[string]any{
		"fingerprint":     "fp",
		"engine":          "mysql",
		"engine_version":  "8.0.36",
		"source_db":       "src",
		"template_name":   "tpl",
		"migrations_hash": "m",
		"dump_hash":       "d",
		"lockfile_hashes": map[string]string{"composer.lock": "abc"},
		"repo_id":         int64(42),
		"size_bytes":      int64(1234),
		"created_at":      int64(1),
		"last_used_at":    int64(2),
		"use_count":       int64(3),
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("missing key %q in output map", k)
			continue
		}
		// Map equality is awkward with mixed types — fall back to
		// fmt-style compare so any drift surfaces clearly.
		if !mapsEqual(v, gv) {
			t.Errorf("%s: want %v, got %v", k, v, gv)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected extra keys: got=%v want=%v", keys(got), keys(want))
	}
}

func TestCoalescePort(t *testing.T) {
	if coalescePort(0, 3306) != 3306 {
		t.Errorf("zero port should fall back to default")
	}
	if coalescePort(5432, 3306) != 5432 {
		t.Errorf("non-zero port should win")
	}
}

func mapsEqual(a, b any) bool {
	ma, ok1 := a.(map[string]string)
	mb, ok2 := b.(map[string]string)
	if ok1 && ok2 {
		if len(ma) != len(mb) {
			return false
		}
		for k, v := range ma {
			if mb[k] != v {
				return false
			}
		}
		return true
	}
	return a == b
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Compile-time guard: keep the linter happy if strings goes unused
// when the test file shrinks during a future refactor.
var _ = strings.TrimSpace
