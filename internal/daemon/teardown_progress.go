package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/store"
)

func teardownDatabasesWithHeartbeat(
	ctx context.Context,
	cfg *config.Config,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	stop := startTeardownHeartbeat(ctx, st, repoID, worktreeID, sl, 30*time.Second)
	defer stop()
	return prepare.TeardownDatabases(ctx, cfg, sl, repoID, worktreeID, st)
}

func startTeardownHeartbeat(
	ctx context.Context,
	st *store.Store,
	repoID, worktreeID int64,
	sl string,
	interval time.Duration,
) func() {
	ctx, cancel := context.WithCancel(ctx)
	started := time.Now()
	emit := func(message string) {
		elapsed := time.Since(started).Milliseconds()
		_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtDBTeardownProgress,
			message, repoID, worktreeID, "engine-drop", elapsed,
			map[string]any{"slug": sl, "elapsed_ms": elapsed})
	}
	emit("engine drop beginning")
	done := make(chan struct{})
	safeGo(lblWorktreeReap, sl, func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				emit(fmt.Sprintf("engine drop still running (%s elapsed)", time.Since(started).Round(time.Second)))
			}
		}
	})
	return func() {
		cancel()
		<-done
	}
}
