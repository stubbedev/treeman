package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	wtpkg "github.com/stubbedev/treeman/internal/wt"
)

// secretPatterns matches common credential shapes that may leak into
// hook stdout/stderr or event payloads. Anything matched is replaced
// with `***REDACTED***` before being returned to MCP clients (which
// forward the value to an LLM).
//
// Conservative by design — false positives just hide a token; false
// negatives leak it. Project-specific patterns can be appended here.
var secretPatterns = []*regexp.Regexp{
	// URI userinfo: scheme://user:password@host
	regexp.MustCompile(`([a-z][a-z0-9+.-]*://)([^:/@\s]+):([^@\s]+)@`),
	// KEY=VALUE for common secret-bearing variable names
	regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|auth)[\s]*[:=][\s]*['"]?([^\s'"\n]+)`),
	// AWS access key id
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	// GitHub PATs
	regexp.MustCompile(`\bghp_[A-Za-z0-9]{36,}\b`),
	regexp.MustCompile(`\bgho_[A-Za-z0-9]{36,}\b`),
	regexp.MustCompile(`\bghs_[A-Za-z0-9]{36,}\b`),
	// Generic JWT
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
}

// redactSecrets scrubs known-shaped credentials from a string. Safe
// to call on already-redacted values; idempotent.
func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	out := s
	out = secretPatterns[0].ReplaceAllString(out, "$1$2:***REDACTED***@")
	out = secretPatterns[1].ReplaceAllString(out, "$1=***REDACTED***")
	for _, re := range secretPatterns[2:] {
		out = re.ReplaceAllString(out, "***REDACTED***")
	}
	return out
}

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

// writeMCPEvent records that an MCP tool performed `action` against
// `repoID` (optional). Best-effort: failures are swallowed so an
// audit-log glitch doesn't fail the underlying operation. All
// MCP-originated events share the `mcp_` event_type prefix and a
// `mcp=true` payload key so `logs grep mcp_` produces a clean
// audit trail.
func writeMCPEvent(ctx context.Context, action, message string, repoID int64, payload map[string]string) {
	st, err := openStore(ctx)
	if err != nil {
		return
	}
	defer func() { _ = st.Close() }()
	if payload == nil {
		payload = map[string]string{}
	}
	payload["mcp"] = "true"
	_ = st.WriteEvent(ctx, store.LevelInfo, "mcp_"+action, message, repoID, 0, "", 0, payload)
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
	st, err := openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	// Route through ResolveIdentity so MCP matches the daemon + CLI: the
	// main-worktree overlay is applied (bare active DB name) and a linked
	// worktree's slug is its branch-independent path slug. Required for
	// branch_scoped to keep a stable active namespace across an in-worktree
	// checkout — bare slug.For(wt, branch) would churn the namespace.
	id, err := wtpkg.ResolveIdentity(ctx, st, &cfg, repoRoot, wt, branch, repoID)
	if err != nil {
		return nil, err
	}
	return prepare.Run(ctx, &cfg, wt, id.Slug, st, repoID, id.WtID, captureEnv())
}

// runDbReset is the self-contained equivalent of cmd's
// RunDbResetOnWorktree: drops every branch_scoped database's active
// namespace + current durable copy, then re-runs prepare so each is
// re-seeded from the live base branch. engineFilter (lowercased)
// restricts the reset to one engine family when non-empty. Returns only
// the branch_scoped outcomes the reset actually re-seeded.
func runDbReset(ctx context.Context, worktree, repoOverride, engineFilter string) ([]prepare.Outcome, error) {
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
	st, err := openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	// Same identity routing as runPrepare — reset must operate on the very
	// active namespace the swap lifecycle created (bare on the main worktree).
	id, err := wtpkg.ResolveIdentity(ctx, st, &cfg, repoRoot, wt, branch, repoID)
	if err != nil {
		return nil, err
	}
	if err := prepare.ResetBranchScoped(ctx, &cfg, wt, repoID, id.WtID, st, engineFilter); err != nil {
		return nil, err
	}
	outs, err := prepare.Run(ctx, &cfg, wt, id.Slug, st, repoID, id.WtID, captureEnv())
	if err != nil {
		return outs, err
	}
	// Surface only the branch_scoped databases the reset touched, matched on
	// the canonical engine family so an alias (mariadb/postgresql/…) and an
	// `--engine` filter written as the family name both line up.
	filterLabel := engineFilter
	if engineFilter != "" {
		if fam, ok := engine.Canonical(engineFilter); ok {
			filterLabel = string(fam)
		}
	}
	var seeded []prepare.Outcome
	for _, o := range outs {
		for _, d := range cfg.Databases {
			if !d.BranchScoped {
				continue
			}
			fam, ok := engine.Canonical(d.Engine)
			if !ok {
				continue
			}
			label := string(fam)
			if filterLabel != "" && label != filterLabel {
				continue
			}
			if label == o.Engine {
				seeded = append(seeded, o)
				break
			}
		}
	}
	return seeded, nil
}

// runBranchScopedStatus reports the swap state of a worktree's
// branch_scoped databases. Routes through ResolveIdentity so the active
// namespace renders the same name the swap lifecycle uses (bare on the
// main worktree).
func runBranchScopedStatus(ctx context.Context, worktree, repoOverride string) ([]prepare.BranchScopedDB, error) {
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
	st, err := openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	id, err := wtpkg.ResolveIdentity(ctx, st, &cfg, repoRoot, wt, branch, repoID)
	if err != nil {
		return nil, err
	}
	return prepare.BranchScopedStatus(ctx, &cfg, repoRoot, wt, id.WtID, st)
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
	env := captureEnv()

	st, dbErr := openStore(ctx)
	var repoID, wtID int64
	var sl slug.Slug
	var isMain bool
	if dbErr == nil {
		defer func() { _ = st.Close() }()
		repoID, _ = st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
		id, idErr := wtpkg.ResolveIdentity(ctx, st, &cfg, repoRoot, wt, branch, repoID)
		if idErr != nil {
			return hooks.RunOutcome{}, idErr
		}
		sl, wtID, isMain = id.Slug, id.WtID, id.IsMain
	} else {
		// No store reachable — fall back to a path-hash slug just so
		// the hook env has something. $TREEMAN_IS_MAIN reads "0".
		sl = slug.For(wt, branch)
	}

	var entries []config.Action
	switch phase {
	case "on-create-before-engines":
		entries = cfg.Hooks.OnCreateBeforeEngines
	case "on-create-after-engines":
		entries = cfg.Hooks.OnCreateAfterEngines
	case "on-delete-before-engines":
		entries = cfg.Hooks.OnDeleteBeforeEngines
	case "on-delete-after-engines":
		entries = cfg.Hooks.OnDeleteAfterEngines
	case "on-checkout":
		entries = cfg.Hooks.OnCheckout
	default:
		return hooks.RunOutcome{}, fmt.Errorf("unknown phase %q (want on-create-before-engines|on-create-after-engines|on-delete-before-engines|on-delete-after-engines|on-checkout)", phase)
	}

	started := hooks.EmitHookStart(ctx, st, repoID, wtID, phase, len(entries))
	out, rErr := hooks.RunHooks(ctx, phase, entries, repoRoot, wt, sl.Value, isMain, env, true)
	hooks.PersistOutcome(ctx, st, repoID, wtID, phase, started, time.Now().UnixMilli(), out)
	return out, rErr
}
