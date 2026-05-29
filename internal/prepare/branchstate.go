package prepare

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"time"

	"lukechampine.com/blake3"

	"github.com/stubbedev/treeman/internal/config"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/migrations/runner"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// ─── engine-agnostic namespace adapter ──────────────────────────────
//
// A "namespace" is the unit a branch-scoped database swaps. For
// name-scoped engines (mysql/postgres/mongo) it's a database name; for
// prefix-scoped engines (redis/es) it's a key/index prefix. Every
// driver already exposes the four primitives below — the adapter just
// erases the name-vs-prefix distinction so the swap orchestrator is
// written once.

// nsDriver is the slice of an engine driver the swap lifecycle needs.
// Capture/Restore treat their SOURCE argument read-only and drop only
// the TARGET, so the active namespace is never lost without first being
// captured.
type nsDriver interface {
	Exists(ctx context.Context, ns string) (bool, error)
	Capture(ctx context.Context, active, durable string) error // active → durable
	Restore(ctx context.Context, durable, active string) error // durable → active (drops active first)
	Empty(ctx context.Context, active string) error            // reset active to an empty, present namespace
	Drop(
		ctx context.Context,
		ns string,
	) error // remove the namespace entirely (EXACT for name-scoped; prefix for prefix-scoped)
	DropDurable(ctx context.Context, durable string) error
}

// bsScope distinguishes how an engine carves up isolation.
type bsScope int

const (
	scopeName   bsScope = iota // active namespace = rendered name_template (DB name)
	scopePrefix                // active namespace = rendered key_prefix (key/index prefix)
)

// branchEngine bundles a connected adapter with its scope so the
// orchestrator can resolve active + durable namespace names.
type branchEngine struct {
	drv    nsDriver
	scope  bsScope
	engine string
}

// durable derives the treeman-managed namespace that holds `branch`'s
// data for this `active` namespace. Deterministic (a hash of the pair),
// so the engine itself is the source of truth for "does this branch's
// copy exist" — no SQLite bookkeeping required. The marker prefix is
// disjoint from both the active namespace and the fingerprint-template
// namespace (`_tm_`/`tm_`) so a Capture/Restore drop never collides.
func (b *branchEngine) durable(active, branch string) string {
	h := bsHash(active + "|" + branch)
	switch {
	case b.scope == scopePrefix && b.engine == "redis":
		return "_tmbs:" + h + ":"
	case b.scope == scopePrefix: // elasticsearch — index names can't start with `_`
		return "tmbs_" + h + "_"
	default:
		return "_tmbs_" + h
	}
}

// parentSeedHint appends actionable guidance to a parent-seed failure.
// Seeding a branch_scoped DB from its parent branch's LIVE database is
// a whole-database copy. Postgres implements that copy as
// `CREATE DATABASE … TEMPLATE parent`, which the server refuses while
// any other session is connected to `parent` ("is being accessed by
// other users") — the common case when the app or another worktree is
// pointed at the base-branch DB. Surface the fix rather than leaking the
// raw driver error.
func parentSeedHint(engine string, err error) string {
	if engine != "postgres" || err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "being accessed by other users") {
		return " (postgres copies the parent db with CREATE DATABASE … TEMPLATE, " +
			"which fails while other sessions are connected to it — close connections " +
			"to the base-branch database, or set `dump.path` so seeding uses the dump instead)"
	}
	return ""
}

func bsHash(s string) string {
	sum := blake3.Sum256([]byte(s))
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 16)
	for _, b := range sum[:8] {
		out = append(out, hexDigits[b>>4], hexDigits[b&0x0f])
	}
	return string(out)
}

// ─── per-engine adapters ────────────────────────────────────────────

type mysqlNS struct{ d *dbmysql.Driver }

func (a mysqlNS) Exists(ctx context.Context, ns string) (bool, error) {
	return a.d.DatabaseExists(ctx, ns)
}

func (a mysqlNS) Capture(ctx context.Context, active, durable string) error {
	return a.d.SnapshotCreate(ctx, active, durable)
}

func (a mysqlNS) Restore(ctx context.Context, durable, active string) error {
	return a.d.SnapshotRestore(ctx, durable, active)
}

func (a mysqlNS) Empty(ctx context.Context, active string) error {
	// EXACT drop, not DropMatching: a branch_scoped DB never fans out
	// into a clone family, and the active name (bare on the main
	// worktree) is a prefix of sibling worktrees' DBs.
	if err := a.d.DropDatabase(ctx, active); err != nil {
		return err
	}
	return a.d.EnsureDB(ctx, active)
}

func (a mysqlNS) Drop(ctx context.Context, ns string) error {
	return a.d.DropDatabase(ctx, ns)
}

func (a mysqlNS) DropDurable(ctx context.Context, durable string) error {
	return a.d.DropSnapshot(ctx, durable)
}

type postgresNS struct{ d *dbpostgres.Driver }

func (a postgresNS) Exists(ctx context.Context, ns string) (bool, error) {
	return a.d.DatabaseExists(ctx, ns)
}

func (a postgresNS) Capture(ctx context.Context, active, durable string) error {
	return a.d.SnapshotCreate(ctx, active, durable)
}

func (a postgresNS) Restore(ctx context.Context, durable, active string) error {
	return a.d.SnapshotRestore(ctx, durable, active)
}

func (a postgresNS) Empty(ctx context.Context, active string) error {
	// EXACT drop, not DropMatching — see mysqlNS.Empty.
	if err := a.d.DropDatabase(ctx, active); err != nil {
		return err
	}
	return a.d.EnsureDB(ctx, active)
}

func (a postgresNS) Drop(ctx context.Context, ns string) error {
	return a.d.DropDatabase(ctx, ns)
}

func (a postgresNS) DropDurable(ctx context.Context, durable string) error {
	return a.d.DropSnapshot(ctx, durable)
}

type mongoNS struct{ d *dbmongo.Driver }

func (a mongoNS) Exists(ctx context.Context, ns string) (bool, error) {
	return a.d.DatabaseExists(ctx, ns)
}

func (a mongoNS) Capture(ctx context.Context, active, durable string) error {
	return a.d.SnapshotCreate(ctx, active, durable)
}

func (a mongoNS) Restore(ctx context.Context, durable, active string) error {
	return a.d.SnapshotRestore(ctx, durable, active)
}

func (a mongoNS) Empty(ctx context.Context, active string) error {
	// EXACT drop, not DropMatching — see mysqlNS.Empty. Mongo creates
	// databases lazily on first write, so a dropped name is "empty".
	return a.d.DropDatabase(ctx, active)
}

func (a mongoNS) Drop(ctx context.Context, ns string) error {
	return a.d.DropDatabase(ctx, ns)
}

func (a mongoNS) DropDurable(ctx context.Context, durable string) error {
	return a.d.DropSnapshot(ctx, durable)
}

type redisNS struct{ d *dbredis.Driver }

func (a redisNS) Exists(ctx context.Context, ns string) (bool, error) {
	return a.d.PrefixExists(ctx, ns)
}

func (a redisNS) Capture(ctx context.Context, active, durable string) error {
	return a.d.SnapshotCreate(ctx, active, durable)
}

func (a redisNS) Restore(ctx context.Context, durable, active string) error {
	return a.d.SnapshotRestore(ctx, durable, active)
}

func (a redisNS) Empty(ctx context.Context, active string) error {
	_, err := a.d.DropPrefix(ctx, active)
	return err
}

func (a redisNS) Drop(ctx context.Context, ns string) error {
	_, err := a.d.DropPrefix(ctx, ns)
	return err
}

func (a redisNS) DropDurable(ctx context.Context, durable string) error {
	return a.d.DropSnapshot(ctx, durable)
}

type esNS struct{ d *dbes.Driver }

func (a esNS) Exists(ctx context.Context, ns string) (bool, error) {
	m, err := a.d.ListMatching(ctx, ns)
	return len(m) > 0, err
}

func (a esNS) Capture(ctx context.Context, active, durable string) error {
	return a.d.SnapshotCreate(ctx, active, durable)
}

func (a esNS) Restore(ctx context.Context, durable, active string) error {
	return a.d.SnapshotRestore(ctx, durable, active)
}

func (a esNS) Empty(ctx context.Context, active string) error {
	_, err := a.d.DropMatching(ctx, active)
	return err
}

func (a esNS) Drop(ctx context.Context, ns string) error {
	_, err := a.d.DropMatching(ctx, ns)
	return err
}

func (a esNS) DropDurable(ctx context.Context, durable string) error {
	return a.d.DropSnapshot(ctx, durable)
}

// branchScopeFor reports the scope + canonical engine label for a
// configured engine, ok=false for unrecognised engines.
func branchScopeFor(eng string) (bsScope, string, bool) {
	fam, ok := engine.Canonical(eng)
	if !ok {
		return scopeName, eng, false
	}
	if fam.Scope() == engine.ScopePrefix {
		return scopePrefix, string(fam), true
	}
	return scopeName, string(fam), true
}

// connectBranchEngine dials `engine` and wraps it in a branchEngine.
// Used by `treeman db reset`, which connects fresh (the prepare path
// builds the adapter from its already-connected driver instead). The
// returned close func releases the connection.
//
// Unrecognised engines return (nil, no-op, nil) so iteration over a
// mixed-engine config can skip non-participating entries quietly,
// BUT log a warning so a typo'd engine name doesn't disappear
// without a trace. Same observability principle as TeardownDatabases'
// unknown-engine arm.
func connectBranchEngine(ctx context.Context, cfg *config.Config, eng string) (*branchEngine, func(), error) {
	scope, label, ok := branchScopeFor(eng)
	if !ok {
		slog.Warn("connect branch engine: skipping unrecognised engine",
			"engine", eng, "allowed", engine.KnownList())
		return nil, func() {}, nil
	}
	switch label {
	case "mysql":
		if cfg.Connections.Mysql == nil {
			return nil, func() {}, errors.New("connections.mysql not configured")
		}
		drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
		if err != nil {
			return nil, func() {}, err
		}
		return &branchEngine{drv: mysqlNS{drv}, scope: scope, engine: label}, func() { _ = drv.Close() }, nil
	case "postgres":
		if cfg.Connections.Postgres == nil {
			return nil, func() {}, errors.New("connections.postgres not configured")
		}
		drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
		if err != nil {
			return nil, func() {}, err
		}
		return &branchEngine{drv: postgresNS{drv}, scope: scope, engine: label}, func() { _ = drv.Close() }, nil
	case "mongodb":
		if cfg.Connections.Mongodb == nil {
			return nil, func() {}, errors.New("connections.mongodb not configured")
		}
		drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
		if err != nil {
			return nil, func() {}, err
		}
		return &branchEngine{drv: mongoNS{drv}, scope: scope, engine: label}, func() { _ = drv.Close(ctx) }, nil
	case "redis":
		if cfg.Connections.Redis == nil {
			return nil, func() {}, errors.New("connections.redis not configured")
		}
		drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
		if err != nil {
			return nil, func() {}, err
		}
		return &branchEngine{drv: redisNS{drv}, scope: scope, engine: label}, func() { _ = drv.Close() }, nil
	case "elasticsearch":
		if cfg.Connections.Elasticsearch == nil {
			return nil, func() {}, errors.New("connections.elasticsearch not configured")
		}
		drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
		if err != nil {
			return nil, func() {}, err
		}
		return &branchEngine{drv: esNS{drv}, scope: scope, engine: label}, func() {}, nil
	default:
		return nil, func() {}, nil
	}
}

// activeNamespace renders the branch-INDEPENDENT active namespace for a
// branch-scoped database: the DB name / key prefix the app connects to,
// which must stay stable as the branch changes underneath it. It keys
// off the worktree's path (not the current branch) so an in-worktree
// `git checkout` doesn't rename the database.
//
// For the main worktree the caller passes a `cfg`/`d` that already has
// the main_worktree overlay applied (a bare, slug-free template), so
// the rendered name is constant regardless of the slug.
func activeNamespace(d config.DatabaseConfig, scope bsScope, worktreePath string) (string, error) {
	tmpl := d.NameTemplate
	if scope == scopePrefix {
		tmpl = d.KeyPrefix
	}
	stable := slug.For(worktreePath, "") // branch-independent
	return template.Render(tmpl, template.FromSlug(stable))
}

// branchScopedArgs bundles the inputs to runBranchScoped. The adapter
// (`eng`) is built from the prepare path's already-connected driver, so
// no second connection is opened. `loadDump` is optional — engines
// without a static-dump path (e.g. redis) pass nil.
type branchScopedArgs struct {
	cfg          *config.Config
	d            config.DatabaseConfig
	dbIdx        int
	tplCtx       template.Context
	worktreePath string
	st           *store.Store
	repoID       int64
	worktreeID   int64
	inheritedEnv map[string]string

	eng *branchEngine
	// loadDump loads the static fallback dump into the active namespace.
	loadDump func(ctx context.Context, active, dumpPath string) error
	// resolveParent, when non-nil, overrides the default git-upstream +
	// store base-branch resolver. Only tests set it; production leaves it
	// nil so `fill` uses resolveBaseSourceDB.
	resolveParent func(ctx context.Context, branch string) (string, bool, error)
}

// runBranchScoped is the unified swap lifecycle for one branch-scoped
// database. It runs on every prepare (create + HEAD-switch finalize):
//
//   - active doesn't exist → seed it (this branch's durable copy, else
//     the parent branch's live data, else dump, else empty).
//   - active exists but unmarked → adopt: back it up as this branch's
//     durable copy, leave the data in place (first-enable / migrating
//     an existing DB into the branch-scoped world).
//   - active exists, marked for a DIFFERENT branch → SWAP: capture the
//     old branch's data into its durable copy, then fill active with
//     this branch's data (durable resume, else parent, else leave the
//     just-captured data as the branch point for a new branch).
//   - active exists, marked for THIS branch → no-op (just migrate).
//
// Migrate runs afterward (idempotent). The `seed:` step runs only when
// the active namespace was built empty/from-dump — never after a
// durable/parent restore, which would double-insert.
func runBranchScoped(ctx context.Context, a branchScopedArgs) (Outcome, error) {
	started := time.Now()
	active, err := activeNamespace(a.d, a.eng.scope, a.worktreePath)
	if err != nil {
		return Outcome{}, fmt.Errorf("render active namespace: %w", err)
	}
	branch, _ := a.st.WorktreeBranch(ctx, a.worktreeID)
	if branch == "" {
		branch = "_detached"
	}

	old, hasOld, err := a.st.GetActiveBranch(ctx, a.worktreeID, active)
	if err != nil {
		return Outcome{}, err
	}
	exists, err := a.eng.drv.Exists(ctx, active)
	if err != nil {
		return Outcome{}, err
	}

	a.event(ctx, "prepare_start", fmt.Sprintf("engine=%s active=%s branch=%s branch_scoped", a.eng.engine, active, branch), nil)

	builtEmpty := false
	var decision string
	switch {
	case !exists:
		// Fresh: nothing in the active slot. Seed this branch's data.
		builtEmpty, decision, err = a.seedFresh(ctx, active, branch)
		if err != nil {
			return Outcome{}, err
		}

	case !hasOld:
		// Active exists but treeman never recorded who owns it — adopt
		// the current contents as THIS branch's data. Back it up; leave
		// the data in place. (First-enable on a pre-existing DB.)
		if err := a.eng.drv.Capture(ctx, active, a.eng.durable(active, branch)); err != nil {
			return Outcome{}, fmt.Errorf("adopt-capture %s: %w", active, err)
		}
		decision = "adopt"

	case old != branch:
		// Branch switch. Preserve the OLD branch's data, then fill the
		// active slot with THIS branch's data.
		decision, err = a.swapBranch(ctx, active, old, branch)
		if err != nil {
			return Outcome{}, err
		}

	default:
		// old == branch: same branch already loaded. Nothing to swap.
		decision = "noop"
	}

	if err := a.st.SetActiveBranch(ctx, a.repoID, a.worktreeID, active, branch, a.eng.engine); err != nil {
		return Outcome{}, fmt.Errorf("record active-branch marker: %w", err)
	}

	if a.d.Migrate != nil {
		if err := a.runStep(ctx, runner.FromMigrate(*a.d.Migrate), active, "migrate"); err != nil {
			return Outcome{}, err
		}
	}
	if a.d.Seed != nil && builtEmpty {
		if err := a.runStep(ctx, runner.FromSeed(*a.d.Seed), active, "seed"); err != nil {
			return Outcome{}, err
		}
	}

	ms := time.Since(started).Milliseconds()
	a.event(ctx, "prepare_done",
		fmt.Sprintf("branch_scoped decision=%s active=%s branch=%s duration=%dms", decision, active, branch, ms),
		map[string]string{"duration_ms": strconv.FormatInt(ms, 10), "decision": decision})

	return Outcome{Engine: a.eng.engine, SourceDB: active, Decision: decision}, nil
}

// seedFresh handles the `!exists` arm of runBranchScoped: the active
// slot is empty, so seed this branch's data (durable/parent via fill,
// else dump, else empty). Returns (builtEmpty, decision). Extracted
// verbatim from runBranchScoped's `case !exists` block.
func (a branchScopedArgs) seedFresh(ctx context.Context, active, branch string) (bool, string, error) {
	filled, how, ferr := a.fill(ctx, active, branch)
	if ferr != nil {
		return false, "", ferr
	}
	builtEmpty := false
	if !filled {
		if err := a.eng.drv.Empty(ctx, active); err != nil {
			return false, "", fmt.Errorf("empty active %s: %w", active, err)
		}
		builtEmpty = true
		if dp, ok, derr := dumpReady(a.d.Dump, a.worktreePath); derr != nil {
			return false, "", derr
		} else if ok && a.loadDump != nil {
			if err := a.loadDump(ctx, active, dp); err != nil {
				return false, "", fmt.Errorf("load dump %s: %w", dp, err)
			}
			how = "dump"
		} else {
			how = "empty"
		}
	}
	return builtEmpty, "seed:" + how, nil
}

// swapBranch handles the `old != branch` arm of runBranchScoped: a
// branch switch. It preserves the OLD branch's data into its durable
// copy, advances the marker, then fills the active slot with THIS
// branch's data. Returns the decision. Extracted verbatim from
// runBranchScoped's `case old != branch` block.
func (a branchScopedArgs) swapBranch(ctx context.Context, active, old, branch string) (string, error) {
	if err := a.eng.drv.Capture(ctx, active, a.eng.durable(active, old)); err != nil {
		return "", fmt.Errorf("capture old branch %q: %w", old, err)
	}
	// Advance the marker to the NEW branch the instant old's data is
	// safe in its durable copy — BEFORE fill mutates the active slot.
	// If the daemon dies mid-fill, the next prepare sees old==branch
	// and takes the noop path instead of re-capturing the (now
	// new-branch) active back into durable(old) and clobbering it.
	// The trade-off is a possibly-stale active on crash, recoverable
	// with `treeman db reset`; no durable copy is ever destroyed.
	if err := a.st.SetActiveBranch(ctx, a.repoID, a.worktreeID, active, branch, a.eng.engine); err != nil {
		return "", fmt.Errorf("record active-branch marker (pre-fill): %w", err)
	}
	filled, how, ferr := a.fill(ctx, active, branch)
	if ferr != nil {
		return "", ferr
	}
	if !filled {
		// No durable copy and no resolvable parent: the data we just
		// captured (old branch's state) IS the branch point for a
		// new branch forked off old. Leave it in place.
		how = "branch-point"
	}
	return "swap:" + how, nil
}

// fill populates `active` with `branch`'s data. Order: the branch's own
// durable copy (resume) → the parent branch's live namespace (seed from
// upstream). Returns (filled, how) — filled=false means neither source
// was available and the caller decides the fallback.
func (a branchScopedArgs) fill(ctx context.Context, active, branch string) (bool, string, error) {
	dur := a.eng.durable(active, branch)
	if ok, _ := a.eng.drv.Exists(ctx, dur); ok {
		if err := a.eng.drv.Restore(ctx, dur, active); err != nil {
			return false, "", fmt.Errorf("restore durable copy for %q: %w", branch, err)
		}
		return true, "resume", nil
	}
	parent, ok, err := a.parentDB(ctx, branch)
	if err != nil {
		return false, "", err
	}
	if ok && parent != "" && parent != active {
		if pe, _ := a.eng.drv.Exists(ctx, parent); pe {
			if err := a.eng.drv.Restore(ctx, parent, active); err != nil {
				return false, "", fmt.Errorf("seed %q from parent branch's live db %q: %w%s",
					active, parent, err, parentSeedHint(a.eng.engine, err))
			}
			return true, "parent", nil
		}
	}
	return false, "", nil
}

func (a branchScopedArgs) repoPath(ctx context.Context) string {
	p, _ := a.st.RepoPath(ctx, a.repoID)
	return p
}

// parentDB resolves the live database to seed `branch` from. Production
// uses the git-upstream + store resolver; tests inject `resolveParent` to
// drive `fill`'s parent path deterministically without a git repo.
func (a branchScopedArgs) parentDB(ctx context.Context, branch string) (string, bool, error) {
	if a.resolveParent != nil {
		return a.resolveParent(ctx, branch)
	}
	return resolveBaseSourceDB(ctx, a.st, a.cfg, a.repoPath(ctx), a.repoID, a.dbIdx, a.eng.scope, branch)
}

func (a branchScopedArgs) runStep(ctx context.Context, spec runner.Spec, active, label string) error {
	spec = spec.WithLogPath(runnerLogPath(a.worktreePath, a.eng.engine, label, active))
	out, err := runner.Run(ctx, spec, a.worktreePath, active, a.tplCtx, a.inheritedEnv)
	if err != nil {
		return fmt.Errorf("%s %s: %w", label, active, err)
	}
	if out.ExitCode != 0 {
		emitRunnerError(ctx, a.st, a.repoID, a.worktreeID, a.eng.engine, active, label, out)
		return fmt.Errorf("%s", runner.FormatError(label, active, out))
	}
	return nil
}

func (a branchScopedArgs) event(ctx context.Context, typ, msg string, extra map[string]string) {
	fields := map[string]string{"engine": a.eng.engine, "mode": "branch_scoped"}
	maps.Copy(fields, extra)
	_ = a.st.WriteEvent(ctx, store.LevelInfo, typ, msg, a.repoID, a.worktreeID, "", 0, fields)
}

// captureBranchScopedOnTeardown snapshots a worktree's current active
// namespace into the current branch's durable copy before the worktree
// is deleted, so re-creating it (or another worktree that later swaps to
// the same branch) can resume. Best-effort: a failure must not block
// teardown. The active namespace itself is dropped by the normal
// teardown path afterward.
func captureBranchScopedOnTeardown(ctx context.Context, eng *branchEngine, st *store.Store, worktreeID int64, active string) error {
	branch, ok, err := st.GetActiveBranch(ctx, worktreeID, active)
	if err != nil || !ok || branch == "" {
		return err
	}
	exists, err := eng.drv.Exists(ctx, active)
	if err != nil || !exists {
		return err
	}
	return eng.drv.Capture(ctx, active, eng.durable(active, branch))
}

// teardownBranchScoped handles `wt delete` for one branch_scoped
// database. It captures the active namespace into the current branch's
// durable copy (so a re-created worktree at the same path resumes),
// then drops the active namespace. Durable per-branch copies are
// intentionally KEPT — they're the whole point of "lives on". Resolves
// the active namespace from the worktree PATH (branch-independent), so
// it drops the same name the swap lifecycle created regardless of which
// branch the worktree currently sits on.
func teardownBranchScoped(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	scope, _, ok := branchScopeFor(d.Engine)
	if !ok {
		return nil
	}
	wtPath, err := st.WorktreePathByID(ctx, worktreeID)
	if err != nil {
		return err
	}
	if wtPath == "" {
		return nil
	}
	active, err := activeNamespace(d, scope, wtPath)
	if err != nil {
		return err
	}
	eng, closeEng, cerr := connectBranchEngine(ctx, cfg, d.Engine)
	if cerr != nil {
		return cerr
	}
	if eng == nil {
		return nil
	}
	defer closeEng()

	if capErr := captureBranchScopedOnTeardown(ctx, eng, st, worktreeID, active); capErr != nil {
		_ = st.WriteEvent(ctx, store.LevelWarn, "branch_capture_error",
			capErr.Error(), repoID, worktreeID, "", 0, nil)
	}
	if err := eng.drv.Drop(ctx, active); err != nil {
		return err
	}
	_ = st.ClearActiveBranch(ctx, worktreeID, active)
	_ = st.WriteEvent(ctx, store.LevelInfo, "db_drop",
		fmt.Sprintf("%s: dropped active %s (branch_scoped; durable copies kept)", d.Engine, active),
		repoID, worktreeID, "", 0, map[string]any{"engine": d.Engine, "active": active})
	return nil
}

// ResetBranchScoped drops the current branch's durable copy AND clears
// the active-branch marker for every branch_scoped database, so the
// next prepare re-seeds the active namespace from the live parent
// branch. Backs `treeman db reset`. `engineFilter` (lowercased)
// restricts the reset to one engine when non-empty.
func ResetBranchScoped(
	ctx context.Context,
	cfg *config.Config,
	worktreePath string,
	repoID, worktreeID int64,
	st *store.Store,
	engineFilter string,
) error {
	// Canonicalize the filter so `--engine mysql` also matches a DB
	// declared as `mariadb`/`tidb`, `--engine postgres` matches
	// `postgresql`, etc.
	filterLabel := engineFilter
	if engineFilter != "" {
		if fam, ok := engine.Canonical(engineFilter); ok {
			filterLabel = string(fam)
		}
	}
	for _, d := range cfg.Databases {
		if !d.BranchScoped {
			continue
		}
		scope, label, ok := branchScopeFor(d.Engine)
		if !ok {
			continue
		}
		if filterLabel != "" && label != filterLabel {
			continue
		}
		active, err := activeNamespace(d, scope, worktreePath)
		if err != nil {
			return fmt.Errorf("render active namespace for %s: %w", d.Engine, err)
		}
		eng, closeEng, cerr := connectBranchEngine(ctx, cfg, d.Engine)
		if cerr != nil {
			return cerr
		}
		if eng == nil {
			continue
		}
		rerr := resetActiveNamespace(ctx, eng, st, worktreeID, active)
		closeEng()
		if rerr != nil {
			return rerr
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_reset",
			fmt.Sprintf("%s: cleared active %s + durable copy", d.Engine, active),
			repoID, worktreeID, "", 0, map[string]string{"engine": d.Engine, "active": active})
	}
	return nil
}

// resetActiveNamespace drops the current branch's durable copy and the
// active namespace itself, then clears the active-branch marker.
//
// Dropping (NOT emptying) the active namespace is deliberate and load-
// bearing: the follow-up prepare must observe a NON-EXISTENT active slot
// so its seed path (`!exists` → fill from durable/parent) fires. Emptying
// would leave a present-but-empty namespace, which prepare then treats as
// an unmarked existing DB and "adopts" as-is — leaving the database empty
// instead of re-seeded from the live parent. For name-scoped engines
// (MySQL/Postgres) `Empty` recreates the database, so that adopt-empty
// outcome was a silent data bug; `Drop` keeps reset reseeding correctly
// across all engines.
func resetActiveNamespace(ctx context.Context, eng *branchEngine, st *store.Store, worktreeID int64, active string) error {
	// Drop the durable copy for whatever branch currently owns the active
	// slot (so a re-seed can't restore the stale copy).
	if branch, has, _ := st.GetActiveBranch(ctx, worktreeID, active); has && branch != "" {
		_ = eng.drv.DropDurable(ctx, eng.durable(active, branch))
	}
	if err := eng.drv.Drop(ctx, active); err != nil {
		return err
	}
	return st.ClearActiveBranch(ctx, worktreeID, active)
}

// BranchScopedDB reports the swap state of one branch_scoped database.
type BranchScopedDB struct {
	Engine string `json:"engine"`
	// Active is the rendered active namespace (DB name / key prefix) the
	// app connects to — stable across branch switches.
	Active string `json:"active"`
	// ActiveBranch is the branch whose data currently occupies Active
	// (the marker). Empty when nothing has been swapped in yet.
	ActiveBranch string `json:"active_branch,omitempty"`
	// ResumableBranches are the local git branches that have a durable
	// copy for this database — switching to one resumes its preserved
	// data instead of re-seeding from the parent.
	ResumableBranches []string `json:"resumable_branches"`
}

// BranchScopedStatus inspects every branch_scoped database for a worktree
// and reports, per DB: the active namespace, which branch currently
// occupies it (the marker), and which local git branches have a durable
// copy a swap could resume from. Durable copies aren't tracked in SQLite
// — their names are deterministic — so this probes the engine for the
// durable of each local branch. `cfg` must already have the main-worktree
// overlay applied (callers route through wt.ResolveIdentity) so the
// active namespace renders the same name the swap lifecycle created.
func BranchScopedStatus(
	ctx context.Context,
	cfg *config.Config,
	repoRoot, worktreePath string,
	worktreeID int64,
	st *store.Store,
) ([]BranchScopedDB, error) {
	branches := localBranches(ctx, repoRoot)
	out := []BranchScopedDB{}
	for _, d := range cfg.Databases {
		if !d.BranchScoped {
			continue
		}
		scope, _, ok := branchScopeFor(d.Engine)
		if !ok {
			continue
		}
		active, err := activeNamespace(d, scope, worktreePath)
		if err != nil {
			return nil, fmt.Errorf("render active namespace for %s: %w", d.Engine, err)
		}
		eng, closeEng, cerr := connectBranchEngine(ctx, cfg, d.Engine)
		if cerr != nil {
			return nil, cerr
		}
		if eng == nil {
			continue
		}
		activeBranch, _, _ := st.GetActiveBranch(ctx, worktreeID, active)
		resumable := []string{}
		for _, b := range branches {
			exists, perr := eng.drv.Exists(ctx, eng.durable(active, b))
			if perr != nil {
				continue
			}
			if exists {
				resumable = append(resumable, b)
			}
		}
		closeEng()
		out = append(out, BranchScopedDB{
			Engine:            eng.engine,
			Active:            active,
			ActiveBranch:      activeBranch,
			ResumableBranches: resumable,
		})
	}
	return out, nil
}

// localBranches lists the repo's local branch names. Best-effort: an
// error (not a git repo, git missing) yields an empty list so status
// degrades to "no resumable branches" rather than failing.
func localBranches(ctx context.Context, repoRoot string) []string {
	out, err := gitcmd.Output(ctx, repoRoot, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil
	}
	var branches []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if b := strings.TrimSpace(line); b != "" {
			branches = append(branches, b)
		}
	}
	return branches
}
