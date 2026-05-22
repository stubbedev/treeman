package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stubbedev/treeman/internal/resolve"
)

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
		return "", nil
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
		safeGo("wt_reap:"+trashPath, func() {
			if err := parallelRemoveAll(st.BgCtx, trashPath, 4); err != nil {
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
	safeGo("wt_reap_drain:"+repoPath, func() {
		for {
			select {
			case <-st.BgCtx.Done():
				return
			case trash, ok := <-q:
				if !ok {
					return
				}
				if err := parallelRemoveAll(st.BgCtx, trash, 4); err != nil {
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
			go func(t string) {
				defer wg.Done()
				defer func() { <-sem }()
				if runErr := lowPriorityCommand(ctx, "rm", []string{"-rf", t}).Run(); runErr != nil {
					slog.Debug("reap rm", "path", t, "err", runErr)
				}
			}(target)
		}
		wg.Wait()
	}
	return os.RemoveAll(root)
}
