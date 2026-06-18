package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestLookupWorktreeIDAmbiguousSlug covers the issue #12 repro: two
// active worktrees on the same ticket collapse to the same slug,
// `kon_12568`. The historic lookup silently returned the newest id —
// or none at all when LIMIT 1 picked a row in a different repo
// scope. The fixed lookup must surface the candidates instead so the
// caller can render them.
func TestLookupWorktreeIDAmbiguousSlug(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	repoID, _ := s.EnsureRepo(ctx, "/r", "r")
	if _, err := s.EnsureWorktree(ctx, repoID, "/r/.worktrees/bugfix/KON-12568", "kon_12568", "bugfix/KON-12568"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureWorktree(ctx, repoID, "/r/.worktrees/hotfix/KON-12568", "kon_12568", "hotfix/KON-12568"); err != nil {
		t.Fatal(err)
	}

	id, err := s.LookupWorktreeID(ctx, repoID, "kon_12568")
	if id != 0 {
		t.Errorf("ambiguous slug must not collapse to a single id; got %d", id)
	}
	if err == nil {
		t.Fatal("expected ambiguity error for duplicate slug")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention 'ambiguous', got: %v", err)
	}
	if !strings.Contains(err.Error(), "bugfix/KON-12568") || !strings.Contains(err.Error(), "hotfix/KON-12568") {
		t.Errorf("error should list both candidate branches, got: %v", err)
	}
}

// TestLookupWorktreeIDBranchWinsOverDuplicateSlug verifies that a
// unique branch lookup resolves cleanly even when the same name's
// slug collides on multiple rows. Branches are unique within an
// active set; the rank logic must prefer them over slugs.
func TestLookupWorktreeIDBranchWinsOverDuplicateSlug(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	repoID, _ := s.EnsureRepo(ctx, "/r", "r")
	bugID, _ := s.EnsureWorktree(ctx, repoID, "/r/.worktrees/bugfix/KON-12568", "kon_12568", "bugfix/KON-12568")
	hotID, _ := s.EnsureWorktree(ctx, repoID, "/r/.worktrees/hotfix/KON-12568", "kon_12568", "hotfix/KON-12568")

	got, err := s.LookupWorktreeID(ctx, repoID, "hotfix/KON-12568")
	if err != nil {
		t.Fatalf("unique branch lookup must not error: %v", err)
	}
	if got != hotID {
		t.Errorf("got id=%d, want %d (hotfix)", got, hotID)
	}
	// Sanity: the bug row is still findable via its own branch.
	got, err = s.LookupWorktreeID(ctx, repoID, "bugfix/KON-12568")
	if err != nil || got != bugID {
		t.Errorf("bugfix lookup: got id=%d err=%v, want id=%d", got, err, bugID)
	}
}

// TestLookupWorktreeIDDeletedRowsIgnored confirms that the lookup
// only considers active rows. A historic soft-deleted row sharing a
// slug with one active row should not produce a false ambiguity.
func TestLookupWorktreeIDDeletedRowsIgnored(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	repoID, _ := s.EnsureRepo(ctx, "/r", "r")
	deadID, _ := s.EnsureWorktree(ctx, repoID, "/r/.worktrees/old", "kon_12568", "feature/KON-12568-old")
	if err := s.MarkWorktreeDeleted(ctx, deadID); err != nil {
		t.Fatal(err)
	}
	liveID, _ := s.EnsureWorktree(ctx, repoID, "/r/.worktrees/new", "kon_12568", "feature/KON-12568-new")

	got, err := s.LookupWorktreeID(ctx, repoID, "kon_12568")
	if err != nil {
		t.Fatalf("expected unique match after deleted row excluded, got error: %v", err)
	}
	if got != liveID {
		t.Errorf("got id=%d, want %d", got, liveID)
	}
}
