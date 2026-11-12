package daemon

import (
	"errors"
	"fmt"

	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/shellenv"
)

// beforeEnginesFailure adds the hint matching why the
// create-before-engines step failed. When the hook environment never
// loaded, no hook ran, so the contract hint about database-touching
// hooks would mislead (#198).
func beforeEnginesFailure(err error) error {
	if de, ok := errors.AsType[*shellenv.DirenvError](err); ok {
		return fmt.Errorf("%w\nhint: %s", err, envLoadHint(de))
	}
	// create-before-engines runs BEFORE the engine databases exist
	// (deps must install so migrate can find them) — a hook that boots
	// the app and queries the DB here hits "Unknown database" (#18).
	return fmt.Errorf(
		"%w\nhint: %s hooks run before this worktree's databases exist; move any app-boot / DB-touching step to %s. Retry this worktree with `treeman worktree finalize`",
		err,
		hooks.CreateBeforeEngines,
		hooks.CreateAfterEngines,
	)
}

func envLoadHint(de *shellenv.DirenvError) string {
	noHookRan := fmt.Sprintf("loading this worktree's direnv environment failed before any %s hook ran", hooks.CreateBeforeEngines)
	switch de.Failure {
	case shellenv.DirenvTimedOut:
		return noHookRan + "; a cold `use flake` build keeps running in the nix daemon after the timeout. Once `direnv exec . true` returns promptly in the worktree, retry with `treeman worktree finalize`"
	case shellenv.DirenvInterrupted:
		return noHookRan + " because the run was stopped; retry with `treeman worktree finalize`"
	default:
		return noHookRan + "; fix the `.envrc` (or run `direnv allow` in the worktree), then retry with `treeman worktree finalize`"
	}
}
