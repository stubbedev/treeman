package daemon

import (
	"os"
	"path/filepath"
)

// gitOpMarkers are the entries in a worktree's git directory that
// signal an in-progress, conflict-prone git operation. They live
// alongside HEAD, so the HEAD watcher (which already subscribes to that
// directory) reacts to fsnotify events on them, and gitOpInProgress
// probes them to decide whether a prepare/finalize should be deferred
// until the tree settles. `rebase-merge`/`rebase-apply` are
// directories; the rest are files — os.Stat treats both uniformly.
var gitOpMarkers = map[string]string{
	"MERGE_HEAD":       "merge",
	"rebase-merge":     "rebase",
	"rebase-apply":     "rebase",
	"CHERRY_PICK_HEAD": "cherry-pick",
	"REVERT_HEAD":      "revert",
}

// isGitOpMarker reports whether base names one of the op-state markers.
// The HEAD watcher uses this to widen its event filter beyond HEAD
// itself so it observes the merge/rebase start AND clear transitions.
func isGitOpMarker(base string) bool {
	_, ok := gitOpMarkers[base]
	return ok
}

// resolveGitDir returns the per-worktree git directory — where HEAD,
// MERGE_HEAD, rebase-*/ etc. live. For a linked worktree this is the
// admin dir under <common>/worktrees/<name>/ (NOT the repo's shared
// common dir), which is exactly where git records merge/rebase state
// per worktree. Reuses resolveHeadPath's gitlink handling: the git dir
// is the parent of the resolved HEAD path.
func resolveGitDir(worktreePath string) (string, error) {
	headPath, err := resolveHeadPath(worktreePath)
	if err != nil {
		return "", err
	}
	return filepath.Dir(headPath), nil
}

// gitOpInProgress reports the in-progress conflict-prone git operation
// in gitDir ("merge"|"rebase"|"cherry-pick"|"revert"), or "" when the
// tree is clean. A worktree mid-merge/-rebase carries conflict markers
// in tracked files, so prepare must defer until the marker clears.
func gitOpInProgress(gitDir string) string {
	for marker, op := range gitOpMarkers {
		if _, err := os.Stat(filepath.Join(gitDir, marker)); err == nil {
			return op
		}
	}
	return ""
}

// worktreeGitOp is the worktree-path convenience used by the finalize
// guards: resolve the git dir, then probe. Returns "" (proceed) when
// the git dir can't be resolved — a missing/corrupt .git pointer is the
// watcher's problem to surface, not a reason to silently defer prepare
// forever.
func worktreeGitOp(worktreePath string) string {
	gitDir, err := resolveGitDir(worktreePath)
	if err != nil {
		return ""
	}
	return gitOpInProgress(gitDir)
}
