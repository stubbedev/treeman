package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/wtreg"
)

// AutoFetchLoop runs a periodic `git fetch --all --prune` against
// every registered repo, then attempts a best-effort
// `git merge --ff-only @{u}` against every active worktree (main +
// linked). The cadence comes from the GLOBAL config's
// `auto_fetch.interval_minutes`; per-repo opt-out is honoured by
// consulting that repo's resolved config (`auto_fetch.enabled: false`).
//
// "Best effort" = every failure path logs a warn and continues:
//
//   - dirty working tree     → skip pull (would refuse anyway).
//   - detached HEAD          → skip pull (no @{u}).
//   - no upstream configured → skip pull.
//   - non-fast-forward       → skip pull (refuses to merge).
//   - fetch network failure  → skip pull, try again next tick.
//
// We never touch the user's local branches with a non-ff merge or
// rebase. The whole point of this loop is "keep remote refs warm +
// advance the working tree when it's trivially safe", not to resolve
// real divergence.
func AutoFetchLoop(ctx context.Context, st *State) {
	cfg, _ := resolve.LoadResolved("")
	if !cfg.AutoFetch.IsEnabled() {
		slog.Info("auto_fetch_loop disabled by global config")
		return
	}
	interval := time.Duration(cfg.AutoFetch.IntervalMinutes) * time.Minute
	if interval < time.Minute {
		// Defensive clamp — a misconfigured `interval_minutes: 0`
		// would otherwise spin a tight ticker that hammers every
		// remote on every tick.
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	slog.Info("auto_fetch_loop started", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runAutoFetchSweep(ctx, st)
		}
	}
}

// runAutoFetchSweep iterates every registered repo and dispatches
// fetch + ff-pull for that repo's worktrees. One bad repo logs and
// the sweep continues to the next.
func runAutoFetchSweep(ctx context.Context, st *State) {
	repos, err := st.Store.ListRepoRefs(ctx)
	if err != nil {
		slog.Warn("auto_fetch enumerate repos", "err", err)
		return
	}
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		// Per-repo opt-out. Resolved config is layered, so a repo's
		// own `.treeman.yaml` can flip `auto_fetch.enabled: false`
		// to skip just this repo while leaving the daemon-wide loop
		// running for everything else.
		cfg, err := resolve.LoadResolved(r.Path)
		if err != nil {
			slog.Warn("auto_fetch load cfg", "repo", r.Path, "err", err)
			continue
		}
		if !cfg.AutoFetch.IsEnabled() {
			slog.Debug("auto_fetch skip (repo disabled)", "repo", r.Path)
			continue
		}
		fetchAndPullRepo(ctx, st, r)
	}
}

// fetchAndPullRepo runs `git fetch --all --prune` against repoPath
// and then `git merge --ff-only @{u}` against every working tree
// attached to the repo (main + every linked worktree returned by
// wtreg.GitWorktreePaths).
func fetchAndPullRepo(ctx context.Context, st *State, r store.RepoRef) {
	if _, err := os.Stat(r.Path); err != nil {
		slog.Debug("auto_fetch skip (repo path missing)", "repo", r.Path, "err", err)
		return
	}
	// `--quiet` suppresses progress; stderr is captured into the
	// gitcmd.Error on failure. `--prune` clears tombstoned remote
	// branches so `git branch -r` doesn't accumulate ghosts.
	if err := gitcmd.Run(ctx, r.Path, "fetch", "--all", "--prune", "--quiet"); err != nil {
		// Network failures, auth failures, missing remote, etc. Log
		// and move on — next tick retries.
		slog.Warn("auto_fetch fetch failed", "repo", r.Path, "err", err)
		_ = st.Store.WriteEvent(ctx, store.LevelWarn, "auto_fetch_fetch_failed",
			err.Error(), r.ID, 0, "", 0, nil)
		return
	}

	// Build the full set of worktrees to attempt pull on: main repo
	// path first, then linked worktrees.
	paths := []string{r.Path}
	linked, err := wtreg.GitWorktreePaths(ctx, r.Path)
	if err != nil {
		slog.Warn("auto_fetch list linked worktrees", "repo", r.Path, "err", err)
	} else {
		paths = append(paths, linked...)
	}

	for _, wtPath := range paths {
		if ctx.Err() != nil {
			return
		}
		tryFFPull(ctx, st, r.ID, wtPath)
	}
}

// tryFFPull attempts a fast-forward merge of the upstream-tracking
// branch into the working tree's current HEAD. Every refusable
// pre-condition is checked inline so we can log a precise reason
// instead of relying on git's exit-1 to mean any of three things.
func tryFFPull(ctx context.Context, st *State, repoID int64, wtPath string) {
	if _, err := os.Stat(wtPath); err != nil {
		return
	}
	// Detached HEAD has no upstream — `symbolic-ref` exits non-zero.
	head, err := gitcmd.String(ctx, wtPath, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		slog.Debug("auto_fetch skip (detached HEAD or no branch)", "wt", wtPath)
		return
	}
	// Strip `refs/heads/` for the event payload below.
	branch := strings.TrimPrefix(head, "refs/heads/")

	// `@{u}` resolves to the upstream-tracking ref. Missing upstream
	// → exit 128 with "no upstream configured" on stderr.
	if !gitcmd.Exists(ctx, wtPath, "@{u}") {
		slog.Debug("auto_fetch skip (no upstream)", "wt", wtPath, "branch", branch)
		return
	}

	// Dirty index / working tree → skip. `--no-renames` is irrelevant
	// for the pure "is anything modified?" check; `-uno` keeps
	// untracked files out of the rename count since an untracked
	// build artefact shouldn't block a fast-forward.
	out, err := gitcmd.Output(ctx, wtPath, "status", "--porcelain=v1", "-uno")
	if err != nil {
		slog.Warn("auto_fetch status failed", "wt", wtPath, "err", err)
		return
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		slog.Debug("auto_fetch skip (dirty tree)", "wt", wtPath, "branch", branch)
		return
	}

	// merge --ff-only refuses (exit 128) when @{u} isn't a descendant
	// of HEAD. That's the divergence case — we never resolve it.
	if err := gitcmd.Run(ctx, wtPath, "merge", "--ff-only", "--quiet", "@{u}"); err != nil {
		var ge *gitcmd.Error
		if errors.As(err, &ge) && ge.ExitCode != 0 {
			slog.Debug("auto_fetch skip (non-ff)", "wt", wtPath, "branch", branch, "err", err)
			return
		}
		slog.Warn("auto_fetch ff-pull failed", "wt", wtPath, "err", err)
		return
	}
	slog.Info("auto_fetch ff-pull advanced", "wt", wtPath, "branch", branch)
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "auto_fetch_pulled",
		"fast-forward pull advanced "+branch,
		repoID, lookupWorktreeID(ctx, st, wtPath), "", 0, nil)
}

// lookupWorktreeID best-effort resolves wtPath → worktree row id for
// the event log. Returns 0 (the events table accepts it) when the
// path isn't registered — e.g. main worktree of a repo whose first
// `wt create` hasn't run yet.
func lookupWorktreeID(ctx context.Context, st *State, wtPath string) int64 {
	row, err := st.Store.LookupActiveWorktreeByPath(ctx, wtPath)
	if err != nil || row.ID == 0 {
		return 0
	}
	return row.ID
}
