package prepare

import (
	"context"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// baseBranchOf resolves a branch's tracked upstream and strips the
// remote prefix, yielding the local base branch the new worktree was
// forked off (e.g. "develop"). Returns "" when the branch has no
// upstream configured — `branch_scoped` then falls back to
// the static `dump.path`.
//
// Why upstream: treeman creates worktrees with
// `git worktree add -b feature/x <path> origin/<base>`. Git's
// default `branch.autoSetupMerge=true` sets feature/x's upstream to
// `origin/<base>` when the start point is a remote-tracking branch,
// so `@{upstream}` recovers the base. Branches cut from a purely
// local ref (no remote-tracking start point) won't have an upstream;
// those fall through to the dump path.
func baseBranchOf(ctx context.Context, repoRoot, branch string) string {
	if branch == "" {
		return ""
	}
	out, err := gitcmd.String(ctx, repoRoot, "rev-parse", "--abbrev-ref", branch+"@{upstream}")
	if err != nil || out == "" {
		return ""
	}
	return stripRemotePrefix(ctx, repoRoot, out)
}

// stripRemotePrefix removes a leading `<remote>/` from `ref` when the
// first path component names a configured git remote. `origin/develop`
// → `develop`; a local ref like `develop` is returned unchanged.
func stripRemotePrefix(ctx context.Context, repoRoot, ref string) string {
	slash := strings.IndexByte(ref, '/')
	if slash < 0 {
		return ref
	}
	candidate := ref[:slash]
	remotes, err := gitcmd.Output(ctx, repoRoot, "remote")
	if err != nil {
		return ref
	}
	for _, r := range strings.Split(strings.TrimSpace(string(remotes)), "\n") {
		if strings.TrimSpace(r) == candidate {
			return ref[slash+1:]
		}
	}
	return ref
}

// resolveBaseSourceDB returns the live database name to dump-stream
// from when seeding `databases[dbIdx]` for a new worktree on
// `newBranch`. Reports (name, true, nil) when the base branch is
// checked out somewhere treeman can find — a tracked worktree or the
// repo-root checkout — and ("", false, nil) when it isn't (caller
// falls back to `dump.path`).
//
// Name rendering follows where the base lives:
//   - tracked main worktree (is_main row) OR the repo-root checkout
//     currently on the base branch → the
//     `main_worktree.databases[dbIdx].name_template` overlay,
//     rendered against the main slug. This is how a feature branch
//     forked off `develop` (which lives at the repo root) seeds from
//     the unprefixed `kontainer` DB.
//   - any other tracked worktree → the worktree-tier
//     `name_template`, rendered against that worktree's stored slug.
func resolveBaseSourceDB(
	ctx context.Context,
	st *store.Store,
	cfg *config.Config,
	repoRoot string,
	repoID int64,
	dbIdx int,
	scope bsScope,
	newBranch string,
) (string, bool, error) {
	baseBranch := baseBranchOf(ctx, repoRoot, newBranch)
	if baseBranch == "" {
		return "", false, nil
	}

	rows, err := st.ListWorktreesForRepo(ctx, repoID)
	if err != nil {
		return "", false, err
	}
	for _, w := range rows {
		if w.Deleted || w.Branch != baseBranch {
			continue
		}
		if w.IsMain {
			return renderMainBaseDB(cfg, dbIdx, scope, w.Path, baseBranch)
		}
		// A linked worktree's active namespace is branch-independent —
		// keyed off its path, not its stored (branch-derived) slug.
		return renderWorktreeBaseDB(cfg, dbIdx, scope, slug.For(w.Path, ""))
	}

	// Base branch not in any tracked worktree row — is the repo root
	// itself on it? (The common case: develop sits at the repo root,
	// not enrolled as a treeman main worktree.)
	if cur, _ := gitcmd.String(ctx, repoRoot, "rev-parse", "--abbrev-ref", "HEAD"); cur == baseBranch {
		return renderMainBaseDB(cfg, dbIdx, scope, repoRoot, baseBranch)
	}
	return "", false, nil
}

// baseTemplate returns the active-namespace template for `d` under
// `scope`: name_template for name-scoped engines, key_prefix for
// prefix-scoped engines.
func baseTemplate(d config.DatabaseConfig, scope bsScope) string {
	if scope == scopePrefix {
		return d.KeyPrefix
	}
	return d.NameTemplate
}

// renderMainBaseDB renders the active namespace for databases[dbIdx]
// with the main_worktree overlay applied and the main slug for
// (path, branch). The overlaid template is typically bare (no `{slug}`),
// so the rendered name is the unprefixed app DB the repo root points at.
func renderMainBaseDB(cfg *config.Config, dbIdx int, scope bsScope, path, branch string) (string, bool, error) {
	cc := *cfg
	config.ApplyMainWorktreeOverlay(&cc)
	if dbIdx < 0 || dbIdx >= len(cc.Databases) {
		return "", false, nil
	}
	name, err := template.Render(baseTemplate(cc.Databases[dbIdx], scope), template.FromSlug(slug.ForMain(path, branch)))
	if err != nil {
		return "", false, err
	}
	return name, name != "", nil
}

// renderWorktreeBaseDB renders the active namespace for databases[dbIdx]
// against a tracked worktree's branch-independent slug.
func renderWorktreeBaseDB(cfg *config.Config, dbIdx int, scope bsScope, sl slug.Slug) (string, bool, error) {
	if dbIdx < 0 || dbIdx >= len(cfg.Databases) {
		return "", false, nil
	}
	name, err := template.Render(baseTemplate(cfg.Databases[dbIdx], scope), template.FromSlug(sl))
	if err != nil {
		return "", false, err
	}
	return name, name != "", nil
}
