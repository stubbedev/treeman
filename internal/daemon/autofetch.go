package daemon

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/wtreg"
)

// Skip-reason codes emitted as `auto_fetch_skipped` events and stamped
// onto `State.syncLastSkip` so the sync_status RPC can answer "why
// didn't this branch advance on the last sweep?".
const (
	SyncSkipDirty      = "dirty"
	SyncSkipNoUpstream = "no_upstream"
	SyncSkipDetached   = "detached_head"
	SyncSkipNonFF      = "non_ff"
	SyncSkipPathGone   = "path_missing"
	SyncSkipRebaseFail = "rebase_conflict"
)

// Backoff curve for repeated fetch failures. Doubling exponential from
// 1 min up to 1 h. Capping at 1 h prevents an offline laptop from
// retrying every 15 min forever; resuming after the cap is still fast
// enough that a returning network is noticed within an hour.
var syncBackoffSchedule = []time.Duration{
	1 * time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
	1 * time.Hour,
}

// backoffFor maps a 1-indexed consecutive-failure count to the next
// retry delay. Values past the schedule's length stay at the final
// (cap) value.
func backoffFor(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	idx := failures - 1
	if idx >= len(syncBackoffSchedule) {
		idx = len(syncBackoffSchedule) - 1
	}
	return syncBackoffSchedule[idx]
}

// repoJitter returns a deterministic per-repo offset in the range
// [-30s, +30s]. Hashing the repo path keeps the offset stable across
// restarts — important so a repo that has been offline doesn't keep
// shifting its retry window each daemon boot.
func repoJitter(repoPath string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(repoPath))
	// Map [0, 2^32) → [-30s, +30s].
	const span = int64(60 * time.Second)
	off := int64(h.Sum32())%span - span/2
	return time.Duration(off)
}

// autoFetchInterval maps the configured `auto_fetch.interval_minutes`
// to a ticker duration, clamping sub-minute values up to 1m so a
// misconfigured `interval_minutes: 0` can't spin a tight ticker that
// hammers every remote on every tick.
func autoFetchInterval(cfg config.AutoFetchConfig) time.Duration {
	d := time.Duration(cfg.IntervalMinutes) * time.Minute
	if d < time.Minute {
		d = time.Minute
	}
	return d
}

// AutoFetchLoop runs a periodic `git fetch --all --prune` against
// every registered repo, then advances every active worktree (main +
// linked) by either fast-forward or rebase depending on
// `auto_fetch.mode`. The cadence comes from the GLOBAL config's
// `auto_fetch.interval_minutes`; per-repo opt-out is honoured by
// consulting that repo's resolved config (`auto_fetch.enabled: false`).
//
// Every failure path skips and logs:
//
//   - dirty working tree     → skip pull (would refuse anyway).
//   - detached HEAD          → skip pull (no @{u}).
//   - no upstream configured → skip pull.
//   - non-fast-forward (ff)  → skip pull (refuses to merge).
//   - rebase conflict        → abort + skip.
//   - fetch network failure  → exponential backoff per repo.
//
// In ff mode the user's local branch is never touched on divergence.
// In rebase mode `--autostash` shields dirty trees and the rebase is
// aborted on conflict before any partial state lands. Either way the
// loop never resolves real divergence on the user's behalf.
func AutoFetchLoop(ctx context.Context, st *State) {
	cfg, _ := resolve.LoadResolved("")
	if !cfg.AutoFetch.IsEnabled() {
		slog.Info("auto_fetch_loop disabled by global config")
		return
	}
	interval := autoFetchInterval(cfg.AutoFetch)
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
// fetch + advance for that repo's worktrees. One bad repo logs and
// the sweep continues to the next.
//
// Each repo is skipped if its backoff window hasn't elapsed (set by
// a prior fetch failure). Jitter shifts the eligibility check by a
// deterministic per-repo offset so all repos don't hit the same tick
// in lockstep.
func runAutoFetchSweep(ctx context.Context, st *State) {
	repos, err := st.Store.ListRepoRefs(ctx)
	if err != nil {
		slog.Warn("auto_fetch enumerate repos", "err", err)
		return
	}
	now := time.Now()
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
		// Backoff gate: an offline / auth-failing repo is parked for
		// up to 1h, with jitter so the unlock time isn't perfectly
		// aligned across repos.
		until := st.SyncBackoffUntil(r.Path)
		if !until.IsZero() && now.Add(repoJitter(r.Path)).Before(until) {
			slog.Debug("auto_fetch skip (backoff)", "repo", r.Path, "until", until)
			continue
		}
		_ = SyncRepo(ctx, st, r, &cfg)
	}
}

// SyncRepo runs `git fetch --all --prune` against r.Path and then
// advances every working tree attached to the repo. Returns the
// fetch error (nil on success) so callers driving an on-demand sync
// can report network failures.
//
// On fetch failure the repo's failure counter ticks and an
// exponential backoff is parked on the State so the sweep skips this
// repo until the window elapses. On success the counter resets and
// the last-fetch timestamp is updated.
func SyncRepo(ctx context.Context, st *State, r store.RepoRef, cfg *config.Config) error {
	if _, err := os.Stat(r.Path); err != nil {
		slog.Debug("auto_fetch skip (repo path missing)", "repo", r.Path, "err", err)
		return err
	}
	if cfg == nil {
		c, err := resolve.LoadResolved(r.Path)
		if err != nil {
			return fmt.Errorf("load cfg: %w", err)
		}
		cfg = &c
	}
	// `--quiet` suppresses progress; stderr is captured into the
	// gitcmd.Error on failure. `--prune` clears tombstoned remote
	// branches so `git branch -r` doesn't accumulate ghosts.
	if err := gitcmd.Run(ctx, r.Path, "fetch", "--all", "--prune", "--quiet"); err != nil {
		failures := st.RecordSyncFailure(r.Path, backoffFor(st.SyncFailCount(r.Path)+1))
		slog.Warn("auto_fetch fetch failed",
			"repo", r.Path, "err", err,
			"consecutive_failures", failures)
		_ = st.Store.WriteEvent(ctx, store.LevelWarn, "auto_fetch_fetch_failed",
			err.Error(), r.ID, 0, "", 0, map[string]string{
				"consecutive_failures": fmt.Sprintf("%d", failures),
				"next_retry_at":        st.SyncBackoffUntil(r.Path).UTC().Format(time.RFC3339),
			})
		return err
	}
	st.RecordSyncSuccess(r.Path)

	// Build the full set of worktrees to advance: main repo path
	// first, then linked worktrees.
	paths := []string{r.Path}
	linked, err := wtreg.GitWorktreePaths(ctx, r.Path)
	if err != nil {
		slog.Warn("auto_fetch list linked worktrees", "repo", r.Path, "err", err)
	} else {
		paths = append(paths, linked...)
	}

	mode := cfg.AutoFetch.ResolvedMode()
	for _, wtPath := range paths {
		if ctx.Err() != nil {
			return nil
		}
		_ = SyncWorktree(ctx, st, r.ID, wtPath, mode)
	}

	// After the mainline branch has advanced, prune local branches whose
	// upstream was deleted and that are provably merged, then reap the
	// branch_scoped durable databases those deleted branches left behind.
	for _, branch := range pruneGoneLocals(ctx, r.Path) {
		prepare.ReapBranchDurables(ctx, cfg, st.Store, r.ID, branch)
		_ = st.Store.WriteEvent(ctx, store.LevelInfo, "branch_pruned",
			"pruned merged branch with deleted upstream: "+branch,
			r.ID, 0, "", 0, map[string]string{"branch": branch})
	}
	return nil
}

// SyncWorktree advances one working tree's HEAD to its upstream-
// tracking branch using the supplied mode ("ff" or "rebase"). Returns
// the first error encountered (refusable preconditions return nil —
// they're not failures, they're "this worktree isn't a candidate
// right now" and the skip is recorded as an event + state marker).
//
// Every refusable precondition is checked inline so we can log a
// precise reason instead of relying on git's exit-1 to mean any of
// three things.
func SyncWorktree(ctx context.Context, st *State, repoID int64, wtPath, mode string) error {
	if _, err := os.Stat(wtPath); err != nil {
		emitSkip(ctx, st, repoID, wtPath, "", SyncSkipPathGone, err.Error())
		return err
	}
	// Detached HEAD has no upstream — `symbolic-ref` exits non-zero.
	head, err := gitcmd.String(ctx, wtPath, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		emitSkip(ctx, st, repoID, wtPath, "", SyncSkipDetached, "detached HEAD or no branch")
		return nil
	}
	// Strip `refs/heads/` for the event payload below.
	branch := strings.TrimSpace(strings.TrimPrefix(head, "refs/heads/"))

	// `@{u}` resolves to the upstream-tracking ref. Missing upstream
	// → exit 128 with "no upstream configured" on stderr.
	if !gitcmd.Exists(ctx, wtPath, "@{u}") {
		emitSkip(ctx, st, repoID, wtPath, branch, SyncSkipNoUpstream, "no upstream configured")
		return nil
	}

	switch mode {
	case "rebase":
		return advanceRebase(ctx, st, repoID, wtPath, branch)
	default:
		return advanceFF(ctx, st, repoID, wtPath, branch)
	}
}

// advanceFF runs the safe `merge --ff-only` path: dirty-tree check
// first, then attempt fast-forward. Divergence is skipped, never
// resolved.
func advanceFF(ctx context.Context, st *State, repoID int64, wtPath, branch string) error {
	// Dirty index / working tree → skip. `-uno` keeps untracked files
	// out so a stray build artefact doesn't block a fast-forward.
	out, err := gitcmd.Output(ctx, wtPath, "status", "--porcelain=v1", "-uno")
	if err != nil {
		slog.Warn("auto_fetch status failed", "wt", wtPath, "err", err)
		return err
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		emitSkip(ctx, st, repoID, wtPath, branch, SyncSkipDirty, "working tree dirty")
		return nil
	}

	// merge --ff-only refuses (exit 128) when @{u} isn't a descendant
	// of HEAD. That's the divergence case — we never resolve it.
	if err := gitcmd.Run(ctx, wtPath, "merge", "--ff-only", "--quiet", "@{u}"); err != nil {
		var ge *gitcmd.Error
		if errors.As(err, &ge) && ge.ExitCode != 0 {
			emitSkip(ctx, st, repoID, wtPath, branch, SyncSkipNonFF, "fast-forward refused (divergent)")
			return nil
		}
		slog.Warn("auto_fetch ff-pull failed", "wt", wtPath, "err", err)
		return err
	}
	emitAdvance(ctx, st, repoID, wtPath, branch, "ff", "fast-forward pull advanced")
	return nil
}

// advanceRebase runs `git rebase --autostash @{u}`. --autostash
// shields a dirty tree (and restores it after); a conflict aborts the
// rebase before any partial state can land.
func advanceRebase(ctx context.Context, st *State, repoID int64, wtPath, branch string) error {
	if err := gitcmd.Run(ctx, wtPath, "rebase", "--autostash", "--quiet", "@{u}"); err != nil {
		// Best effort abort. If --autostash got past initial checks
		// and then hit a conflict, the worktree is in a half-rebased
		// state until we unwind it. A failed abort is the worst case
		// (half-rebased + we never noticed) — log loudly so the user
		// sees something in `treemand` stderr and can `git rebase
		// --abort` by hand.
		if abortErr := gitcmd.Run(ctx, wtPath, "rebase", "--abort"); abortErr != nil {
			slog.Error("auto_fetch rebase --abort failed; worktree may be half-rebased",
				"wt", wtPath, "branch", branch, "abort_err", abortErr, "rebase_err", err)
			_ = st.Store.WriteEvent(ctx, store.LevelError, "auto_fetch_rebase_abort_failed",
				abortErr.Error(), repoID, lookupWorktreeID(ctx, st, wtPath), "", 0,
				map[string]string{"wt": wtPath, "branch": branch})
		}
		emitSkip(ctx, st, repoID, wtPath, branch, SyncSkipRebaseFail, err.Error())
		return nil
	}
	emitAdvance(ctx, st, repoID, wtPath, branch, "rebase", "rebase advanced")
	return nil
}

// emitSkip writes an `auto_fetch_skipped` event and stamps the
// worktree's last-skip-reason on State so sync_status can surface it
// without scanning the event log.
func emitSkip(ctx context.Context, st *State, repoID int64, wtPath, branch, reason, detail string) {
	slog.Debug("auto_fetch skip", "wt", wtPath, "branch", branch, "reason", reason)
	st.RecordSyncSkip(wtPath, reason)
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "auto_fetch_skipped",
		detail, repoID, lookupWorktreeID(ctx, st, wtPath), "", 0, map[string]string{
			"wt":     wtPath,
			"branch": branch,
			"reason": reason,
		})
}

// emitAdvance writes an `auto_fetch_pulled` event and clears any
// previous skip marker for the worktree (the branch did advance).
func emitAdvance(ctx context.Context, st *State, repoID int64, wtPath, branch, mode, detail string) {
	slog.Info(detail, "wt", wtPath, "branch", branch, "mode", mode)
	st.RecordSyncSkip(wtPath, "")
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "auto_fetch_pulled",
		detail+" "+branch, repoID, lookupWorktreeID(ctx, st, wtPath), "", 0, map[string]string{
			"wt":     wtPath,
			"branch": branch,
			"mode":   mode,
		})
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
