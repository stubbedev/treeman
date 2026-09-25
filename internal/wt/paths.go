package wt

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/gitenv"
)

// WorktreesRoot resolves cfg.Worktrees.Root against repoRoot,
// defaulting to ".worktrees". Absolute paths pass through; relative
// paths join under repoRoot.
func WorktreesRoot(cfg config.Config, repoRoot string) string {
	raw := cfg.Worktrees.Root
	if raw == "" {
		raw = ".worktrees"
	}
	if filepath.IsAbs(raw) {
		return raw
	}
	return filepath.Join(repoRoot, raw)
}

// DetectDefaultBranch returns the upstream default branch name —
// preferring origin/HEAD's symbolic ref, falling back to the local
// HEAD ref, and finally "main" as a last resort. The origin/HEAD
// lookup reads the loose symref file fork-free (git never packs
// symbolic refs) and only falls back to the `symbolic-ref` fork when
// the file is missing.
func DetectDefaultBranch(ctx context.Context, repoRoot string) string {
	if b, err := os.ReadFile(filepath.Join(repoRoot, ".git", "refs", "remotes", "origin", "HEAD")); err == nil {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/remotes/origin/"); ok && ref != "" {
			return ref
		}
	}
	if s, err := gitcmd.String(ctx, repoRoot, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if cut, ok := strings.CutPrefix(s, "origin/"); ok {
			return cut
		}
		return s
	}
	if s := gitenv.DetectBranch(ctx, repoRoot); s != "" {
		return s
	}
	return "main"
}

// RefExistsLocal reports whether refs/heads/<name> resolves in repoRoot.
func RefExistsLocal(ctx context.Context, repoRoot, name string) bool {
	return gitcmd.Exists(ctx, repoRoot, "refs/heads/"+name)
}

// RefExistsRemote reports whether refs/remotes/origin/<name>
// resolves in repoRoot.
func RefExistsRemote(ctx context.Context, repoRoot, name string) bool {
	return gitcmd.Exists(ctx, repoRoot, "refs/remotes/origin/"+name)
}
