package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/snapshot"
)

// SnapshotGCLoop runs the periodic snapshot eviction sweep until
// ctx is cancelled. Wakes every `snapshots.retention.gc_interval_
// minutes` of the GLOBAL config (`~/.config/treeman/config.yaml`),
// and per-repo it consults that repo's own config so cap policies
// can be per-project.
//
// Each tick:
//  1. Enumerate every repo from the SQLite registry.
//  2. Load the repo's layered config (so .treeman.yaml overrides
//     apply).
//  3. Run snapshot.EvictExcess against the repo's CapPerRepo.
//
// Errors are logged and the loop continues — one flaky repo
// shouldn't break GC for the others.
func SnapshotGCLoop(ctx context.Context, st *State) {
	// Pull the global interval from the layered config rooted at
	// the user's home dir (no repo). This is the steady-state knob.
	globalCfg, _ := config.LoadLayered("")
	interval := time.Duration(globalCfg.Snapshots.Retention.GcIntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	slog.Info("snapshot_gc_loop started", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runGCSweep(ctx, st)
		}
	}
}

func runGCSweep(ctx context.Context, st *State) {
	paths, err := st.Store.ListRepoPaths(ctx)
	if err != nil {
		slog.Warn("snapshot_gc enumerate repos", "err", err)
		return
	}
	for _, p := range paths {
		cfg, err := config.LoadLayered(p)
		if err != nil {
			slog.Warn("snapshot_gc load cfg", "repo", p, "err", err)
			continue
		}
		repoID, err := st.Store.EnsureRepo(ctx, p, filepathBase(p))
		if err != nil {
			continue
		}
		snapshot.EvictExcess(ctx, &cfg, st.Store, repoID)
	}
}

// filepathBase mirrors filepath.Base but lives here to keep this
// file dependency-light (the package already imports filepath via
// other files).
func filepathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
