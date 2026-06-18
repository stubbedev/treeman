package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenAndMigrate(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// Re-opening should be a no-op.
	s2, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
}

func TestInheritedEnvRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	repoID, _ := s.EnsureRepo(ctx, "/repos/x", "x")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/wt/feat", "feat", "feat")

	// Empty load before any save.
	env, err := s.LoadInheritedEnv(ctx, wtID)
	if err != nil {
		t.Fatal(err)
	}
	if env != nil {
		t.Errorf("expected nil env before save, got %#v", env)
	}

	// Save + reload via id.
	want := map[string]string{
		"PATH":   "/usr/bin:/usr/local/bin:/home/me/.asdf/shims",
		"NVM":    "v18.19.0",
		"LANG":   "en_US.UTF-8",
		"AWKARD": `tab	and "quotes"`,
	}
	if err := s.SaveInheritedEnv(ctx, wtID, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadInheritedEnv(ctx, wtID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("len mismatch: got %d, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, got[k], v)
		}
	}

	// LoadByPath should match.
	gotByPath, err := s.LoadInheritedEnvByPath(ctx, "/wt/feat")
	if err != nil {
		t.Fatal(err)
	}
	if gotByPath["PATH"] != want["PATH"] {
		t.Errorf("by-path PATH mismatch: %q", gotByPath["PATH"])
	}

	// Empty map clears to NULL.
	if err := s.SaveInheritedEnv(ctx, wtID, nil); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.LoadInheritedEnv(ctx, wtID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != nil {
		t.Errorf("expected nil after clearing, got %#v", cleared)
	}
}

func TestEnsureRepoAndWorktree(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	repoID, err := s.EnsureRepo(ctx, "/repos/foo", "foo")
	if err != nil {
		t.Fatal(err)
	}
	repoID2, _ := s.EnsureRepo(ctx, "/repos/foo", "foo")
	if repoID != repoID2 {
		t.Errorf("expected idempotent: %d vs %d", repoID, repoID2)
	}

	wtID, err := s.EnsureWorktree(ctx, repoID, "/repos/foo/.worktrees/x", "proj_1", "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	if wtID == 0 {
		t.Fatal("zero wt id")
	}

	paths, err := s.ListRepoPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/repos/foo" {
		t.Errorf("unexpected: %v", paths)
	}
}

func TestRemoveRepoCascadesChildren(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	repoID, err := s.EnsureRepo(ctx, "/repos/foo", "foo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := s.EnsureWorktree(ctx, repoID, "/repos/foo/.worktrees/x", "proj_1", "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteEvent(ctx, LevelInfo, EvtWorktreeCreateStart, "hi",
		repoID, wtID, "", 0, nil); err != nil {
		t.Fatal(err)
	}

	// Refuses with active worktree.
	n, err := s.CountActiveWorktreesForRepo(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("active count: %d", n)
	}

	if err := s.MarkWorktreeDeleted(ctx, wtID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRepo(ctx, repoID); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}

	var repos int
	_ = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM repos").Scan(&repos)
	if repos != 0 {
		t.Errorf("repos remaining: %d", repos)
	}
	var wts int
	_ = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM worktrees").Scan(&wts)
	if wts != 0 {
		t.Errorf("worktrees remaining: %d", wts)
	}
	var evs int
	_ = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&evs)
	if evs != 0 {
		t.Errorf("events remaining: %d", evs)
	}
}

func TestWriteEvent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.WriteEvent(ctx, LevelInfo, EvtWorktreeCreateStart, "hi",
		0, 0, "", 0, map[string]string{"engine": "mysql"}); err != nil {
		t.Fatal(err)
	}
	var n int
	row := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM events")
	if err := row.Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("want 1 event, got %d", n)
	}
}
