package prepare

import (
	"context"
	"fmt"

	"github.com/stubbedev/treeman/internal/store"
)

// SetupIncompleteError refuses a prepare of a worktree whose setup (its
// create-before-engines step) failed and has not succeeded since. The
// databases' migrate and seed steps depend on what setup installs
// (vendor/, node_modules/): run against a half-set-up worktree they all
// fail, and a branch-scoped database is re-seeded on top of the failure
// (#198).
type SetupIncompleteError struct {
	WorktreeID int64
	Failure    string
}

func (e *SetupIncompleteError) Error() string {
	return fmt.Sprintf("worktree %d: setup has not succeeded since it failed (%s); "+
		"run `treeman worktree finalize` to re-run setup before preparing its databases",
		e.WorktreeID, e.Failure)
}

// requireSetup returns a *SetupIncompleteError when the store records
// worktreeID's setup as failed. A finalize opens its own lifecycle op
// before preparing, so only a prepare outside one (a prepare task, the
// MCP server) is refused. Without a store or a worktree row there is
// nothing to check.
func requireSetup(ctx context.Context, st *store.Store, worktreeID int64) error {
	if st == nil || worktreeID <= 0 {
		return nil
	}
	failure, failed, err := st.SetupFailure(ctx, worktreeID)
	if err != nil {
		return fmt.Errorf("check the setup of worktree %d before preparing: %w", worktreeID, err)
	}
	if failed {
		return &SetupIncompleteError{WorktreeID: worktreeID, Failure: failure.Message}
	}
	return nil
}
