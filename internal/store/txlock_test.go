package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestReadThenWriteTransactionWaitsForConcurrentWriter(t *testing.T) {
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
	wtID, err := st.EnsureWorktree(ctx, repoID, "/tmp/repo/a", "a", "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetTemplateBuilt(ctx, wtID, "db_a", "mongodb", "", TemplateBuilt{Fingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}

	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, ok, err := readTemplateBuilt(ctx, tx, wtID, "db_a", "mongodb", ""); err != nil || !ok {
		t.Fatalf("read inside the transaction: ok=%v err=%v, want true nil", ok, err)
	}

	concurrent := make(chan error, 1)
	go func() {
		concurrent <- st.SetTemplateBuilt(ctx, wtID, "db_b", "mongodb", "", TemplateBuilt{Fingerprint: "fp"})
	}()
	select {
	case err := <-concurrent:
		concurrent <- err
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM template_db_built WHERE worktree_id = ? AND db_key = ?`, wtID, "db_a"); err != nil {
		t.Fatalf("write after a concurrent commit: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-concurrent; err != nil {
		t.Fatalf("concurrent writer: %v", err)
	}
	if _, ok, _ := st.GetTemplateBuilt(ctx, wtID, "db_b", "mongodb", ""); !ok {
		t.Fatal("concurrent writer's record must be stored")
	}
}
