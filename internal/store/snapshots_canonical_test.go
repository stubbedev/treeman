package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestRecordSnapshotCanonicalisesEngine asserts every alias treeman
// accepts under `databases[].engine` is collapsed to its canonical
// Family label when written to the snapshots row. Pre-canonical rows
// still work because every reader runs engine.Canonical, but new
// writes converge on one form so downstream callers don't have to
// re-canonicalise on every read.
func TestRecordSnapshotCanonicalisesEngine(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		write string
		want  string
	}{
		{"mysql", "mysql"},
		{"mariadb", "mysql"},
		{"tidb", "mysql"},
		{"postgres", "postgres"},
		{"postgresql", "postgres"},
		{"mongodb", "mongodb"},
		{"redis", "redis"},
		{"elasticsearch", "elasticsearch"},
		{"opensearch", "elasticsearch"},
	}
	for _, c := range cases {
		t.Run(c.write, func(t *testing.T) {
			fp := "fp_" + c.write
			err := st.RecordSnapshot(ctx, SnapshotRecord{
				Fingerprint: fp, Engine: c.write, EngineVersion: "x",
				SourceDB: "src", TemplateName: "tpl",
				LastUsedAt: 1, RepoID: repoID,
			})
			if err != nil {
				t.Fatalf("record: %v", err)
			}
			rec, err := st.LookupSnapshot(ctx, fp)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if rec == nil {
				t.Fatal("nil rec")
			}
			if rec.Engine != c.want {
				t.Errorf("alias %q -> stored engine %q, want %q", c.write, rec.Engine, c.want)
			}
		})
	}
}

// TestRecordSnapshotPreservesUnknownEngine ensures the canonical
// normalisation only rewrites recognised aliases — typos and
// not-yet-supported engines are stored as-is so the downstream
// `unsupported engine %q` error message still points at the
// original input.
func TestRecordSnapshotPreservesUnknownEngine(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	err = st.RecordSnapshot(ctx, SnapshotRecord{
		Fingerprint: "fp_sqlite", Engine: "sqlite",
		SourceDB: "src", TemplateName: "tpl",
		LastUsedAt: 1, RepoID: repoID,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	rec, _ := st.LookupSnapshot(ctx, "fp_sqlite")
	if rec.Engine != "sqlite" {
		t.Errorf("unknown engine rewritten: stored %q, want sqlite", rec.Engine)
	}
}
