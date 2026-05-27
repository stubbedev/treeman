package store

import (
	"context"
	"testing"
)

func TestActiveBranchRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openTestStoreWithWt(t)

	// No marker yet.
	if _, ok, err := st.GetActiveBranch(ctx, wtID, "kontainer"); err != nil || ok {
		t.Fatalf("expected no marker, got ok=%t err=%v", ok, err)
	}

	// Set → read back.
	if err := st.SetActiveBranch(ctx, repoID, wtID, "kontainer", "develop", "mysql"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetActiveBranch(ctx, wtID, "kontainer")
	if err != nil || !ok || got != "develop" {
		t.Fatalf("got=%q ok=%t err=%v, want develop", got, ok, err)
	}

	// Upsert overwrites branch (same db_key).
	if err := st.SetActiveBranch(ctx, repoID, wtID, "kontainer", "feature/x", "mysql"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = st.GetActiveBranch(ctx, wtID, "kontainer")
	if got != "feature/x" {
		t.Errorf("after upsert got %q, want feature/x", got)
	}

	// A second db_key on the same worktree is independent.
	if err := st.SetActiveBranch(ctx, repoID, wtID, "mongodb_wt", "develop", "mongodb"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetActiveBranch(ctx, wtID, "kontainer"); got != "feature/x" {
		t.Errorf("kontainer marker leaked: %q", got)
	}

	// Clear one key.
	if err := st.ClearActiveBranch(ctx, wtID, "kontainer"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetActiveBranch(ctx, wtID, "kontainer"); ok {
		t.Error("kontainer marker should be cleared")
	}
	if _, ok, _ := st.GetActiveBranch(ctx, wtID, "mongodb_wt"); !ok {
		t.Error("mongodb_wt marker should survive a scoped clear")
	}

	// Clear all for worktree.
	if err := st.ClearActiveBranchesForWorktree(ctx, wtID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetActiveBranch(ctx, wtID, "mongodb_wt"); ok {
		t.Error("all markers should be cleared for the worktree")
	}
}
