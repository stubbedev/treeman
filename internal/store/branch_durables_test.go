package store

import (
	"context"
	"testing"
)

func TestBranchDurablesRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openTestStoreWithWt(t)

	// Empty pool reads as an empty (non-nil) slice.
	if got, err := st.ListBranchDurables(ctx, repoID); err != nil || len(got) != 0 {
		t.Fatalf("empty pool: got %d rows err=%v", len(got), err)
	}

	// Record two durables for the same worktree, distinct branches.
	rows := []BranchDurableRow{
		{RepoID: repoID, WorktreeID: wtID, Engine: "elasticsearch", DBKey: "kho_x", Branch: "develop", DurableName: "tmbs_aaa_"},
		{RepoID: repoID, WorktreeID: wtID, Engine: "mysql", DBKey: "kontainer_x", Branch: "feature/y", DurableName: "_tmbs_bbb"},
	}
	for _, r := range rows {
		if err := st.RecordBranchDurable(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.ListBranchDurables(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	// Ordered by branch: "develop" before "feature/y".
	if got[0].Branch != "develop" || got[0].DurableName != "tmbs_aaa_" {
		t.Errorf("row0 = %+v, want develop/tmbs_aaa_", got[0])
	}
	if got[0].WorktreeID != wtID || got[0].DBKey != "kho_x" {
		t.Errorf("row0 provenance = wt:%d db_key:%s", got[0].WorktreeID, got[0].DBKey)
	}

	// Upsert by (repo, durable_name): same name, new branch/db_key.
	if err := st.RecordBranchDurable(ctx, BranchDurableRow{
		RepoID: repoID, WorktreeID: wtID, Engine: "elasticsearch",
		DBKey: "kho_z", Branch: "feature/renamed", DurableName: "tmbs_aaa_",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ListBranchDurables(ctx, repoID)
	if len(got) != 2 {
		t.Fatalf("upsert grew the pool to %d rows", len(got))
	}

	// Delete by name is idempotent.
	if err := st.DeleteBranchDurable(ctx, repoID, "tmbs_aaa_"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteBranchDurable(ctx, repoID, "tmbs_aaa_"); err != nil {
		t.Fatalf("second delete should be a no-op, got %v", err)
	}
	got, _ = st.ListBranchDurables(ctx, repoID)
	if len(got) != 1 || got[0].DurableName != "_tmbs_bbb" {
		t.Fatalf("after delete got %+v, want only _tmbs_bbb", got)
	}
}

func TestBranchDurablesEngineCanonicalised(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openTestStoreWithWt(t)

	// A driver alias canonicalises to its Family label on write, matching
	// the snapshots-table convention.
	if err := st.RecordBranchDurable(ctx, BranchDurableRow{
		RepoID: repoID, WorktreeID: wtID, Engine: "mariadb",
		DBKey: "kontainer_x", Branch: "develop", DurableName: "_tmbs_ccc",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListBranchDurables(ctx, repoID)
	if len(got) != 1 || got[0].Engine != "mysql" {
		t.Fatalf("engine = %q, want canonical mysql", got[0].Engine)
	}
}
