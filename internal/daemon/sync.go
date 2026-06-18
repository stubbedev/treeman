package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/wtreg"
)

// SyncNow drives an on-demand fetch + advance for the targeted scope:
//
//   - empty target → every registered repo (mirrors the auto-fetch
//     sweep, but bypasses the backoff gate so the user can punch
//     through an offline-mode pause).
//   - repo path    → that repo only.
//   - worktree path under a repo → the repo's fetch runs, then only
//     the named worktree is advanced.
//
// Returns a per-repo status snapshot (last_fetch, ahead/behind, etc.)
// plus a flat list of error strings (one per failed repo). Empty
// errors slice means "every repo synced cleanly".
func SyncNow(ctx context.Context, st *State, target string) ([]rpc.SyncRepoStatus, []string) {
	repos, err := selectReposForTarget(ctx, st, target)
	if err != nil {
		return nil, []string{err.Error()}
	}
	if len(repos) == 0 {
		return nil, []string{fmt.Sprintf("no registered repo matches %q", target)}
	}

	wtFilter := ""
	if target != "" {
		// Resolve target to its repo root. If target == repo root the
		// filter is empty (every wt is in scope). If target is deeper
		// (a worktree path), narrow to that path. A failed abs is an
		// explicit error — silently expanding scope to the whole repo
		// would surprise the caller with an unintended write surface.
		abs, err := filepath.Abs(target)
		if err != nil {
			return nil, []string{fmt.Sprintf("resolve target %q: %v", target, err)}
		}
		if !isSameRepoRoot(abs, repos) {
			wtFilter = abs
		}
	}

	var errs []string
	for _, r := range repos {
		cfg, err := resolve.LoadResolved(r.Path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: load cfg: %v", r.Path, err))
			continue
		}
		// Manual sync_now ignores per-repo opt-out *and* the backoff
		// gate — the call is an explicit override.
		if wtFilter == "" {
			if err := SyncRepo(ctx, st, r, &cfg); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", r.Path, err))
			}
			continue
		}
		// Targeted worktree: run fetch once, advance only the named wt.
		if err := gitcmd.Run(ctx, r.Path, "fetch", "--all", "--prune", "--quiet"); err != nil {
			st.RecordSyncFailure(r.Path, backoffFor(st.SyncFailCount(r.Path)+1))
			errs = append(errs, fmt.Sprintf("%s: fetch: %v", r.Path, err))
			continue
		}
		st.RecordSyncSuccess(r.Path)
		_ = SyncWorktree(ctx, st, r.ID, wtFilter, cfg.AutoFetch.ResolvedMode())
	}
	// Snapshot only the repos this call touched so the response shape
	// matches the request scope. Passing "" here would leak status for
	// every registered repo into a targeted sync_now's reply.
	out := make([]rpc.SyncRepoStatus, 0, len(repos))
	for _, r := range repos {
		out = append(out, SyncStatusSnapshot(ctx, st, r.Path)...)
	}
	return out, errs
}

// selectReposForTarget maps a sync_now target string to the set of
// registered repos that should fetch. Resolution order:
//
//   - "" → every repo.
//   - matches a repo path exactly → just that repo.
//   - is inside a registered repo (a worktree path) → that repo.
//   - otherwise → empty (caller surfaces "no match" error).
func selectReposForTarget(ctx context.Context, st *State, target string) ([]store.RepoRef, error) {
	all, err := st.Store.ListRepoRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate repos: %w", err)
	}
	if target == "" {
		return all, nil
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil, fmt.Errorf("resolve target: %w", err)
	}
	// Exact repo match first.
	for _, r := range all {
		if r.Path == abs {
			return []store.RepoRef{r}, nil
		}
	}
	// Try worktree → owning repo. A worktree path lives under the
	// repo dir for the main worktree, or under .worktrees/... for a
	// linked worktree; both cases resolve via SQLite.
	if row, err := st.Store.LookupActiveWorktreeByPath(ctx, abs); err == nil && row.ID != 0 {
		for _, r := range all {
			if r.ID == row.RepoID {
				return []store.RepoRef{r}, nil
			}
		}
	}
	// Last resort: a path that's *inside* a registered repo dir but
	// not in the registry (caller passed a deep subdir). Pick the
	// longest matching repo root.
	var best store.RepoRef
	for _, r := range all {
		if strings.HasPrefix(abs+string(filepath.Separator), r.Path+string(filepath.Separator)) {
			if len(r.Path) > len(best.Path) {
				best = r
			}
		}
	}
	if best.ID != 0 {
		return []store.RepoRef{best}, nil
	}
	return nil, nil
}

// isSameRepoRoot returns true when abs matches one of the supplied
// repo roots exactly. Used to tell "target == repo root, sync every
// wt" apart from "target is one specific worktree".
func isSameRepoRoot(abs string, repos []store.RepoRef) bool {
	for _, r := range repos {
		if r.Path == abs {
			return true
		}
	}
	return false
}

// SyncStatusSnapshot returns the current sync state for every
// registered repo (or just one, when repoFilter is non-empty). For
// each repo it also lists every linked worktree's ahead/behind/dirty
// counts plus the last skip reason recorded by the auto-fetch sweep.
func SyncStatusSnapshot(ctx context.Context, st *State, repoFilter string) []rpc.SyncRepoStatus {
	repos, err := st.Store.ListRepoRefs(ctx)
	if err != nil {
		return nil
	}
	out := make([]rpc.SyncRepoStatus, 0, len(repos))
	for _, r := range repos {
		if repoFilter != "" && r.Path != repoFilter {
			continue
		}
		cfg, _ := resolve.LoadResolved(r.Path)
		rs := rpc.SyncRepoStatus{
			RepoPath:       r.Path,
			Mode:           cfg.AutoFetch.ResolvedMode(),
			ConsecFailures: st.SyncFailCount(r.Path),
		}
		if t := st.SyncLastFetch(r.Path); !t.IsZero() {
			rs.LastFetchUnix = t.Unix()
		}
		if t := st.SyncBackoffUntil(r.Path); !t.IsZero() {
			rs.NextRetryUnix = t.Unix()
		}

		paths := []string{r.Path}
		linked, _ := wtreg.GitWorktreePaths(ctx, r.Path)
		paths = append(paths, linked...)
		// Probe worktrees concurrently: each worktreeSyncStatus spawns 3
		// git subprocesses and they're fully independent across worktrees,
		// so serial probing made the status RPC O(N×3) in wall-clock. Bound
		// the fanout and write distinct slice indices to preserve order.
		rs.Worktrees = make([]rpc.SyncWorktreeStatus, len(paths))
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(8)
		for i, p := range paths {
			g.Go(func() error {
				rs.Worktrees[i] = worktreeSyncStatus(gctx, st, p)
				return nil
			})
		}
		_ = g.Wait()
		out = append(out, rs)
	}
	return out
}

// worktreeSyncStatus probes one working tree for ahead/behind counts
// against `@{u}` plus a dirty flag, joined with the last skip reason
// recorded on State. Returns a zeroed entry for worktrees with no
// upstream — those rows surface via LastSkipReason instead.
func worktreeSyncStatus(ctx context.Context, st *State, wtPath string) rpc.SyncWorktreeStatus {
	out := rpc.SyncWorktreeStatus{
		Path:           wtPath,
		LastSkipReason: st.SyncLastSkip(wtPath),
	}
	if head, err := gitcmd.String(ctx, wtPath, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		out.Branch = strings.TrimSpace(strings.TrimPrefix(head, "refs/heads/"))
	}
	// `git rev-list --count --left-right HEAD...@{u}` emits one line
	// "ahead<tab>behind". Errors here typically mean no upstream — we
	// leave the counts at zero and let LastSkipReason explain.
	out2, err := gitcmd.Output(ctx, wtPath, "rev-list", "--count", "--left-right", "HEAD...@{u}")
	if err == nil {
		fields := strings.Fields(strings.TrimSpace(string(out2)))
		if len(fields) == 2 {
			if a, err := strconv.Atoi(fields[0]); err == nil {
				out.Ahead = a
			}
			if b, err := strconv.Atoi(fields[1]); err == nil {
				out.Behind = b
			}
		}
	}
	// Dirty check matches the auto-fetch loop's gate so the column
	// answers "would auto-fetch advance this on the next tick?".
	if dirty, err := gitcmd.Output(ctx, wtPath, "status", "--porcelain=v1", "-uno"); err == nil {
		if len(strings.TrimSpace(string(dirty))) > 0 {
			out.Dirty = true
		}
	}
	return out
}
