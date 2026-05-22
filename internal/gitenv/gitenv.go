// Package gitenv resolves the main-repo root from any directory
// inside the repo — including linked worktrees, where the
// `.git` file is a gitlink pointing at the main repo's `.git/
// worktrees/<name>/` directory.
//
// `treeman` needs this because `.treeman.yaml` + the seed dump
// live in the main checkout, not in linked worktrees. A CLI run
// from inside `<main>/.worktrees/PROJ-1` must resolve `<main>` for
// config loading + dump path resolution.
package gitenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/stubbedev/treeman/internal/gitcmd"
)

// MainRoot returns the main-repo working directory, even when
// `start` is inside a linked worktree. Resolution order:
//
//  1. `git -C start rev-parse --git-common-dir` — gives `<main>/.git`
//     when start is a linked worktree; gives `<main>/.git` when
//     start is the main checkout. Strip the trailing `.git`.
//  2. Walk up looking for `.git` (regular repo / main checkout).
//  3. Walk up looking for `.treeman.yaml` (works for treeman-aware
//     repos that aren't git repos — rare).
//
// Returns "" + error when none of these find a root.
func MainRoot(start string) (string, error) {
	if abs, err := filepath.Abs(start); err == nil {
		start = abs
	}

	// git-common-dir resolves linked worktrees → main repo's .git.
	// Background ctx is fine: this is a fast local probe and callers
	// upstream don't have a cancellation deadline to honor.
	common, err := gitcmd.String(context.Background(), start, "rev-parse", "--git-common-dir")
	if err == nil {
		if !filepath.IsAbs(common) {
			// `git rev-parse` returns relative paths inside the
			// repo when started from a non-toplevel dir. Resolve
			// against start.
			common = filepath.Join(start, common)
		}
		// Canonicalise (the gitdir for a linked worktree is
		// <main>/.git on disk; we want <main>).
		common, _ = filepath.EvalSymlinks(common)
		if strings.HasSuffix(common, string(filepath.Separator)+".git") {
			return strings.TrimSuffix(common, string(filepath.Separator)+".git"), nil
		}
		return common, nil
	}

	// Fallbacks for non-git dirs.
	dir := start
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".treeman.yaml")); err == nil && !fi.IsDir() {
			return dir, nil
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no main repo found (no git-common-dir / .git / .treeman.yaml in any ancestor)")
		}
		dir = parent
	}
}

// IsLinkedWorktree returns true when `path` is a linked worktree
// (its `.git` is a regular file gitlink, not a directory).
func IsLinkedWorktree(path string) bool {
	fi, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	return !fi.IsDir()
}

// IsWorktreeClean returns true when `git status --porcelain` in the
// worktree is empty — no uncommitted changes, no untracked files.
// Used by `wt back --remove-if-clean` and equivalent safety
// checks.
func IsWorktreeClean(path string) (bool, error) {
	// `git status` is read-only from treeman's POV but updates the
	// index lock by default — pass readOnly=false so we don't disable
	// GIT_OPTIONAL_LOCKS for what is, semantically, a query.
	out, err := gitcmd.Output(context.Background(), path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "", nil
}

// HasUnpushedCommits returns true when the worktree's HEAD has
// commits not reachable from its upstream tracking branch. Used to
// avoid silently dropping work on auto-remove.
func HasUnpushedCommits(path string) (bool, error) {
	// `git -C <path> rev-list @{upstream}..HEAD --count` — empty
	// upstream returns non-zero exit which we treat as "no tracking
	// branch yet → can't be unpushed".
	count, err := gitcmd.String(context.Background(), path, "rev-list", "@{upstream}..HEAD", "--count")
	if err != nil {
		// No upstream — treat as no unpushed work.
		return false, nil
	}
	return count != "0" && count != "", nil
}

// DetectBranch reads the HEAD of a worktree (handles gitlink files
// for linked worktrees). Returns "" if detached HEAD or unreadable.
func DetectBranch(worktree string) string {
	headPath := filepath.Join(worktree, ".git", "HEAD")
	if fi, err := os.Stat(headPath); err != nil || fi.IsDir() {
		// gitlink file — follow it.
		linkBytes, err := os.ReadFile(filepath.Join(worktree, ".git"))
		if err != nil {
			return ""
		}
		gitdir := strings.TrimSpace(strings.TrimPrefix(string(linkBytes), "gitdir:"))
		gitdir = strings.TrimSpace(gitdir)
		headPath = filepath.Join(gitdir, "HEAD")
	}
	b, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	const prefix = "ref: refs/heads/"
	if strings.HasPrefix(line, prefix) {
		return strings.TrimPrefix(line, prefix)
	}
	return ""
}
