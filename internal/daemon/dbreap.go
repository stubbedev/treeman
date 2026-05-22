package daemon

import (
	"log/slog"

	"github.com/stubbedev/treeman/internal/prepare"
)

// ScheduleDBDrop enqueues a TeardownDatabases call onto repoPath's
// drop queue. Each repo has a single drain goroutine — sequential
// drops per repo prevent five-`gwtd`-at-once bursts from saturating
// the DB server with parallel `DROP DATABASE` storms. Different
// repos still drop in parallel because each owns its own queue.
//
// The job runs on st.BgCtx so it survives the originating teardown
// RPC but dies with the daemon. A daemon crash mid-drop leaves the
// per-worktree DB lingering — harmless since the DB just sits empty
// and inert, and a future `prepare` for the same slug will overwrite
// it via the engine's CREATE OR REPLACE / DROP+CREATE semantics.
func ScheduleDBDrop(st *State, repoPath string, job DBDropJob) {
	queue := st.dropQueueFor(repoPath)
	select {
	case queue <- job:
	default:
		// Queue saturated (64 pending drops on one repo is well
		// beyond normal). Fire a one-shot goroutine so we never
		// lose the drop entirely.
		safeGo("db_drop_overflow:"+repoPath, func() {
			runDBDrop(st, job)
		})
	}
}

func (st *State) dropQueueFor(repoPath string) chan DBDropJob {
	st.dropQueuesMu.Lock()
	defer st.dropQueuesMu.Unlock()
	if q, ok := st.dropQueues[repoPath]; ok {
		return q
	}
	q := make(chan DBDropJob, 64)
	st.dropQueues[repoPath] = q
	safeGo("db_drop_drain:"+repoPath, func() {
		for {
			select {
			case <-st.BgCtx.Done():
				return
			case job, ok := <-q:
				if !ok {
					return
				}
				runDBDrop(st, job)
			}
		}
	})
	return q
}

func runDBDrop(st *State, job DBDropJob) {
	cfg := job.Cfg
	if err := prepare.TeardownDatabases(st.BgCtx, &cfg, job.Slug, job.RepoID, job.WorktreeID, st.Store); err != nil {
		slog.Warn("background db drop",
			"slug", job.Slug, "repo_id", job.RepoID, "wt_id", job.WorktreeID, "err", err)
	}
}
