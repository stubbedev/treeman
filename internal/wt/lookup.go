package wt

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/store"
)

// LookupWorktree queries SQLite for active worktrees attached to
// repoRoot and returns the path that best matches name:
//
//  1. exact basename match
//  2. exact branch match
//  3. exact slug match
//  4. unambiguous prefix match (basename or slug or branch)
//
// Multiple prefix hits are surfaced via the sink so the user can
// disambiguate; the function returns ("", false) in that case.
func LookupWorktree(ctx context.Context, repoRoot, name string, sink Sink) (string, bool) {
	if sink == nil {
		sink = NoopSink{}
	}
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return "", false
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return "", false
	}
	defer func() { _ = st.Close() }()
	rows, err := st.DB.QueryContext(ctx, `
		SELECT w.path, w.slug, COALESCE(w.branch,'')
		FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE w.deleted_at IS NULL AND r.path = ?`, repoRoot)
	if err != nil {
		return "", false
	}
	defer func() { _ = rows.Close() }()
	var exactBase, exactSlug, exactBranch string
	var prefixHits []string
	for rows.Next() {
		var path, slug, branch string
		if err := rows.Scan(&path, &slug, &branch); err != nil {
			continue
		}
		base := filepath.Base(path)
		switch {
		case base == name:
			exactBase = path
		case slug == name:
			exactSlug = path
		case branch == name:
			exactBranch = path
		case len(name) >= 2 && (strings.HasPrefix(base, name) || strings.HasPrefix(slug, name) || strings.HasPrefix(branch, name)):
			prefixHits = append(prefixHits, path)
		}
	}
	switch {
	case exactBase != "":
		return exactBase, true
	case exactBranch != "":
		return exactBranch, true
	case exactSlug != "":
		return exactSlug, true
	case len(prefixHits) == 1:
		return prefixHits[0], true
	case len(prefixHits) > 1:
		sink.Warn("ambiguous prefix match — candidates:")
		for _, p := range prefixHits {
			sink.Warn("  %s", p)
		}
		return "", false
	}
	return "", false
}

// IsMatchingExistingWorktree returns true when wtPath is already a
// linked worktree whose checked-out branch is `branch` AND a live
// (deleted_at IS NULL) registry row exists for it. Used by
// Create's idempotency guard so repeated invocations against an
// existing worktree are a no-op instead of an error.
func IsMatchingExistingWorktree(ctx context.Context, repoRoot, wtPath, branch string) bool {
	if !gitenv.IsLinkedWorktree(wtPath) {
		return false
	}
	current, err := gitcmd.String(ctx, wtPath, "branch", "--show-current")
	if err != nil || current != branch {
		return false
	}
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return false
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return false
	}
	defer func() { _ = st.Close() }()
	var n int
	_ = st.DB.QueryRowContext(ctx,
		`SELECT 1 FROM worktrees w JOIN repos r ON r.id = w.repo_id
		 WHERE r.path = ? AND w.path = ? AND w.deleted_at IS NULL`,
		repoRoot, wtPath).Scan(&n)
	return n == 1
}
