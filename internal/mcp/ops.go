package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// resolveRepo returns the absolute repo root inferred from an
// explicit override or the cwd. Mirrors cmd's resolveRepo so this
// package can run without importing the cmd tree.
func resolveRepo(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		return gitenv.MainRoot(abs)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return gitenv.MainRoot(cwd)
}

// resolveWorktree expands "" → cwd, then canonicalises to an
// absolute path. Branch is read from .git/HEAD when available.
func resolveWorktree(path string) (wt, branch string) {
	if path == "" {
		path, _ = os.Getwd()
	}
	wt, _ = filepath.Abs(path)
	branch = detectBranch(wt)
	return wt, branch
}

func detectBranch(worktree string) string {
	head := filepath.Join(worktree, ".git", "HEAD")
	if _, err := os.Stat(head); err != nil {
		link, err := os.ReadFile(filepath.Join(worktree, ".git"))
		if err != nil {
			return ""
		}
		gitdir := strings.TrimSpace(strings.TrimPrefix(string(link), "gitdir:"))
		head = filepath.Join(gitdir, "HEAD")
	}
	b, err := os.ReadFile(head)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	const pfx = "ref: refs/heads/"
	if strings.HasPrefix(s, pfx) {
		return strings.TrimPrefix(s, pfx)
	}
	return ""
}

// captureEnv snapshots os.Environ() so hook + prepare subprocesses
// see the invoking shell's $PATH.
func captureEnv() map[string]string {
	env := os.Environ()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}

// openStore opens the default SQLite event store. Caller closes.
func openStore(ctx context.Context) (*store.Store, error) {
	p, err := store.DefaultDBPath()
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, p)
}

// runPrepare is the self-contained equivalent of cmd's
// RunPrepareOnWorktree. Discovers repo + cfg, opens the store,
// dispatches prepare.Run.
func runPrepare(ctx context.Context, worktree, repoOverride string) ([]prepare.Outcome, error) {
	wt, branch := resolveWorktree(worktree)
	repoRoot, err := resolveRepo(repoOverride)
	if err != nil {
		repoRoot, err = gitenv.MainRoot(wt)
		if err != nil {
			return nil, err
		}
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, err
	}
	sl := slug.For(wt, branch)
	st, err := openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	wtID, _ := st.EnsureWorktree(ctx, repoID, wt, sl.Value, branch)
	return prepare.Run(ctx, &cfg, wt, sl, st, repoID, wtID, captureEnv())
}

// runHookPhase synchronously executes one hook phase. Mirrors cmd's
// RunHookPhase but with no CLI surface.
func runHookPhase(ctx context.Context, phase, worktree string) (hooks.RunOutcome, error) {
	wt, branch := resolveWorktree(worktree)
	repoRoot, err := gitenv.MainRoot(wt)
	if err != nil {
		return hooks.RunOutcome{}, err
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return hooks.RunOutcome{}, err
	}
	sl := slug.For(wt, branch)
	env := captureEnv()
	switch phase {
	case "precreate":
		return hooks.RunPrecreateHooks(ctx, cfg.Hooks.Precreate, repoRoot, wt, sl.Value, env)
	case "postcreate":
		return hooks.RunHooks(ctx, phase, cfg.Hooks.Postcreate, repoRoot, wt, sl.Value, env, true)
	case "predelete":
		return hooks.RunHooks(ctx, phase, cfg.Hooks.Predelete, repoRoot, wt, sl.Value, env, true)
	case "postdelete":
		return hooks.RunHooks(ctx, phase, cfg.Hooks.Postdelete, repoRoot, wt, sl.Value, env, true)
	}
	return hooks.RunOutcome{}, fmt.Errorf("unknown phase %q (want precreate|postcreate|predelete|postdelete)", phase)
}
