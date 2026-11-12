package mcp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/fspath"
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
	wtpkg "github.com/stubbedev/treeman/internal/wt"
)

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
//
// An unresolvable non-empty argument is an error: the absolute form is
// still returned (callers that only need a path for a slug computation
// may ignore it), but every caller that acts on the worktree must fail
// instead of operating on <cwd>/<name>.
func resolveWorktree(ctx context.Context, path string) (wt, branch string, err error) {
	cwd, cwdErr := requestCwd(ctx)
	if path == "" {
		if cwdErr != nil {
			return "", "", fmt.Errorf("resolve the worktree: %w", cwdErr)
		}
		wt = fspath.Canonical(cwd)
		return wt, detectBranch(wt), nil
	}
	switch {
	case filepath.IsAbs(path):
		wt = filepath.Clean(path)
	case cwdErr != nil:
		return "", "", fmt.Errorf("resolve worktree %q against the workspace root: %w", path, cwdErr)
	default:
		wt = filepath.Join(cwd, path)
	}
	fi, statErr := os.Stat(wt)
	switch {
	case statErr == nil && fi.IsDir():
		wt = fspath.Canonical(wt)
		return wt, detectBranch(wt), nil
	case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
		return "", "", fmt.Errorf("resolve worktree %q: %w", path, statErr)
	}
	if cwdErr == nil {
		if repoRoot, rootErr := gitenv.MainRoot(ctx, cwd); rootErr == nil {
			p, ok, err := wtpkg.LookupWorktree(ctx, repoRoot, path, wtpkg.NoopSink{})
			if err != nil {
				return "", "", err
			}
			if ok {
				return p, detectBranch(p), nil
			}
		}
	}
	return wt, detectBranch(wt), &noWorktreeMatchError{name: path}
}

type noWorktreeMatchError struct {
	name string
}

func (e *noWorktreeMatchError) Error() string {
	return fmt.Sprintf("no worktree matches %q (try worktree_list)", e.name)
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

type worktreeScope struct {
	wt, repoRoot string
	cfg          config.Config
	st           *store.Store
	repoID       int64
	id           wtpkg.Identity
}

func openWorktreeScope(ctx context.Context, worktree, repoOverride string) (*worktreeScope, error) {
	wt, branch, err := resolveWorktree(ctx, worktree)
	if err != nil {
		return nil, err
	}
	repoRoot, err := worktreeRepoRoot(ctx, wt, repoOverride)
	if err != nil {
		return nil, err
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, err
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("open the store for worktree %s: %w", wt, err)
	}
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("register repo %s: %w", repoRoot, err)
	}
	id, err := wtpkg.ResolveIdentity(ctx, st, &cfg, repoRoot, wt, branch, repoID)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return &worktreeScope{wt: wt, repoRoot: repoRoot, cfg: cfg, st: st, repoID: repoID, id: id}, nil
}

func worktreeRepoRoot(ctx context.Context, wt, repoOverride string) (string, error) {
	if repoOverride != "" {
		return resolveRepo(ctx, repoOverride)
	}
	return gitenv.MainRoot(ctx, wt)
}

func runPrepare(ctx context.Context, worktree, repoOverride string) ([]prepare.Outcome, error) {
	sc, err := openWorktreeScope(ctx, worktree, repoOverride)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sc.st.Close() }()
	outs, err := prepare.Run(ctx, &sc.cfg, sc.wt, sc.id.Slug, sc.st, sc.repoID, sc.id.WtID, captureEnv())
	if err != nil {
		return outs, err
	}
	if err := runPostEngineHooks(ctx, sc); err != nil {
		return outs, err
	}
	return outs, nil
}

// runPostEngineHooks fires create-after-engines after an MCP-driven
// prepare/db_reset, mirroring the daemon pipeline's post-hook step —
// these paths mutate engine content just like finalize, so hooks that
// react to fresh data (cache flushes) must run here too.
func runPostEngineHooks(ctx context.Context, sc *worktreeScope) error {
	trigger := hooks.CreateAfterEngines
	actions := trigger.Actions(&sc.cfg.Hooks)
	if len(actions) == 0 {
		return nil
	}
	started := hooks.EmitHookStart(ctx, sc.st, sc.repoID, sc.id.WtID, trigger, len(actions))
	out, err := hooks.RunHooks(ctx, trigger, actions, sc.repoRoot, sc.wt, sc.id.Slug.Value, sc.id.IsMain, captureEnv(), true)
	runIDs := hooks.PersistOutcome(ctx, sc.st, sc.repoID, sc.id.WtID, trigger, started, time.Now().UnixMilli(), out)
	if err != nil {
		return fmt.Errorf("%s: %w", trigger, err)
	}
	return hooks.FirstFailureError(trigger, out, runIDs)
}

// runDbReset is the self-contained equivalent of cmd's
// RunDbResetOnWorktree: drops every branch_scoped database's active
// namespace + current durable copy, then re-runs prepare so each is
// re-seeded from the live base branch. engineFilter (lowercased)
// restricts the reset to one engine family when non-empty. Returns only
// the branch_scoped outcomes the reset actually re-seeded.
func runDbReset(ctx context.Context, worktree, repoOverride, engineFilter string) ([]prepare.Outcome, error) {
	sc, err := openWorktreeScope(ctx, worktree, repoOverride)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sc.st.Close() }()
	if err := prepare.ResetBranchScoped(ctx, &sc.cfg, sc.wt, sc.repoID, sc.id.WtID, sc.st, engineFilter); err != nil {
		return nil, err
	}
	outs, err := prepare.Run(ctx, &sc.cfg, sc.wt, sc.id.Slug, sc.st, sc.repoID, sc.id.WtID, captureEnv())
	if err != nil {
		return outs, err
	}
	if err := runPostEngineHooks(ctx, sc); err != nil {
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
		for _, d := range sc.cfg.Databases {
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
	sc, err := openWorktreeScope(ctx, worktree, repoOverride)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sc.st.Close() }()
	return prepare.SaveBranchScoped(ctx, &sc.cfg, sc.wt, sc.repoID, sc.id.WtID, sc.st, engineFilter)
}

func runBranchScopedStatus(ctx context.Context, worktree, repoOverride string) ([]prepare.BranchScopedDB, error) {
	sc, err := openWorktreeScope(ctx, worktree, repoOverride)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sc.st.Close() }()
	return prepare.BranchScopedStatus(ctx, &sc.cfg, sc.repoRoot, sc.wt, sc.id.WtID, sc.st)
}

// confirmInputID keys the confirmation prompt in a tool call's
// InputRequests / InputResponses.
const confirmInputID = "confirm"

// confirmDestructive gates a destructive tool call. It returns
// (nil, "") when the action may run, (nil, reason) when it must not,
// and (ask, "") when the user has to be asked first: the handler
// returns ask as its result and the SDK gets the prompt answered (by
// the client on protocol 2026-07-28+, by a server-sent elicitation on
// older ones) and calls the handler again with the answer in
// InputResponses.
//
// ack=true means the agent already has the user's approval. A client
// that cannot prompt is refused with a reason naming ack=true, the
// same contract as the CLI refusing a destructive prompt without a
// TTY.
func confirmDestructive(req *mcpsdk.CallToolRequest, ack bool, message string) (*mcpsdk.CallToolResult, string) {
	if ack {
		return nil, ""
	}
	if req != nil && req.Params != nil {
		if res, ok := req.Params.InputResponses[confirmInputID].(*mcpsdk.ElicitResult); ok {
			if res.Action == "accept" {
				return nil, ""
			}
			return nil, "user answered " + res.Action
		}
	}
	if !canElicit(req) {
		return nil, "confirmation required: " + message + " This client cannot prompt; re-run with ack=true once the user has approved."
	}
	return &mcpsdk.CallToolResult{InputRequests: mcpsdk.InputRequestMap{
		confirmInputID: &mcpsdk.ElicitParams{
			Mode:            "form",
			Message:         message,
			RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}}, ""
}

func canElicit(req *mcpsdk.CallToolRequest) bool {
	if req == nil || req.Session == nil {
		return false
	}
	ip := req.Session.InitializeParams()
	return ip != nil && ip.Capabilities != nil && ip.Capabilities.Elicitation != nil
}

// runHookPhase synchronously executes one hook phase. Mirrors cmd's
// RunHookPhase but with no CLI surface. envOverrides (when non-nil)
// are merged on top of the captured os.Environ() so an MCP caller can
// tweak one var (e.g. retry a flaky setup with `DEBUG=1`) without
// editing .treeman.yaml.
func runHookPhase(ctx context.Context, trigger hooks.Trigger, worktree string, envOverrides map[string]string) (hooks.RunOutcome, error) {
	sc, err := openWorktreeScope(ctx, worktree, "")
	if err != nil {
		return hooks.RunOutcome{}, err
	}
	defer func() { _ = sc.st.Close() }()
	env := captureEnv()
	maps.Copy(env, envOverrides)
	entries := trigger.Actions(&sc.cfg.Hooks)
	started := hooks.EmitHookStart(ctx, sc.st, sc.repoID, sc.id.WtID, trigger, len(entries))
	out, rErr := hooks.RunHooks(ctx, trigger, entries, sc.repoRoot, sc.wt, sc.id.Slug.Value, sc.id.IsMain, env, true)
	hooks.PersistOutcome(ctx, sc.st, sc.repoID, sc.id.WtID, trigger, started, time.Now().UnixMilli(), out)
	return out, rErr
}
