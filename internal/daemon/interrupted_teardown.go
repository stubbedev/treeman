package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/store"
)

const resumeTeardownWorkers = 4

// SweepInterruptedTeardowns finishes every teardown the previous daemon
// stopped in the middle of. Such a worktree's delete:start has no
// terminal event, so it reads as tearing down forever, and whatever the
// interrupted run had not reached yet (databases, the checkout, git's
// admin entry, the ports, the registry row) stays behind. Every
// teardown step is idempotent, so the resumed run completes what is
// left. It runs without force: what the original request removed is
// already gone, and anything git would refuse stays and fails the
// teardown visibly instead. Must run before this daemon dispatches any
// teardown of its own, which the store-wide daemon lock guarantees at
// boot.
func SweepInterruptedTeardowns(ctx context.Context, st *State) {
	rows, err := st.Store.ListInterruptedTeardowns(ctx)
	if err != nil {
		slog.Error("interrupted-teardown sweep: interrupted teardowns stay unfinished", "err", err)
		return
	}
	ctx = withTrigger(ctx, trigger{Source: triggerDaemonRestart})
	var wg sync.WaitGroup
	sem := make(chan struct{}, resumeTeardownWorkers)
	for _, row := range rows {
		wg.Add(1)
		sem <- struct{}{}
		safeGo(lblTeardownResume, row.Path, func() {
			defer wg.Done()
			defer func() { <-sem }()
			resumeTeardown(ctx, st, row)
		})
	}
	wg.Wait()
}

func resumeTeardown(ctx context.Context, st *State, row store.WorktreeRow) {
	slog.Info("resuming teardown interrupted by a daemon restart", "wt", row.Path, "id", row.ID)
	env, err := st.Store.LoadInheritedEnv(ctx, row.ID)
	if err != nil {
		msg := fmt.Sprintf(
			"resume teardown of %s interrupted by a daemon restart: load the environment captured at its last create/finalize: %v; rerun the delete",
			row.Path,
			err,
		)
		slog.Error(msg)
		_ = st.Store.WriteEvent(ctx, store.LevelError, store.EvtWorktreeDeleteError, msg,
			row.RepoID, row.ID, "", 0, map[string]string{"worktree_path": row.Path})
		return
	}
	if err := TeardownWorktree(gitcmd.WithPath(ctx, env["PATH"]), st, row.RepoPath, row.Path, false, env); err != nil {
		slog.Warn("resumed teardown failed", "wt", row.Path, "err", err)
	}
}
