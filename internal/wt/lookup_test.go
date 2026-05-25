package wt

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

// withTempStore points TREEMAN_DB_PATH at a per-test temp sqlite,
// opens it, and returns the open *Store. The store is closed on
// test cleanup.
func withTempStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "treeman.db")
	t.Setenv("TREEMAN_DB_PATH", dbPath)
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestLookupWorktree(t *testing.T) {
	ctx := context.Background()
	st := withTempStore(t)
	repoRoot := t.TempDir()
	repoID, err := st.EnsureRepo(ctx, repoRoot, "myapp")
	if err != nil {
		t.Fatal(err)
	}
	mkWt := func(path, slug, branch string) {
		t.Helper()
		if _, err := st.EnsureWorktree(ctx, repoID, path, slug, branch); err != nil {
			t.Fatal(err)
		}
	}
	mkWt(filepath.Join(repoRoot, ".worktrees", "feature-x"), "app_feature-x", "feature/x")
	mkWt(filepath.Join(repoRoot, ".worktrees", "feature-y"), "app_feature-y", "feature/y")

	cases := []struct {
		name   string
		query  string
		wantOK bool
		want   string // basename when ok
	}{
		{"exact basename", "feature-x", true, "feature-x"},
		{"exact branch", "feature/x", true, "feature-x"},
		{"exact slug", "app_feature-x", true, "feature-x"},
		{"unambiguous prefix on basename", "feature-y", true, "feature-y"},
		{"ambiguous prefix yields no match", "feature-", false, ""},
		{"missing", "no-match", false, ""},
		{"single-char query never prefix-matches", "f", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := LookupWorktree(ctx, repoRoot, c.query, NoopSink{})
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v (got=%q)", ok, c.wantOK, got)
			}
			if ok && filepath.Base(got) != c.want {
				t.Errorf("basename(%s) = %q, want %q", got, filepath.Base(got), c.want)
			}
		})
	}
}

func TestLookupWorktreeIgnoresDeleted(t *testing.T) {
	ctx := context.Background()
	st := withTempStore(t)
	repoRoot := t.TempDir()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, "myapp")
	id, _ := st.EnsureWorktree(ctx, repoID, filepath.Join(repoRoot, ".worktrees", "gone"), "app_gone", "gone")
	if err := st.MarkWorktreeDeleted(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := LookupWorktree(ctx, repoRoot, "gone", NoopSink{}); ok {
		t.Error("deleted worktree should not be found")
	}
}
