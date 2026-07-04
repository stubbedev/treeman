package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

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
	regexp.MustCompile(
		`(?i)\b(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|auth)[\s]*[:=][\s]*['"]?([^\s'"\n]+)`,
	),
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

// resolveRepo returns the absolute repo root for the current request.
// The source is chosen by precedence (highest first):
//
//   - explicit override (a tool's `repo` param)
//   - the request's workspace root (HTTP header or MCP roots/list),
//     carried on ctx by installContextMiddleware
//   - the process cwd (the stdio transport's single-client default)
//
// The header/roots step is what lets one HTTP server instance serve many
// concurrent clients, each pinned to its own checkout, without a shared
// process cwd. Over stdio no resolver is set, so this falls through to
// os.Getwd() exactly as before.
func resolveRepo(ctx context.Context, override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		return gitenv.MainRoot(ctx, abs)
	}
	dir, err := requestCwd(ctx)
	if err != nil {
		return "", err
	}
	return gitenv.MainRoot(ctx, dir)
}

// requestCwd is the directory a bare (override-less) worktree/repo lookup
// resolves against for this request: the request's workspace root when one
// was supplied (HTTP header or MCP roots), else the process cwd.
//
// Over HTTP there is no meaningful process cwd — it would be wherever the
// shared daemon was launched (e.g. a systemd WorkingDirectory), which has
// nothing to do with the requesting client. So when no root was supplied
// over HTTP this errors rather than silently resolving against the daemon's
// directory. Over stdio (one client per process) os.Getwd() is the right
// default, as before.
func requestCwd(ctx context.Context) (string, error) {
	if root := rootDirFromCtx(ctx); root != "" {
		return filepath.Abs(root)
	}
	if r := resolverFrom(ctx); r != nil && r.httpMode {
		return "", errors.New(
			"no workspace root for this request: pass a repo/worktree argument, set the X-Repo-Root header, or expose an MCP root",
		)
	}
	return os.Getwd()
}

// resolveWorktree maps the MCP `worktree` argument to an absolute
// worktree path plus its branch. The argument may be:
//
//   - ""                                 → the current working directory
//   - a registered slug / branch / basename → that worktree's path
//   - a filesystem path                  → canonicalised to absolute
//
// A registered name is resolved against the repo inferred from cwd and
// wins over the raw-path interpretation, so a bare name like "develop"
// resolves to the tracked worktree instead of being mis-read as
// <cwd>/develop — a non-existent path that downstream registration
// would otherwise persist as a phantom worktree row (plus per-branch
// databases that no teardown reclaims). Branch is read from .git/HEAD.
func resolveWorktree(ctx context.Context, path string) (wt, branch string) {
	if path == "" {
		// Default to the request's workspace root (HTTP header / MCP
		// roots) and only then the process cwd — see resolveRepo.
		path, _ = requestCwd(ctx)
		wt, _ = filepath.Abs(path)
		return wt, detectBranch(wt)
	}
	wt, _ = filepath.Abs(path)
	// An existing directory is an explicit path the caller chose —
	// honour it verbatim.
	if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
		return wt, detectBranch(wt)
	}
	// Not an on-disk path: try resolving it as a registered worktree
	// name (slug / branch / basename) against the repo inferred from
	// the request's workspace root (or cwd over stdio).
	if cwd, err := requestCwd(ctx); err == nil {
		if repoRoot, err := gitenv.MainRoot(ctx, cwd); err == nil {
			if p, ok := wtpkg.LookupWorktree(ctx, repoRoot, path, wtpkg.NoopSink{}); ok {
				return p, detectBranch(p)
			}
		}
	}
	// Unresolved name and no such path: return the absolute form. The
	// ResolveIdentity guard refuses to register a non-worktree path, so
	// a typo surfaces as a clear error instead of a phantom row.
	return wt, detectBranch(wt)
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
	if after, ok := strings.CutPrefix(s, pfx); ok {
		return after
	}
	return ""
}

// captureEnv snapshots os.Environ() so hook + prepare subprocesses
// see the invoking shell's $PATH.
func captureEnv() map[string]string {
	env := os.Environ()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		for i := range len(kv) {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}

// writeMCPEvent records that an MCP tool emitted `eventType` (a
// store.EvtMCP* constant) against `repoID` (optional). Best-effort:
// failures are swallowed so an audit-log glitch doesn't fail the
// underlying operation. All MCP-originated events share the `mcp:`
// prefix and a `mcp=true` payload key so `logs grep mcp:` produces a
// clean audit trail.
func writeMCPEvent(ctx context.Context, eventType, message string, repoID int64, payload map[string]string) {
	st, err := openStore(ctx)
	if err != nil {
		return
	}
	defer func() { _ = st.Close() }()
	if payload == nil {
		payload = map[string]string{}
	}
	payload["mcp"] = "true"
	_ = st.WriteEvent(ctx, store.LevelInfo, eventType, message, repoID, 0, "", 0, payload)
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
	wt, branch := resolveWorktree(ctx, worktree)
	repoRoot, err := resolveRepo(ctx, repoOverride)
	if err != nil {
		repoRoot, err = gitenv.MainRoot(ctx, wt)
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
	outs, err := prepare.Run(ctx, &cfg, wt, id.Slug, st, repoID, id.WtID, captureEnv())
	if err != nil {
		return outs, err
	}
	if err := runPostEngineHooks(ctx, st, &cfg, repoRoot, wt, id.Slug.Value, id.IsMain, repoID, id.WtID); err != nil {
		return outs, err
	}
	return outs, nil
}

// runPostEngineHooks fires create-after-engines after an MCP-driven
// prepare/db_reset, mirroring the daemon pipeline's post-hook step —
// these paths mutate engine content just like finalize, so hooks that
// react to fresh data (cache flushes) must run here too.
func runPostEngineHooks(
	ctx context.Context,
	st *store.Store,
	cfg *config.Config,
	repoRoot, wtRoot, slugVal string,
	isMain bool,
	repoID, wtID int64,
) error {
	actions := cfg.Hooks.OnCreateAfterEngines
	if len(actions) == 0 {
		return nil
	}
	started := hooks.EmitHookStart(ctx, st, repoID, wtID, "create-after-engines", len(actions))
	out, err := hooks.RunHooks(ctx, "create-after-engines", actions, repoRoot, wtRoot, slugVal, isMain, captureEnv(), true)
	runIDs := hooks.PersistOutcome(ctx, st, repoID, wtID, "create-after-engines", started, time.Now().UnixMilli(), out)
	if err != nil {
		return fmt.Errorf("create-after-engines: %w", err)
	}
	return hooks.FirstFailureError("create-after-engines", out, runIDs)
}

// runDbReset is the self-contained equivalent of cmd's
// RunDbResetOnWorktree: drops every branch_scoped database's active
// namespace + current durable copy, then re-runs prepare so each is
// re-seeded from the live base branch. engineFilter (lowercased)
// restricts the reset to one engine family when non-empty. Returns only
// the branch_scoped outcomes the reset actually re-seeded.
func runDbReset(ctx context.Context, worktree, repoOverride, engineFilter string) ([]prepare.Outcome, error) {
	wt, branch := resolveWorktree(ctx, worktree)
	repoRoot, err := resolveRepo(ctx, repoOverride)
	if err != nil {
		repoRoot, err = gitenv.MainRoot(ctx, wt)
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
	if err := runPostEngineHooks(ctx, st, &cfg, repoRoot, wt, id.Slug.Value, id.IsMain, repoID, id.WtID); err != nil {
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

// runDbSave captures a worktree's branch_scoped active namespaces into
// the current branch's durable copies without switching branches. Same
// identity routing as runDbReset.
func runDbSave(ctx context.Context, worktree, repoOverride, engineFilter string) ([]prepare.BranchScopedSave, error) {
	wt, branch := resolveWorktree(ctx, worktree)
	repoRoot, err := resolveRepo(ctx, repoOverride)
	if err != nil {
		repoRoot, err = gitenv.MainRoot(ctx, wt)
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
	return prepare.SaveBranchScoped(ctx, &cfg, wt, repoID, id.WtID, st, engineFilter)
}

// runBranchScopedStatus reports the swap state of a worktree's
// branch_scoped databases. Routes through ResolveIdentity so the active
// namespace renders the same name the swap lifecycle uses (bare on the
// main worktree).
func runBranchScopedStatus(ctx context.Context, worktree, repoOverride string) ([]prepare.BranchScopedDB, error) {
	wt, branch := resolveWorktree(ctx, worktree)
	repoRoot, err := resolveRepo(ctx, repoOverride)
	if err != nil {
		repoRoot, err = gitenv.MainRoot(ctx, wt)
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

// confirmDestructive asks the user via MCP elicitation before running
// a destructive action. Returns (true, "") when the agent should
// proceed: dry_run=true short-circuits to true, ack=true skips
// elicitation, an elicitation error (client doesn't support it)
// falls through to true, and an "accept" response returns true. Only
// an explicit "decline"/"cancel" stops the action.
//
// The intent: clients that DO support elicitation (Claude Desktop,
// etc.) get a confirmation pop-up; clients that don't are unchanged.
// Agents that want to bypass confirmation entirely can pass ack=true.
func confirmDestructive(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	dryRun, ack bool,
	message string,
) (bool, string) {
	if dryRun || ack {
		return true, ""
	}
	if req == nil || req.Session == nil {
		return true, ""
	}
	res, err := req.Session.Elicit(ctx, &mcpsdk.ElicitParams{
		Mode:    "confirmation",
		Message: message,
	})
	if err != nil || res == nil {
		// Client doesn't support elicitation (or it errored). Fall
		// through — refusing would break non-interactive agents that
		// can't even ask the question.
		return true, ""
	}
	switch res.Action {
	case "accept":
		return true, ""
	case "decline":
		return false, "user declined"
	case "cancel":
		return false, "user cancelled"
	default:
		return false, "user action: " + res.Action
	}
}

// runHookPhase synchronously executes one hook phase. Mirrors cmd's
// RunHookPhase but with no CLI surface. envOverrides (when non-nil)
// are merged on top of the captured os.Environ() so an MCP caller can
// tweak one var (e.g. retry a flaky setup with `DEBUG=1`) without
// editing .treeman.yaml.
func runHookPhase(ctx context.Context, phase, worktree string, envOverrides map[string]string) (hooks.RunOutcome, error) {
	wt, branch := resolveWorktree(ctx, worktree)
	repoRoot, err := gitenv.MainRoot(ctx, wt)
	if err != nil {
		return hooks.RunOutcome{}, err
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return hooks.RunOutcome{}, err
	}
	env := captureEnv()
	maps.Copy(env, envOverrides)

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
	case "create-before-engines":
		entries = cfg.Hooks.OnCreateBeforeEngines
	case "create-after-engines":
		entries = cfg.Hooks.OnCreateAfterEngines
	case "delete-before-engines":
		entries = cfg.Hooks.OnDeleteBeforeEngines
	case "delete-after-engines":
		entries = cfg.Hooks.OnDeleteAfterEngines
	case "checkout":
		entries = cfg.Hooks.OnCheckout
	default:
		return hooks.RunOutcome{}, fmt.Errorf(
			"unknown phase %q (want create-before-engines|create-after-engines|delete-before-engines|delete-after-engines|checkout)",
			phase,
		)
	}

	started := hooks.EmitHookStart(ctx, st, repoID, wtID, phase, len(entries))
	out, rErr := hooks.RunHooks(ctx, phase, entries, repoRoot, wt, sl.Value, isMain, env, true)
	hooks.PersistOutcome(ctx, st, repoID, wtID, phase, started, time.Now().UnixMilli(), out)
	return out, rErr
}
