package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTemplateBuiltRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	wtID, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/a", "a", "a")

	if _, ok, err := st.GetTemplateBuilt(ctx, wtID, "db_a", "mysql"); err != nil || ok {
		t.Fatalf("absent row: ok=%v err=%v, want false nil", ok, err)
	}
	if err := st.SetTemplateBuilt(ctx, wtID, "db_a", "mysql", "fp1"); err != nil {
		t.Fatal(err)
	}
	if fp, ok, err := st.GetTemplateBuilt(ctx, wtID, "db_a", "mysql"); err != nil || !ok || fp != "fp1" {
		t.Fatalf("after set: fp=%q ok=%v err=%v, want fp1 true nil", fp, ok, err)
	}
	// Upsert overwrites.
	if err := st.SetTemplateBuilt(ctx, wtID, "db_a", "mysql", "fp2"); err != nil {
		t.Fatal(err)
	}
	if fp, _, _ := st.GetTemplateBuilt(ctx, wtID, "db_a", "mysql"); fp != "fp2" {
		t.Fatalf("after upsert: fp=%q, want fp2", fp)
	}
	// Different engine / key are independent.
	if err := st.SetTemplateBuilt(ctx, wtID, "db_a", "postgres", "fpP"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTemplateBuilt(ctx, wtID, "db_b", "mysql", "fpB"); err != nil {
		t.Fatal(err)
	}

	if err := st.ClearTemplateBuiltForKey(ctx, wtID, "db_a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetTemplateBuilt(ctx, wtID, "db_a", "mysql"); ok {
		t.Error("db_a/mysql should be cleared")
	}
	if _, ok, _ := st.GetTemplateBuilt(ctx, wtID, "db_a", "postgres"); ok {
		t.Error("db_a/postgres should be cleared (key-scoped)")
	}
	if _, ok, _ := st.GetTemplateBuilt(ctx, wtID, "db_b", "mysql"); !ok {
		t.Error("db_b/mysql must survive a db_a clear")
	}

	if err := st.ClearTemplateBuiltForWorktree(ctx, wtID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetTemplateBuilt(ctx, wtID, "db_b", "mysql"); ok {
		t.Error("worktree-wide clear must remove every row")
	}
}
