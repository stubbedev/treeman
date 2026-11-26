package store

import (
	"context"
	"path/filepath"
	"reflect"
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

	if _, ok, err := st.GetTemplateBuilt(ctx, wtID, "db_a", "mysql", ""); err != nil || ok {
		t.Fatalf("absent row: ok=%v err=%v, want false nil", ok, err)
	}
	first := TemplateBuilt{Fingerprint: "fp1", Watermarks: map[string]string{"db_a": "ps:db_a:7", "db_a_test_1": "ps:db_a_test_1:3"}}
	if err := st.SetTemplateBuilt(ctx, wtID, "db_a", "mysql", "", first); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := st.GetTemplateBuilt(ctx, wtID, "db_a", "mysql", ""); err != nil || !ok || !reflect.DeepEqual(got, first) {
		t.Fatalf("after set: got=%+v ok=%v err=%v, want %+v true nil", got, ok, err, first)
	}
	// Upsert overwrites the fingerprint and replaces every watermark.
	if err := st.SetTemplateBuilt(ctx, wtID, "db_a", "mysql", "", TemplateBuilt{Fingerprint: "fp2"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetTemplateBuilt(ctx, wtID, "db_a", "mysql", ""); got.Fingerprint != "fp2" || len(got.Watermarks) != 0 {
		t.Fatalf("after upsert: got=%+v, want fp2 with no watermarks", got)
	}
	// Different engine / key are independent.
	if err := st.SetTemplateBuilt(ctx, wtID, "db_a", "postgres", "", TemplateBuilt{Fingerprint: "fpP"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTemplateBuilt(ctx, wtID, "db_b", "mysql", "", TemplateBuilt{Fingerprint: "fpB"}); err != nil {
		t.Fatal(err)
	}

	if err := st.ClearTemplateBuiltForKey(ctx, wtID, "db_a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetTemplateBuilt(ctx, wtID, "db_a", "mysql", ""); ok {
		t.Error("db_a/mysql should be cleared")
	}
	if _, ok, _ := st.GetTemplateBuilt(ctx, wtID, "db_a", "postgres", ""); ok {
		t.Error("db_a/postgres should be cleared (key-scoped)")
	}
	if _, ok, _ := st.GetTemplateBuilt(ctx, wtID, "db_b", "mysql", ""); !ok {
		t.Error("db_b/mysql must survive a db_a clear")
	}

	if err := st.ClearTemplateBuiltForWorktree(ctx, wtID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetTemplateBuilt(ctx, wtID, "db_b", "mysql", ""); ok {
		t.Error("worktree-wide clear must remove every row")
	}
}

// TestTakeTemplateBuilt: taking the record returns what was set and
// deletes it, so a second take (and a get) find nothing.
func TestTakeTemplateBuilt(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	wtID, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/a", "a", "a")

	if _, ok, err := st.TakeTemplateBuilt(ctx, wtID, "db_a", "mysql", ""); err != nil || ok {
		t.Fatalf("absent row: ok=%v err=%v, want false nil", ok, err)
	}
	want := TemplateBuilt{Fingerprint: "fp1", Watermarks: map[string]string{"db_a": "pg:4"}}
	if err := st.SetTemplateBuilt(ctx, wtID, "db_a", "postgres", "", want); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTemplateBuilt(ctx, wtID, "db_b", "postgres", "", TemplateBuilt{Fingerprint: "fpB"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.TakeTemplateBuilt(ctx, wtID, "db_a", "postgres", "")
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("take: got=%+v ok=%v err=%v, want %+v true nil", got, ok, err, want)
	}
	if _, ok, err := st.TakeTemplateBuilt(ctx, wtID, "db_a", "postgres", ""); err != nil || ok {
		t.Fatalf("second take: ok=%v err=%v, want false nil", ok, err)
	}
	if _, ok, _ := st.GetTemplateBuilt(ctx, wtID, "db_b", "postgres", ""); !ok {
		t.Error("db_b must survive taking db_a")
	}
}
