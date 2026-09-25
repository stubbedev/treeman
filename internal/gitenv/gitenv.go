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
	"fmt"
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
func MainRoot(ctx context.Context, start string) (string, error) {
	if abs, err := filepath.Abs(start); err == nil {
		start = abs
	}

	// git-common-dir resolves linked worktrees → main repo's .git.
	common, err := gitcmd.String(ctx, start, "rev-parse", "--git-common-dir")
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
		if before, ok := strings.CutSuffix(common, string(filepath.Separator)+".git"); ok {
			return before, nil
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

// IsGitWorktree returns true when `path` is any git working tree —
// the main checkout or a standalone clone (`.git` directory) or a
// linked worktree (`.git` gitlink file). A directory that merely
// holds leftovers from a torn-down worktree has no `.git` at all,
// which is exactly the case callers use this to reject.
func IsGitWorktree(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// IsWorktreeClean returns true when `git status --porcelain=v1 -uno`
// in the worktree is empty — no uncommitted changes to TRACKED
// files. Untracked files deliberately don't count: this is the same
// dirty definition the daemon's merge-safety check uses
// (autofetch), and git's own `worktree remove` still refuses on
// untracked files where that matters.
//
// The probe runs WITHOUT GIT_OPTIONAL_LOCKS=0 (readOnly=false) so git
// persists the refreshed index stat-cache — with it, every call
// re-lstats the entire tracked tree, which dominated repeated status
// checks on multi-GB worktrees.
func IsWorktreeClean(ctx context.Context, path string) (bool, error) {
	out, err := gitcmd.OutputRW(ctx, path, false, "status", "--porcelain=v1", "-uno")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "", nil
}

// HasUnpushedCommits returns true when the worktree's HEAD has
// commits not reachable from its upstream tracking branch. Used to
// avoid silently dropping work on auto-remove.
func HasUnpushedCommits(ctx context.Context, path string) (bool, error) {
	// `git -C <path> rev-list @{upstream}..HEAD --count` — empty
	// upstream returns non-zero exit which we treat as "no tracking
	// branch yet → can't be unpushed".
	count, err := gitcmd.String(ctx, path, "rev-list", "@{upstream}..HEAD", "--count")
	if err != nil {
		// No upstream — treat as no unpushed work.
		return false, nil //nolint:nilerr // no tracking branch means nothing can be unpushed
	}
	return count != "0" && count != "", nil
}

// HeadPath returns the absolute path of a working tree's HEAD file,
// following the `gitdir:` gitlink redirect when `.git` is a regular
// file (linked worktrees). This is a plain file read — `git
// symbolic-ref` / `branch --show-current` forks are replaceable by it
// everywhere the branch name of a known worktree is needed.
func HeadPath(worktree string) (string, error) {
	dotGit := filepath.Join(worktree, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil {
		return "", fmt.Errorf("stat .git: %w", err)
	}
	if fi.IsDir() {
		return filepath.Join(dotGit, "HEAD"), nil
	}
	body, err := os.ReadFile(dotGit)
	if err != nil {
		return "", fmt.Errorf("read gitlink: %w", err)
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(body)), "gitdir: ")
	if !ok {
		return "", fmt.Errorf("gitlink missing %q prefix", "gitdir: ")
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(worktree, gitdir)
	}
	return filepath.Join(gitdir, "HEAD"), nil
}

// DetectBranch reads the HEAD of a worktree (handles gitlink files
// for linked worktrees). Returns "" if detached HEAD or unreadable.
func DetectBranch(_ context.Context, worktree string) string {
	headPath, err := HeadPath(worktree)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	const prefix = "ref: refs/heads/"
	if after, ok := strings.CutPrefix(line, prefix); ok {
		return after
	}
	return ""
}
