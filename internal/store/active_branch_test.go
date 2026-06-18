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

// TestActiveBranchCleanColumns covers the lever-1 bookkeeping: a plain
// SetActiveBranch records clean=0 (the safe "must capture" default), a
// marker advance re-resets it, and SetActiveBranchClean round-trips the
// clean flag + watermark.
func TestActiveBranchCleanColumns(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openTestStoreWithWt(t)

	// No marker → not clean, no watermark.
	if clean, wm, ok, err := st.GetActiveBranchClean(ctx, wtID, "app"); err != nil || ok || clean || wm != "" {
		t.Fatalf("no marker: clean=%t wm=%q ok=%t err=%v", clean, wm, ok, err)
	}

	// SetActiveBranch always records clean=0 (safe default).
	if err := st.SetActiveBranch(ctx, repoID, wtID, "app", "develop", "mysql"); err != nil {
		t.Fatal(err)
	}
	if clean, wm, ok, _ := st.GetActiveBranchClean(ctx, wtID, "app"); !ok || clean || wm != "" {
		t.Fatalf("after SetActiveBranch: clean=%t wm=%q ok=%t, want clean=false", clean, wm, ok)
	}

	// Mark clean with a watermark.
	if err := st.SetActiveBranchClean(ctx, repoID, wtID, "app", "develop", "mysql", true, "wm:7"); err != nil {
		t.Fatal(err)
	}
	if clean, wm, _, _ := st.GetActiveBranchClean(ctx, wtID, "app"); !clean || wm != "wm:7" {
		t.Fatalf("after SetActiveBranchClean(true): clean=%t wm=%q, want true/wm:7", clean, wm)
	}

	// A marker advance (re-fill) must reset clean back to the safe default.
	if err := st.SetActiveBranch(ctx, repoID, wtID, "app", "feature", "mysql"); err != nil {
		t.Fatal(err)
	}
	if clean, wm, _, _ := st.GetActiveBranchClean(ctx, wtID, "app"); clean || wm != "" {
		t.Fatalf("marker advance must reset clean: clean=%t wm=%q", clean, wm)
	}

	// clean=false stores an empty watermark even if one is passed.
	if err := st.SetActiveBranchClean(ctx, repoID, wtID, "app", "feature", "mysql", false, "wm:9"); err != nil {
		t.Fatal(err)
	}
	if clean, wm, _, _ := st.GetActiveBranchClean(ctx, wtID, "app"); clean || wm != "" {
		t.Fatalf("clean=false must blank the watermark: clean=%t wm=%q", clean, wm)
	}
}
