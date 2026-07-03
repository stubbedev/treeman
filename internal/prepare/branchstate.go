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
	dbs3 "github.com/stubbedev/treeman/internal/db/s3"
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
	// RestoreParent fills `active` from a parent LIVE namespace, excluding
	// anything that belongs to a deeper worktree namespace nested under the
	// parent prefix (`srcKeep`; nil = copy everything). Name-scoped engines
	// copy an EXACT database and ignore srcKeep; prefix-scoped engines need
	// it because a bare main-worktree parent prefix nests every sibling
	// worktree's `<prefix>_<slug>_*` data.
	RestoreParent(ctx context.Context, parent, active string, srcKeep func(string) bool) error
	Empty(ctx context.Context, active string) error // reset active to an empty, present namespace
	Drop(
		ctx context.Context,
		ns string,
	) error // remove the namespace entirely (EXACT for name-scoped; prefix for prefix-scoped)
	DropDurable(ctx context.Context, durable string) error
	// Watermark returns a sound, monotonic, per-namespace write-counter
	// token for `ns`. Unchanged between two calls ⇒ provably no writes to
	// `ns` in between (counters only increase, so a write can't hide). ""
	// means the engine exposes no sound cheap signal — the caller must NOT
	// skip a capture. Any probe error also returns "".
	Watermark(ctx context.Context, ns string) (string, error)
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
	case b.scope == scopePrefix && b.engine == "s3":
		// Durable is a sibling bucket. Bucket names are dns-safe: no `_`,
		// no leading/trailing hyphen — `tmbs-<16hex>` (21 chars) fits the
		// 3-63 lowercase rule.
		return "tmbs-" + h
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

func (a mysqlNS) RestoreParent(ctx context.Context, parent, active string, _ func(string) bool) error {
	return a.d.SnapshotRestore(ctx, parent, active)
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

func (a mysqlNS) Watermark(ctx context.Context, ns string) (string, error) {
	// Per-database when performance_schema tracks table writes; else the
	// driver falls back to a server-wide counter (sound, coarser).
	return a.d.WriteWatermark(ctx, ns)
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

func (a postgresNS) RestoreParent(ctx context.Context, parent, active string, _ func(string) bool) error {
	return a.d.SnapshotRestore(ctx, parent, active)
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

func (a postgresNS) Watermark(ctx context.Context, ns string) (string, error) {
	return a.d.WriteWatermark(ctx, ns)
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

func (a mongoNS) RestoreParent(ctx context.Context, parent, active string, _ func(string) bool) error {
	return a.d.SnapshotRestore(ctx, parent, active)
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

func (a mongoNS) Watermark(ctx context.Context, ns string) (string, error) {
	// No sound, cheap, per-database write counter in mongo (opcounters
	// are server-wide and reset on restart; db.stats sizes can net to
	// zero after offsetting writes). Decline to skip captures.
	return "", nil
}

// redisNS adapts the Redis driver. `keep` plays the same sibling-
// exclusion role as esNS.keep (see its doc): on the main worktree the
// bare, slug-free active prefix is a prefix of every linked worktree's
// `<prefix>_<slug>_*` keys, so every op that enumerates or drops the
// active prefix must spare sibling-owned keys. nil = keep everything. The
// durable copy is hash-derived and never collides, so DropDurable stays
// unfiltered.
type redisNS struct {
	d    *dbredis.Driver
	keep func(string) bool
}

func (a redisNS) Exists(ctx context.Context, ns string) (bool, error) {
	return a.d.PrefixExistsFiltered(ctx, ns, a.keep)
}

func (a redisNS) Capture(ctx context.Context, active, durable string) error {
	return a.d.SnapshotCreateFiltered(ctx, active, durable, a.keep)
}

func (a redisNS) Restore(ctx context.Context, durable, active string) error {
	return a.d.SnapshotRestoreFiltered(ctx, durable, active, a.keep)
}

func (a redisNS) RestoreParent(ctx context.Context, parent, active string, srcKeep func(string) bool) error {
	return a.d.SnapshotRestoreSrcFiltered(ctx, parent, active, srcKeep, a.keep)
}

func (a redisNS) Empty(ctx context.Context, active string) error {
	_, err := a.d.DropPrefixFiltered(ctx, active, a.keep)
	return err
}

func (a redisNS) Drop(ctx context.Context, ns string) error {
	_, err := a.d.DropPrefixFiltered(ctx, ns, a.keep)
	return err
}

func (a redisNS) DropDurable(ctx context.Context, durable string) error {
	return a.d.DropSnapshot(ctx, durable)
}

func (a redisNS) Watermark(ctx context.Context, ns string) (string, error) {
	// No sound, cheap, per-prefix write counter in redis. Decline to
	// skip captures.
	return "", nil
}

// esNS adapts the Elasticsearch driver. `keep`, when non-nil, excludes
// indices owned by a sibling worktree's slug from every operation that
// enumerates or drops the active prefix. It is load-bearing on the main
// worktree, whose branch_scoped active prefix is bare (slug-free) and so
// is a prefix of every linked worktree's `<prefix>_<slug>_*` indices —
// without the filter, an Empty/Drop/Restore at the repo root would wipe
// sibling worktrees' live data and Exists would bleed into them. This is
// the prefix-engine analogue of mysql/postgres EXACT-name scoping. The
// durable copy is hash-derived and never collides, so DropDurable stays
// unfiltered.
type esNS struct {
	d    *dbes.Driver
	keep func(string) bool
}

func (a esNS) Exists(ctx context.Context, ns string) (bool, error) {
	m, err := a.d.ListMatching(ctx, ns)
	if err != nil {
		return false, err
	}
	for _, n := range m {
		if a.keep == nil || a.keep(n) {
			return true, nil
		}
	}
	return false, nil
}

func (a esNS) Capture(ctx context.Context, active, durable string) error {
	return a.d.SnapshotCreateFiltered(ctx, active, durable, a.keep)
}

func (a esNS) Restore(ctx context.Context, durable, active string) error {
	return a.d.SnapshotRestoreFiltered(ctx, durable, active, a.keep)
}

func (a esNS) RestoreParent(ctx context.Context, parent, active string, srcKeep func(string) bool) error {
	return a.d.SnapshotRestoreSrcFiltered(ctx, parent, active, srcKeep, a.keep)
}

func (a esNS) Empty(ctx context.Context, active string) error {
	_, err := a.d.DropMatchingFiltered(ctx, active, a.keep)
	return err
}

func (a esNS) Drop(ctx context.Context, ns string) error {
	_, err := a.d.DropMatchingFiltered(ctx, ns, a.keep)
	return err
}

func (a esNS) DropDurable(ctx context.Context, durable string) error {
	return a.d.DropSnapshot(ctx, durable)
}

func (a esNS) Watermark(ctx context.Context, ns string) (string, error) {
	// Filter to THIS worktree's indices: on a bare main-worktree prefix the
	// unfiltered sum would include sibling worktrees' writes and falsely
	// mark the active dirty.
	return a.d.WriteWatermarkFiltered(ctx, ns, a.keep)
}

// s3NS adapts the S3 driver. Unlike redis/es, S3 branch ops act on
// EXACT buckets (the active worktree bucket and a per-branch durable
// bucket are independent buckets), so there is no sibling-prefix
// nesting and no `keep` filter — S3 behaves like the name-scoped
// engines. Capture/Restore/RestoreParent all reduce to a whole-bucket
// server-side copy.
type s3NS struct{ d *dbs3.Driver }

func (a s3NS) Exists(ctx context.Context, ns string) (bool, error) {
	return a.d.BucketExists(ctx, ns)
}

func (a s3NS) Capture(ctx context.Context, active, durable string) error {
	return a.d.CopyBucket(ctx, active, durable)
}

func (a s3NS) Restore(ctx context.Context, durable, active string) error {
	return a.d.CopyBucket(ctx, durable, active)
}

func (a s3NS) RestoreParent(ctx context.Context, parent, active string, _ func(string) bool) error {
	return a.d.CopyBucket(ctx, parent, active)
}

func (a s3NS) Empty(ctx context.Context, active string) error {
	return a.d.Empty(ctx, active)
}

func (a s3NS) Drop(ctx context.Context, ns string) error {
	// EXACT bucket drop, not the prefix reap — a branch-scoped active
	// bucket name can be a prefix of sibling worktrees' buckets.
	return a.d.DropBucket(ctx, ns)
}

func (a s3NS) DropDurable(ctx context.Context, durable string) error {
	return a.d.DropBucket(ctx, durable)
}

func (a s3NS) Watermark(ctx context.Context, ns string) (string, error) {
	// S3 exposes no sound, cheap per-bucket write counter (object count
	// nets to zero across offsetting put/delete; LastModified scans the
	// whole bucket). Decline to skip captures — correctness over a
	// micro-optimisation.
	return "", nil
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

// siblingKeep returns a predicate that excludes names owned by another
// worktree's slug, or nil when there are no siblings (keep everything).
// Prefix-scoped adapters use it so a bare main-worktree active prefix
// never enumerates or drops sibling worktrees' `<prefix>_<slug>_*`
// namespace. Name-scoped engines ignore it (they drop EXACT names).
func siblingKeep(siblings []string) func(string) bool {
	if len(siblings) == 0 {
		return nil
	}
	return func(n string) bool { return !nameOwnedByOtherSlug(n, siblings) }
}

// connectBranchEngine dials `engine` and wraps it in a branchEngine.
// Used by `treeman db reset`, which connects fresh (the prepare path
// builds the adapter from its already-connected driver instead). The
// returned close func releases the connection.
//
// `siblings` are the other worktrees' slugs; for prefix-scoped engines
// they build the sibling-exclusion filter (see esNS.keep) so an op on a
// bare main-worktree active prefix spares sibling-owned indices/keys.
// Pass nil when the caller only touches hash-derived durable namespaces
// (reap, status), which never collide.
//
// Unrecognised engines return (nil, no-op, nil) so iteration over a
// mixed-engine config can skip non-participating entries quietly,
// BUT log a warning so a typo'd engine name doesn't disappear
// without a trace. Same observability principle as TeardownDatabases'
// unknown-engine arm.
func connectBranchEngine(ctx context.Context, cfg *config.Config, eng string, siblings []string) (*branchEngine, func(), error) {
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
		return &branchEngine{
			drv:    redisNS{d: drv, keep: siblingKeep(siblings)},
			scope:  scope,
			engine: label,
		}, func() { _ = drv.Close() }, nil
	case "elasticsearch":
		if cfg.Connections.Elasticsearch == nil {
			return nil, func() {}, errors.New("connections.elasticsearch not configured")
		}
		drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
		if err != nil {
			return nil, func() {}, err
		}
		return &branchEngine{drv: esNS{d: drv, keep: siblingKeep(siblings)}, scope: scope, engine: label}, func() {}, nil
	case "s3":
		return connectS3Branch(ctx, cfg, scope, label)
	default:
		return nil, func() {}, nil
	}
}

// connectS3Branch dials S3 for the branch-swap lifecycle. Extracted from
// connectBranchEngine's switch to keep that function under the
// complexity gate. S3 ops are exact-bucket, so no sibling-keep filter
// (see s3NS).
func connectS3Branch(ctx context.Context, cfg *config.Config, scope bsScope, label string) (*branchEngine, func(), error) {
	if cfg.Connections.S3 == nil {
		return nil, func() {}, errors.New("connections.s3 not configured")
	}
	drv, err := dbs3.Connect(ctx, *cfg.Connections.S3)
	if err != nil {
		return nil, func() {}, err
	}
	return &branchEngine{drv: s3NS{drv}, scope: scope, engine: label}, func() {}, nil
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

	// migrateFP is the content fingerprint of this database's migration
	// inputs (migrations + lockfile + commands + engine version), used to
	// skip a redundant migrate when a resumed branch's inputs are
	// unchanged since its durable copy was last migrated. Empty disables
	// the skip (always migrate) — the safe default for callers that don't
	// supply it.
	migrateFP string

	eng *branchEngine
	// loadDump loads one static fallback dump entry into the active
	// namespace. seedFresh calls it once per resolved dumpFile so the
	// per-entry SourceDB (mongo namespace remap) is honoured.
	loadDump func(ctx context.Context, active string, dump dumpFile) error
	// resolveParent, when non-nil, overrides the default git-upstream +
	// store base-branch resolver. Only tests set it; production leaves it
	// nil so `fill` uses resolveBaseSourceDB.
	resolveParent func(ctx context.Context, branch string) (string, bool, error)
	// resolveBaseBranchFn, when non-nil, overrides the git base-branch
	// resolver used by the durable-snapshot seed fallback. Only tests set
	// it; production leaves it nil so `baseBranchDurable` uses
	// resolveBaseBranch.
	resolveBaseBranchFn func(ctx context.Context, branch string) string
}

// runBranchScoped is the unified swap lifecycle for one branch-scoped
// database. It runs on every prepare (create + HEAD-switch finalize):
//
//   - active doesn't exist → seed it (this branch's durable copy, else
//     the base branch's live data, else the base branch's durable
//     snapshot, else dump, else empty).
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

	a.event(ctx, store.EvtPrepareStart, fmt.Sprintf("engine=%s active=%s branch=%s branch_scoped", a.eng.engine, active, branch), nil)

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
		if err := a.captureDurable(ctx, active, branch); err != nil {
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

	migrated := false
	if a.d.Migrate != nil {
		prevFP, hasPrev, _ := a.st.GetBranchMigrated(ctx, a.worktreeID, active, branch)
		if migrateNeeded(builtEmpty, hasPrev, prevFP, a.migrateFP) {
			if err := a.runStep(ctx, runner.FromMigrate(*a.d.Migrate), active, "migrate"); err != nil {
				return Outcome{}, err
			}
			migrated = true
			// Record the fingerprint only when we have one; an empty
			// fingerprint can never be matched, so the next prepare
			// re-migrates (correct — we couldn't prove it was at head).
			if a.migrateFP != "" {
				_ = a.st.SetBranchMigrated(ctx, a.worktreeID, active, branch, a.migrateFP)
			}
		} else {
			a.event(ctx, store.EvtMigrateSkip,
				fmt.Sprintf("engine=%s active=%s branch=%s migrate skipped (inputs unchanged)", a.eng.engine, active, branch),
				map[string]string{"fingerprint": a.migrateFP})
		}
	}
	seeded := false
	if a.d.Seed != nil && builtEmpty {
		if err := a.runStep(ctx, runner.FromSeed(*a.d.Seed), active, "seed"); err != nil {
			return Outcome{}, err
		}
		seeded = true
	}

	// Lever-1 bookkeeping: record whether `active` now mirrors `branch`'s
	// durable copy so the next swap can skip a redundant capture.
	a.recordClean(ctx, active, branch, decision, migrated, seeded)

	ms := time.Since(started).Milliseconds()
	a.event(ctx, store.EvtPrepareEnd,
		fmt.Sprintf("branch_scoped decision=%s active=%s branch=%s duration=%dms", decision, active, branch, ms),
		map[string]string{"duration_ms": strconv.FormatInt(ms, 10), "decision": decision})

	return Outcome{Engine: a.eng.engine, SourceDB: active, Decision: decision}, nil
}

// seedFresh handles the `!exists` arm of runBranchScoped: the active
// slot is empty, so seed this branch's data (durable / live parent /
// base-branch durable snapshot via fill, else dump, else empty).
// Returns (builtEmpty, decision). Extracted verbatim from
// runBranchScoped's `case !exists` block.
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
		dumps, derr := dumpsReady(a.d.Dump, a.worktreePath)
		if derr != nil {
			return false, "", derr
		}
		if len(dumps) > 0 && a.loadDump != nil {
			for _, dr := range dumps {
				if err := a.loadDump(ctx, active, dr); err != nil {
					return false, "", fmt.Errorf("load dump %s: %w", dr.Path, err)
				}
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
	// Lever 1: skip capturing old → durable(old) when `active` provably
	// still mirrors that durable copy. Sound because the watermark is a
	// monotonic write counter — an unchanged value cannot hide a write.
	if a.captureRedundant(ctx, active, old) {
		a.event(ctx, store.EvtBranchCaptureSkip,
			fmt.Sprintf("engine=%s active=%s branch=%s capture skipped (no writes since resume)", a.eng.engine, active, old),
			map[string]string{"old_branch": old})
	} else if err := a.captureDurable(ctx, active, old); err != nil {
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

// migrateNeeded reports whether the branch_scoped migrate step must run.
// It runs when the active namespace was just built empty/from-dump
// (builtEmpty), when no prior migrated-fingerprint is recorded for this
// branch (hasPrev=false), when the recorded fingerprint differs from the
// current inputs, or when the current fingerprint is empty (no inputs to
// trust). Otherwise the resumed durable copy is already at head — skip.
func migrateNeeded(builtEmpty, hasPrev bool, prevFP, curFP string) bool {
	if builtEmpty || curFP == "" || !hasPrev {
		return true
	}
	return prevFP != curFP
}

// captureSkippable reports whether the outgoing branch's capture can be
// safely skipped: `active` is known to mirror that branch's durable copy
// (prevClean), the durable copy still exists, and a freshly probed
// watermark is non-empty and equals the one recorded when the mirror was
// established (proving zero writes since). A "" watermark — an engine
// without a sound signal, or a probe error — is never skippable.
func captureSkippable(prevClean, durableExists bool, prevWM, curWM string) bool {
	return prevClean && durableExists && curWM != "" && curWM == prevWM
}

// captureRedundant probes the live state to decide whether swapBranch may
// skip capturing `active` into durable(old). Any probe error falls back to
// "not redundant" (capture), so a transient failure never risks data loss.
func (a branchScopedArgs) captureRedundant(ctx context.Context, active, old string) bool {
	prevClean, prevWM, ok, err := a.st.GetActiveBranchClean(ctx, a.worktreeID, active)
	if err != nil || !ok || !prevClean {
		return false
	}
	durExists, derr := a.eng.drv.Exists(ctx, a.eng.durable(active, old))
	if derr != nil {
		return false
	}
	curWM, werr := a.eng.drv.Watermark(ctx, active)
	if werr != nil {
		return false
	}
	return captureSkippable(prevClean, durExists, prevWM, curWM)
}

// recordClean writes the lever-1 bookkeeping at the end of a prepare:
// whether `active` currently mirrors `branch`'s durable copy, plus the
// watermark observed now. Clean is true only when the data came straight
// from a durable restore (resume) or an adopt with no migrate/seed/write
// since — and only when the engine exposes a sound watermark. A `noop`
// (same branch already loaded) carries the prior clean state forward iff
// the watermark is unchanged. Everything else is recorded dirty.
func (a branchScopedArgs) recordClean(ctx context.Context, active, branch, decision string, migrated, seeded bool) {
	dirty := func() {
		_ = a.st.SetActiveBranchClean(ctx, a.repoID, a.worktreeID, active, branch, a.eng.engine, false, "")
	}
	if migrated || seeded {
		dirty()
		return
	}
	if decision == "noop" {
		prevClean, prevWM, ok, err := a.st.GetActiveBranchClean(ctx, a.worktreeID, active)
		if err == nil && ok && prevClean {
			if w, werr := a.eng.drv.Watermark(ctx, active); werr == nil && w != "" && w == prevWM {
				_ = a.st.SetActiveBranchClean(ctx, a.repoID, a.worktreeID, active, branch, a.eng.engine, true, prevWM)
				return
			}
		}
		dirty()
		return
	}
	if decision == "adopt" || strings.HasSuffix(decision, ":resume") {
		if w, werr := a.eng.drv.Watermark(ctx, active); werr == nil && w != "" {
			_ = a.st.SetActiveBranchClean(ctx, a.repoID, a.worktreeID, active, branch, a.eng.engine, true, w)
			return
		}
	}
	dirty()
}

// fill populates `active` with `branch`'s data. Order: the branch's own
// durable copy (resume) → the base branch's live namespace (seed from the
// upstream checkout) → the base branch's freshest durable SNAPSHOT (seed
// from a parked base, e.g. `develop` that lives only as a ref). Returns
// (filled, how) — filled=false means no source was available and the
// caller decides the fallback (empty).
func (a branchScopedArgs) fill(ctx context.Context, active, branch string) (bool, string, error) {
	dur := a.eng.durable(active, branch)
	if ok, _ := a.eng.drv.Exists(ctx, dur); ok {
		if err := retryTransient(ctx, func() error { return a.eng.drv.Restore(ctx, dur, active) }); err != nil {
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
			keep := a.parentSourceKeep(ctx, parent)
			if err := retryTransient(ctx, func() error { return a.eng.drv.RestoreParent(ctx, parent, active, keep) }); err != nil {
				return false, "", fmt.Errorf("seed %q from parent branch's live db %q: %w%s",
					active, parent, err, parentSeedHint(a.eng.engine, err))
			}
			return true, "parent", nil
		}
	}
	// The base branch's live DB is unavailable — its worktree was torn
	// down, or the base (e.g. `develop`) exists only as a ref while the
	// main checkout sits on another branch. Seed from the freshest durable
	// SNAPSHOT of the base branch instead of cold-building an empty schema.
	// migrate then applies only the new branch's own added migrations on
	// top (incremental), so a branch off develop still inherits develop's
	// data without a dump.
	if snap, base, ok := a.baseBranchDurable(ctx, branch); ok {
		if err := a.eng.drv.Restore(ctx, snap, active); err != nil {
			return false, "", fmt.Errorf("seed %q from base branch %q durable snapshot %q: %w",
				active, base, snap, err)
		}
		return true, "parent-snapshot", nil
	}
	return false, "", nil
}

// baseBranchName resolves the local branch the new worktree's branch_scoped
// seed should inherit from (e.g. `develop`). Tests override via
// resolveBaseBranchFn; production uses the git-upstream + main-worktree
// resolver shared with resolveBaseSourceDB.
func (a branchScopedArgs) baseBranchName(ctx context.Context, branch string) string {
	if a.resolveBaseBranchFn != nil {
		return a.resolveBaseBranchFn(ctx, branch)
	}
	return resolveBaseBranch(ctx, a.st, a.repoPath(ctx), a.repoID, branch)
}

// baseBranchDurable finds the freshest existing durable snapshot of the base
// branch for THIS logical database (databases[dbIdx]), to seed a branch
// forked off it when the base branch's LIVE database isn't available to
// copy. It scopes candidates to dbIdx by forward-computing, for every
// worktree (and the main overlay), the durable name that branch would carry
// — `durable(active, base)` — so durables of sibling databases that merely
// share a name prefix (e.g. the test DB) are never matched. Among existing
// candidates it picks the most-recently-used. Returns ("", "", false) when
// no base branch resolves or no snapshot exists.
func (a branchScopedArgs) baseBranchDurable(ctx context.Context, branch string) (string, string, bool) {
	base := a.baseBranchName(ctx, branch)
	if base == "" || base == branch {
		return "", "", false
	}
	rows, err := a.st.ListWorktreesForRepo(ctx, a.repoID)
	if err != nil {
		return "", "", false
	}
	mainCfg := *a.cfg
	config.ApplyMainWorktreeOverlay(&mainCfg)
	candidates := map[string]struct{}{}
	for _, wt := range rows {
		// Deleted worktrees stay in the scan: teardown keeps branch
		// durables alive precisely so a later worktree can seed from
		// them, and their stored path still renders the active name the
		// durable was hashed under.
		dbForWt := a.d
		if wt.IsMain && a.dbIdx < len(mainCfg.Databases) {
			dbForWt = mainCfg.Databases[a.dbIdx]
		}
		act, aerr := activeNamespace(dbForWt, a.eng.scope, wt.Path)
		if aerr != nil {
			continue
		}
		candidates[a.eng.durable(act, base)] = struct{}{}
	}
	if len(candidates) == 0 {
		return "", "", false
	}
	durs, err := a.st.ListBranchDurables(ctx, a.repoID)
	if err != nil {
		return "", "", false
	}
	best, bestUsed := "", int64(-1)
	for _, d := range durs {
		if d.Engine != a.eng.engine || d.Branch != base {
			continue
		}
		if _, ok := candidates[d.DurableName]; !ok {
			continue
		}
		if d.LastUsedAt <= bestUsed {
			continue
		}
		if ex, _ := a.eng.drv.Exists(ctx, d.DurableName); !ex {
			continue
		}
		best, bestUsed = d.DurableName, d.LastUsedAt
	}
	if best == "" {
		return "", "", false
	}
	return best, base, true
}

// parentSourceKeep returns a predicate excluding keys/indices that belong
// to a worktree namespace nested STRICTLY under `parent`, so a parent-seed
// copies only the parent worktree's OWN data. It matters when `parent` is a
// bare main-worktree prefix (a prefix of every linked worktree's
// `<prefix>_<slug>_*` data); without it a feature branch would be seeded
// with every sibling worktree's data too. Returns nil for name-scoped
// engines (exact-db copy, no nesting) or when nothing nests under `parent`.
func (a branchScopedArgs) parentSourceKeep(ctx context.Context, parent string) func(string) bool {
	if a.eng.scope != scopePrefix {
		return nil
	}
	wts, err := a.st.ListWorktreesForRepo(ctx, a.repoID)
	if err != nil {
		return nil
	}
	mainCfg := *a.cfg
	config.ApplyMainWorktreeOverlay(&mainCfg)
	var deeper []string
	for _, wt := range wts {
		if wt.Deleted {
			continue
		}
		dbForWt := a.d
		if wt.IsMain && a.dbIdx < len(mainCfg.Databases) {
			dbForWt = mainCfg.Databases[a.dbIdx]
		}
		p, perr := activeNamespace(dbForWt, a.eng.scope, wt.Path)
		if perr != nil || p == parent || !strings.HasPrefix(p, parent) {
			continue
		}
		deeper = append(deeper, p)
	}
	if len(deeper) == 0 {
		return nil
	}
	return func(name string) bool {
		for _, p := range deeper {
			if strings.HasPrefix(name, p) {
				return false
			}
		}
		return true
	}
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

// captureDurable captures `active` into `branch`'s durable copy and records
// the durable in SQLite so the orphan sweep can later drop it by name. The
// tracking-row write is best-effort: a failure must not fail the prepare —
// the durable still exists, and the legacy forward-hash reaper still covers
// branches deleted while their worktree is live.
func (a branchScopedArgs) captureDurable(ctx context.Context, active, branch string) error {
	dur := a.eng.durable(active, branch)
	if err := a.eng.drv.Capture(ctx, active, dur); err != nil {
		return err
	}
	_ = a.st.RecordBranchDurable(ctx, store.BranchDurableRow{
		RepoID:      a.repoID,
		WorktreeID:  a.worktreeID,
		Engine:      a.eng.engine,
		DBKey:       active,
		Branch:      branch,
		DurableName: dur,
	})
	return nil
}

// captureBranchScopedOnTeardown snapshots a worktree's current active
// namespace into the current branch's durable copy before the worktree
// is deleted, so re-creating it (or another worktree that later swaps to
// the same branch) can resume. Best-effort: a failure must not block
// teardown. The active namespace itself is dropped by the normal
// teardown path afterward.
func captureBranchScopedOnTeardown(ctx context.Context, eng *branchEngine, st *store.Store, repoID, worktreeID int64, active string) error {
	branch, ok, err := st.GetActiveBranch(ctx, worktreeID, active)
	if err != nil || !ok || branch == "" {
		return err
	}
	exists, err := eng.drv.Exists(ctx, active)
	if err != nil || !exists {
		return err
	}
	dur := eng.durable(active, branch)
	if err := eng.drv.Capture(ctx, active, dur); err != nil {
		return err
	}
	// Record so the orphan sweep can reclaim this durable once `branch` is
	// gone — the worktree row is about to be soft-deleted, so the durable
	// must be tracked independently of it.
	_ = st.RecordBranchDurable(ctx, store.BranchDurableRow{
		RepoID:      repoID,
		WorktreeID:  worktreeID,
		Engine:      eng.engine,
		DBKey:       active,
		Branch:      branch,
		DurableName: dur,
	})
	return nil
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
	eng, closeEng, cerr := connectBranchEngine(ctx, cfg, d.Engine, siblingSlugs(ctx, st, repoID, worktreeID))
	if cerr != nil {
		return cerr
	}
	if eng == nil {
		return nil
	}
	defer closeEng()

	if capErr := captureBranchScopedOnTeardown(ctx, eng, st, repoID, worktreeID, active); capErr != nil {
		_ = st.WriteEvent(ctx, store.LevelWarn, store.EvtBranchCaptureError,
			capErr.Error(), repoID, worktreeID, "", 0, nil)
	}
	if err := eng.drv.Drop(ctx, active); err != nil {
		return err
	}
	_ = st.ClearActiveBranch(ctx, worktreeID, active)
	_ = st.ClearBranchMigratedForKey(ctx, worktreeID, active)
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtDBDrop,
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
		eng, closeEng, cerr := connectBranchEngine(ctx, cfg, d.Engine, siblingSlugs(ctx, st, repoID, worktreeID))
		if cerr != nil {
			return cerr
		}
		if eng == nil {
			continue
		}
		rerr := resetActiveNamespace(ctx, eng, st, repoID, worktreeID, active)
		closeEng()
		if rerr != nil {
			return rerr
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtDBReset,
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
func resetActiveNamespace(ctx context.Context, eng *branchEngine, st *store.Store, repoID, worktreeID int64, active string) error {
	// Drop the durable copy for whatever branch currently owns the active
	// slot (so a re-seed can't restore the stale copy).
	if branch, has, _ := st.GetActiveBranch(ctx, worktreeID, active); has && branch != "" {
		dur := eng.durable(active, branch)
		_ = eng.drv.DropDurable(ctx, dur)
		_ = st.DeleteBranchDurable(ctx, repoID, dur)
	}
	if err := eng.drv.Drop(ctx, active); err != nil {
		return err
	}
	_ = st.ClearBranchMigratedForKey(ctx, worktreeID, active)
	return st.ClearActiveBranch(ctx, worktreeID, active)
}

// BranchScopedSave reports one database handled by SaveBranchScoped.
type BranchScopedSave struct {
	Engine string `json:"engine"`
	Active string `json:"active"`
	// Branch is the branch whose durable copy was refreshed (the marker
	// owner). Empty when the save was skipped before marker resolution.
	Branch  string `json:"branch,omitempty"`
	Durable string `json:"durable,omitempty"`
	// Skipped carries the reason when no capture ran (no marker, active
	// missing, or provably unchanged since the last capture).
	Skipped string `json:"skipped,omitempty"`
}

// SaveBranchScoped captures every branch_scoped active namespace into the
// CURRENT branch's durable copy without a branch switch — the capture
// half of swapBranch as a manual checkpoint. Backs `treeman db save`.
// Other branches' durable copies are untouched. `engineFilter`
// (lowercased) restricts the save to one engine family when non-empty.
func SaveBranchScoped(
	ctx context.Context,
	cfg *config.Config,
	worktreePath string,
	repoID, worktreeID int64,
	st *store.Store,
	engineFilter string,
) ([]BranchScopedSave, error) {
	filterLabel := engineFilter
	if engineFilter != "" {
		if fam, ok := engine.Canonical(engineFilter); ok {
			filterLabel = string(fam)
		}
	}
	var saves []BranchScopedSave
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
			return saves, fmt.Errorf("render active namespace for %s: %w", d.Engine, err)
		}
		eng, closeEng, cerr := connectBranchEngine(ctx, cfg, d.Engine, siblingSlugs(ctx, st, repoID, worktreeID))
		if cerr != nil {
			return saves, cerr
		}
		if eng == nil {
			continue
		}
		s, serr := saveActiveNamespace(ctx, eng, st, repoID, worktreeID, active)
		closeEng()
		if serr != nil {
			return saves, serr
		}
		saves = append(saves, s)
		if s.Skipped == "" {
			_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtDBSave,
				fmt.Sprintf("%s: captured active %s into durable copy for %s", d.Engine, active, s.Branch),
				repoID, worktreeID, "", 0,
				map[string]string{"engine": d.Engine, "active": active, "branch": s.Branch, "durable": s.Durable})
		}
	}
	return saves, nil
}

// saveActiveNamespace captures one active namespace into its marker
// branch's durable copy. Reuses swapBranch's capture-skip lever: when the
// active provably still mirrors the durable (unchanged watermark), the
// capture is skipped rather than re-copied.
func saveActiveNamespace(
	ctx context.Context,
	eng *branchEngine,
	st *store.Store,
	repoID, worktreeID int64,
	active string,
) (BranchScopedSave, error) {
	out := BranchScopedSave{Engine: eng.engine, Active: active}
	branch, ok, err := st.GetActiveBranch(ctx, worktreeID, active)
	if err != nil {
		return out, err
	}
	if !ok || branch == "" {
		out.Skipped = "no active-branch marker — nothing owns this namespace yet (run prepare first)"
		return out, nil
	}
	out.Branch = branch
	exists, err := eng.drv.Exists(ctx, active)
	if err != nil {
		return out, err
	}
	if !exists {
		out.Skipped = "active namespace does not exist"
		return out, nil
	}
	dur := eng.durable(active, branch)
	out.Durable = dur
	prevClean, prevWM, hasClean, cerr := st.GetActiveBranchClean(ctx, worktreeID, active)
	if cerr == nil && hasClean {
		durExists, _ := eng.drv.Exists(ctx, dur)
		if w, werr := eng.drv.Watermark(ctx, active); werr == nil && captureSkippable(prevClean, durExists, prevWM, w) {
			out.Skipped = "unchanged since last capture (watermark match)"
			return out, nil
		}
	}
	if err := eng.drv.Capture(ctx, active, dur); err != nil {
		return out, fmt.Errorf("capture %s into durable copy for %q: %w", active, branch, err)
	}
	_ = st.RecordBranchDurable(ctx, store.BranchDurableRow{
		RepoID:      repoID,
		WorktreeID:  worktreeID,
		Engine:      eng.engine,
		DBKey:       active,
		Branch:      branch,
		DurableName: dur,
	})
	// Mark clean so the next swap (or save) can skip a redundant capture.
	if w, werr := eng.drv.Watermark(ctx, active); werr == nil && w != "" {
		_ = st.SetActiveBranchClean(ctx, repoID, worktreeID, active, branch, eng.engine, true, w)
	}
	return out, nil
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
		// Status only probes hash-derived durable namespaces (and reads the
		// marker), never enumerates or drops the active prefix — so no
		// sibling filter is needed.
		eng, closeEng, cerr := connectBranchEngine(ctx, cfg, d.Engine, nil)
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
