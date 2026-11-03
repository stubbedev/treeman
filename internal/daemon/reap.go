package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/wt"
)

// reapWorkers is how many top-level trash entries parallelRemoveAll
// removes concurrently. Reaping is `rm -rf` under chrt/ionice/nice — it
// is I/O-bound on the deleting filesystem, not CPU-bound, so we scale
// with core count (a rough proxy for the host's I/O parallelism: more
// cores ≈ faster NVMe / more spindles) but cap it: past ~8 concurrent
// rm -rf the disk, not the scheduler, is the bottleneck and extra
// workers just add contention. Floor of 4 keeps the prior behaviour on
// small hosts. Each rm already runs at SCHED_IDLE so foreground work is
// never starved regardless.
func reapWorkers() int {
	n := runtime.NumCPU()
	switch {
	case n < 4:
		return 4
	case n > 8:
		return 8
	default:
		return n
	}
}

// trashDirName is the subdirectory under worktreesRoot where deleted
// working trees are renamed for background reaping. Chosen to (a) be
// hidden, (b) carry the treeman prefix so accidental ls reveals
// origin, and (c) live inside the worktrees root so the rename stays
// on the same filesystem.
const trashDirName = ".treeman-trash"

// removeWorktreeViaTrash atomically moves wtRoot into a trash dir on
// the same filesystem and then runs `git worktree prune` to unregister
// the now-orphaned admin directory. Returns the trash path so the
// caller can hand it to scheduleBackgroundReap.
//
// On any failure (cross-FS rename, missing dir, permission), falls
// back to inline `git worktree remove [--force] wtRoot`. The fallback
// path returns trashPath="" so the caller skips background reaping.
//
// Caller must hold the per-repo teardown mutex — this touches the
// shared <common>/worktrees/ admin tree via `git worktree prune`.
func removeWorktreeViaTrash(
	ctx context.Context,
	repoRoot, wtRoot, worktreesRoot string,
	force bool,
) (trashPath string, err error) {
	if _, statErr := os.Stat(wtRoot); statErr != nil {
		// Working tree already gone — just prune git's view of it.
		_ = runGitPrune(ctx, repoRoot)
		return "", nil //nolint:nilerr // idempotent: a missing worktree is success for a removal
	}

	trashRoot := filepath.Join(worktreesRoot, trashDirName)
	if mkErr := os.MkdirAll(trashRoot, 0o700); mkErr == nil {
		name := fmt.Sprintf("%d-%s", time.Now().UnixNano(), filepath.Base(wtRoot))
		dest := filepath.Join(trashRoot, name)
		if renameErr := os.Rename(wtRoot, dest); renameErr == nil {
			// Rename succeeded: working tree is now under trash.
			// Tell git its admin dir is orphaned.
			_ = runGitPrune(ctx, repoRoot)
			return dest, nil
		}
		// Common failure: cross-filesystem rename (EXDEV) when the
		// user gave an absolute wtRoot outside worktreesRoot. Fall
		// through to git worktree remove.
	}

	gitArgs := []string{"-C", repoRoot, "worktree", "remove"}
	if force {
		gitArgs = append(gitArgs, "--force")
	}
	gitArgs = append(gitArgs, wtRoot)
	if runErr := lowPriorityCommand(ctx, "git", gitArgs).Run(); runErr != nil {
		return "", runErr
	}
	// `git worktree remove` drops tracked files + the admin entry but
	// leaves untracked bring-in / hook-generated dirs (storage/, vendor/,
	// node_modules/) behind, so the next `wt create` at this path trips
	// "destination path already exists". Nuke them — mirrors the CLI
	// inlineTeardown path. Guarded to paths under worktreesRoot, so the
	// cross-FS case (wtRoot outside the worktrees root) is a safe no-op.
	wt.RemoveWorktreeTree(wtRoot, worktreesRoot)
	return "", nil
}

// runGitPrune invokes `git worktree prune --expire=now`. The
// --expire=now is essential: git's default prune horizon is
// `gc.worktreePruneExpire` which is typically 3 months, so a plain
// prune would leave the just-orphaned admin dir behind.
func runGitPrune(ctx context.Context, repoRoot string) error {
	return lowPriorityCommand(ctx, "git",
		[]string{"-C", repoRoot, "worktree", "prune", "--expire=now"}).Run()
}

// scheduleBackgroundReap enqueues trashPath onto repoPath's reap
// queue. Each repo has a single drain goroutine — bursty deletes
// (five `gwtd`s back-to-back) queue serially instead of spawning
// five parallel reapers competing for the same disk. The drain
// worker uses parallelRemoveAll which fans the top-level dir entries
// across its own bounded worker pool, so individual rms still
// overlap inside one trash. Worker runs on st.BgCtx (daemon
// lifetime) — survives the originating RPC but dies with the daemon.
func scheduleBackgroundReap(st *State, repoPath, trashPath string) {
	queue := st.reapQueueFor(repoPath)
	select {
	case queue <- trashPath:
	default:
		// Queue full (highly unlikely with a 64-slot buffer): fall
		// back to a one-shot reaper so we never lose the trash entry.
		safeGo(lblWorktreeReap, trashPath, func() {
			if err := parallelRemoveAll(st.BgCtx, trashPath, reapWorkers()); err != nil {
				slog.Warn("background reap", "trash", trashPath, "err", err)
			}
		})
	}
}

// reapQueueFor returns (or lazily creates) the per-repo reap queue.
// The drain goroutine launches on first creation and lives for the
// daemon's lifetime.
func (st *State) reapQueueFor(repoPath string) chan string {
	st.reapQueuesMu.Lock()
	defer st.reapQueuesMu.Unlock()
	if q, ok := st.reapQueues[repoPath]; ok {
		return q
	}
	q := make(chan string, 64)
	st.reapQueues[repoPath] = q
	safeGo(lblWorktreeReapDrain, repoPath, func() {
		for {
			select {
			case <-st.BgCtx.Done():
				return
			case trash, ok := <-q:
				if !ok {
					return
				}
				if err := parallelRemoveAll(st.BgCtx, trash, reapWorkers()); err != nil {
					slog.Warn("background reap", "trash", trash, "err", err)
				}
			}
		}
	})
	return q
}

// SweepTrashDirs scans every registered repo's `<worktreesRoot>/
// .treeman-trash/` for leftover entries (from a daemon that died
// mid-reap) and queues them for background reaping. Called once on
// daemon boot. Per-repo failures are logged + skipped — a moved or
// deleted repo dir shouldn't abort startup.
func SweepTrashDirs(ctx context.Context, st *State, repoPaths []string) {
	for _, p := range repoPaths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		cfg, err := resolve.LoadResolved(p)
		if err != nil {
			slog.Debug("trash sweep: load config", "repo", p, "err", err)
			continue
		}
		trashRoot := filepath.Join(worktreesRootOf(cfg.Worktrees.Root, p), trashDirName)
		entries, err := os.ReadDir(trashRoot)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Debug("trash sweep: read dir", "trash", trashRoot, "err", err)
			}
			continue
		}
		for _, e := range entries {
			leftover := filepath.Join(trashRoot, e.Name())
			slog.Info("trash sweep: reaping leftover", "path", leftover)
			scheduleBackgroundReap(st, p, leftover)
		}
	}
}

// ReapDeadRepos hard-removes (via RemoveRepo's cascade over events,
// hook_runs, snapshots, worktrees) every registered repo whose root
// path no longer exists on disk. Called once on daemon boot to reclaim
// rows left by ephemeral repos — e2e test checkouts under /tmp, scratch
// clones, etc. — that were never explicitly unregistered.
//
// Conservative by design: only a repo whose path is fully gone is
// reaped, and each removal is logged at WARN with the path. The known
// false-positive is a repo on a temporarily-unmounted volume; on a dev
// host that risk is acceptable against unbounded row growth, but the
// loud log lets an operator notice if a real repo vanished.
func ReapDeadRepos(ctx context.Context, st *State) {
	refs, err := st.Store.ListRepoRefs(ctx)
	if err != nil {
		slog.Warn("dead-repo reap: list repos", "err", err)
		return
	}
	for _, r := range refs {
		if _, err := os.Stat(r.Path); err == nil {
			continue
		}
		if err := st.Store.RemoveRepo(ctx, r.ID); err != nil {
			slog.Warn("dead-repo reap: remove", "repo", r.Path, "id", r.ID, "err", err)
			continue
		}
		slog.Warn("dead-repo reap: removed registry rows for missing repo", "repo", r.Path, "id", r.ID)
	}
}

// SweepOrphanWorktreePorts physically drops port reservations left
// behind by soft-deleted (or vanished) worktrees. Called once on
// daemon boot so leaked rows from an interrupted teardown — or from a
// pre-release binary that soft-deleted without releasing — don't pin
// the allocation range upward forever. Failures are logged + skipped;
// a sweep error must not abort startup.
func SweepOrphanWorktreePorts(ctx context.Context, st *State) {
	n, err := st.Store.PurgeDeletedWorktreePorts(ctx)
	if err != nil {
		slog.Warn("port sweep: purge orphan reservations", "err", err)
		return
	}
	if n > 0 {
		slog.Info("port sweep: reaped orphan reservations", "rows", n)
	}
}

// parallelRemoveAll splits the top-level entries of root across up to
// `workers` concurrent `rm -rf` children, each wrapped in
// lowPriorityCommand (chrt -i 0 / ionice / nice). After all entries
// drain, removes root itself.
//
// Why shell out to `rm` instead of os.RemoveAll: rm honours the
// scheduler/IO niceness wrappers; an in-process Go RemoveAll would
// run at the daemon's own priority (typically nice 0), which is
// exactly what we're trying to avoid. Parallelism is by top-level
// child so node_modules/ and vendor/ get their own worker each.
func parallelRemoveAll(ctx context.Context, root string, workers int) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if workers < 1 {
		workers = 1
	}
	if len(entries) < workers {
		workers = len(entries)
	}
	if workers > 0 {
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for _, e := range entries {
			target := filepath.Join(root, e.Name())
			sem <- struct{}{}
			wg.Add(1)
			safeGo(lblWorktreeReap, target, func() {
				defer wg.Done()
				defer func() { <-sem }()
				if runErr := lowPriorityCommand(ctx, "rm", []string{"-rf", target}).Run(); runErr != nil {
					slog.Debug("reap rm", "path", target, "err", runErr)
				}
			})
		}
		wg.Wait()
	}
	return os.RemoveAll(root)
}
