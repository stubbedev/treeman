package store

import (
	"context"
	"testing"
)

func TestBranchMigratedRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, _, wtID := openTestStoreWithWt(t)

	// No record yet.
	if _, ok, err := st.GetBranchMigrated(ctx, wtID, "app", "develop"); err != nil || ok {
		t.Fatalf("expected no record, got ok=%t err=%v", ok, err)
	}

	// Set → read back.
	if err := st.SetBranchMigrated(ctx, wtID, "app", "develop", "fp1"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetBranchMigrated(ctx, wtID, "app", "develop")
	if err != nil || !ok || got != "fp1" {
		t.Fatalf("got=%q ok=%t err=%v, want fp1", got, ok, err)
	}

	// Per-branch isolation: a different branch is independent.
	if err := st.SetBranchMigrated(ctx, wtID, "app", "feature", "fp2"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetBranchMigrated(ctx, wtID, "app", "develop"); got != "fp1" {
		t.Errorf("develop fingerprint leaked: %q", got)
	}

	// Upsert overwrites the same (wt, db_key, branch).
	if err := st.SetBranchMigrated(ctx, wtID, "app", "develop", "fp1b"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetBranchMigrated(ctx, wtID, "app", "develop"); got != "fp1b" {
		t.Errorf("after upsert got %q, want fp1b", got)
	}

	// ClearForKey wipes every branch of one db_key.
	if err := st.ClearBranchMigratedForKey(ctx, wtID, "app"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetBranchMigrated(ctx, wtID, "app", "develop"); ok {
		t.Error("develop record should be cleared")
	}
	if _, ok, _ := st.GetBranchMigrated(ctx, wtID, "app", "feature"); ok {
		t.Error("feature record should be cleared by ForKey")
	}

	// ClearForWorktree wipes a second db_key too.
	if err := st.SetBranchMigrated(ctx, wtID, "other", "develop", "fp3"); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearBranchMigratedForWorktree(ctx, wtID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetBranchMigrated(ctx, wtID, "other", "develop"); ok {
		t.Error("ForWorktree should clear every db_key")
	}
}
