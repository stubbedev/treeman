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
// upstream configured — `resolveBaseBranch` then tries the
// main-worktree fallback, and `branch_scoped` ultimately falls back
// to the static `dump.path`.
//
// Why upstream: treeman creates worktrees with
// `git worktree add -b feature/x <path> origin/<base>`. Git's
// default `branch.autoSetupMerge=true` sets feature/x's upstream to
// `origin/<base>` when the start point is a remote-tracking branch,
// so `@{upstream}` recovers the base. Branches cut from a purely
// local ref (no remote-tracking start point) won't have an upstream;
// those fall through to the main-worktree fallback below.
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

// resolveBaseBranch returns the local branch a new worktree's
// branch_scoped seed should mirror. Two-tier resolution:
//
//  1. Tracked upstream (`baseBranchOf`). Hits when treeman created
//     the worktree off a remote-tracking start point — `@{upstream}`
//     points to it directly.
//  2. Main-worktree branch + merge-base sanity. Hits when the
//     feature branch has no upstream (cut from a local ref, GitFlow
//     branch where `branch.<name>.merge` points back at itself, or a
//     fresh `git worktree add -b X` without a start point). The main
//     worktree's active branch is the canonical "parent" candidate;
//     `git merge-base newBranch mainBranch` proves shared history
//     before we seed from a potentially unrelated DB.
//
// Main-wt lookup: prefer the enrolled `LookupMainWorktree` row
// (`main_worktree.enabled: true` repos). Fall back to the repo
// root's checkout for repos that don't enroll. Mirrors the same
// split `resolveBaseSourceDB` uses for the post-resolve render path.
//
// Issue #7 regression fix: GitFlow feature branches off `develop`
// were getting `seed:empty` because their `branch.<name>.merge`
// pointed at `develop` on the remote but the worktree's
// `@{upstream}` was unset. The main-wt fallback fills that gap.
func resolveBaseBranch(ctx context.Context, st *store.Store, repoRoot string, repoID int64, newBranch string) string {
	// A pushed feature branch tracks its OWN remote (`git push -u`
	// sets `@{upstream}` to `origin/<newBranch>`), so baseBranchOf
	// returns newBranch itself. That is not a base — discard it and
	// fall through to the main-worktree fallback so the branch still
	// seeds from `develop`. Without the `b != newBranch` guard every
	// pushed feature branch silently degraded to `seed:empty`.
	if b := baseBranchOf(ctx, repoRoot, newBranch); b != "" && b != newBranch {
		return b
	}
	mainBranch := lookupMainBranch(ctx, st, repoRoot, repoID)
	if mainBranch == "" || mainBranch == newBranch {
		return ""
	}
	// Shared history check — without it we'd happily seed an
	// orphan branch from an unrelated DB. `git merge-base` returns
	// non-empty stdout + exit 0 iff a common ancestor exists.
	mb, err := gitcmd.String(ctx, repoRoot, "merge-base", newBranch, mainBranch)
	if err != nil || strings.TrimSpace(mb) == "" {
		return ""
	}
	return mainBranch
}

// lookupMainBranch returns the local branch the main worktree is
// currently on. Enrolled (`main_worktree.enabled: true`) repos
// expose this via the store; un-enrolled repos still have a repo-root
// checkout we can read directly with `git rev-parse`.
func lookupMainBranch(ctx context.Context, st *store.Store, repoRoot string, repoID int64) string {
	if st != nil && repoID > 0 {
		row, err := st.LookupMainWorktree(ctx, repoID)
		if err == nil && !row.Deleted && row.Branch != "" {
			return row.Branch
		}
	}
	out, err := gitcmd.String(ctx, repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	cur := strings.TrimSpace(out)
	if cur == "HEAD" { // detached
		return ""
	}
	return cur
}

// stripRemotePrefix removes a leading `<remote>/` from `ref` when the
// first path component names a configured git remote. `origin/develop`
// → `develop`; a local ref like `develop` is returned unchanged.
func stripRemotePrefix(ctx context.Context, repoRoot, ref string) string {
	before, after, ok := strings.Cut(ref, "/")
	if !ok {
		return ref
	}
	candidate := before
	remotes, err := gitcmd.Output(ctx, repoRoot, "remote")
	if err != nil {
		return ref
	}
	for r := range strings.SplitSeq(strings.TrimSpace(string(remotes)), "\n") {
		if strings.TrimSpace(r) == candidate {
			return after
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
	baseBranch := resolveBaseBranch(ctx, st, repoRoot, repoID, newBranch)
	if baseBranch != "" {
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
	}

	// The resolved base branch is not checked out in any worktree or at the
	// repo root — the common case being a dev box whose repo root is parked
	// on some other feature branch while the real base (e.g. `develop`)
	// exists only as a ref. The main worktree's branch-scoped name is bare
	// (no `{slug}`), so its live DB is branch-agnostic and a valid seed for
	// any branch that shares history with it. Fall back to it instead of
	// cold-seeding an empty schema (the branch_scoped app DB has no
	// `dump.path`, so "no source" otherwise means "empty database").
	return mainWorktreeBaseDB(ctx, st, cfg, repoRoot, repoID, dbIdx, scope, newBranch)
}

// mainWorktreeBaseDB resolves the main worktree's active database for
// databases[dbIdx] as a last-resort seed source for newBranch, gated on
// shared git history. It backstops resolveBaseSourceDB when the resolved
// base branch is not independently checked out anywhere treeman can read
// its DB. Returns ("", false, nil) when there is no usable main worktree or
// the branches are unrelated, leaving the caller to fall through to
// `dump.path` (or an empty seed when none is configured).
func mainWorktreeBaseDB(
	ctx context.Context,
	st *store.Store,
	cfg *config.Config,
	repoRoot string,
	repoID int64,
	dbIdx int,
	scope bsScope,
	newBranch string,
) (string, bool, error) {
	mainBranch := lookupMainBranch(ctx, st, repoRoot, repoID)
	if mainBranch == "" || mainBranch == newBranch {
		return "", false, nil
	}
	// Shared-history guard, mirroring resolveBaseBranch's tier-2 check — never
	// seed a branch from an unrelated DB.
	mb, err := gitcmd.String(ctx, repoRoot, "merge-base", newBranch, mainBranch)
	if err != nil || strings.TrimSpace(mb) == "" {
		// `merge-base` exits non-zero for unrelated branches; treat that (and
		// an empty result) as "no shared history", not a propagatable error.
		return "", false, nil //nolint:nilerr // unrelated branch is not an error here
	}
	// Prefer the enrolled main worktree row's path; fall back to the repo
	// root checkout for repos that don't enroll a main worktree.
	path := repoRoot
	if row, lerr := st.LookupMainWorktree(ctx, repoID); lerr == nil && !row.Deleted && row.Path != "" {
		path = row.Path
	}
	return renderMainBaseDB(cfg, dbIdx, scope, path, mainBranch)
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
