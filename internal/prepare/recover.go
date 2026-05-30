package prepare

import (
	"context"
	"fmt"

	"github.com/stubbedev/treeman/internal/config"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// RecoverStaleWorktree resets the per-worktree primary namespace for
// every declared database so the next prepare can re-run end-to-end on
// a clean slate. Called by the daemon's stale-finalize sweep when a
// previous prepare was killed mid-flight (daemon restart, crash) and
// left engine state half-applied — most commonly half-applied
// migrations on the active DB, which would trip the rerun with
// duplicate-column / table-exists errors.
//
// Per engine, the recovery action depends on the database mode:
//
//   - test-clone (no branch_scoped) → drop the per-worktree source DB
//     plus every test-clone the slug owns. The next prepare's cold-
//     build path drops + recreates them anyway, but doing it here
//     means stale-finalize is observably recovered before the user
//     ever reruns.
//
//   - branch_scoped → drop the ACTIVE namespace WITHOUT the usual
//     pre-teardown Capture. The capture would clobber the durable
//     per-branch copy (which by definition holds pre-migration data,
//     the only clean recovery source) with the partially-migrated
//     active. The durable copy is left intact; the next prepare's
//     runBranchScoped sees !exists and re-fills from durable(branch).
//
// Errors per-engine are emitted as `wt_recovery_error` events at warn
// level and otherwise swallowed: a missing engine connection or a
// dropped server shouldn't block recovery of the others.
func RecoverStaleWorktree(
	ctx context.Context,
	cfg *config.Config,
	sl string,
	worktreePath string,
	repoID, worktreeID int64,
	st *store.Store,
) {
	if st == nil || cfg == nil {
		return
	}
	tplCtx := template.FromSlug(slug.Slug{Value: sl, Source: slug.SourceTicket})
	for _, d := range cfg.Databases {
		if d.BranchScoped {
			if err := recoverBranchScoped(ctx, cfg, d, worktreePath, repoID, worktreeID, st); err != nil {
				_ = st.WriteEvent(ctx, store.LevelWarn, "wt_recovery_error",
					fmt.Sprintf("engine=%s branch_scoped: %v", d.Engine, err),
					repoID, worktreeID, "", 0, map[string]string{
						"engine": d.Engine,
						"mode":   "branch_scoped",
						"error":  err.Error(),
					})
			}
			continue
		}
		if err := recoverTestClone(ctx, cfg, d, tplCtx, repoID, worktreeID, st); err != nil {
			_ = st.WriteEvent(ctx, store.LevelWarn, "wt_recovery_error",
				fmt.Sprintf("engine=%s: %v", d.Engine, err),
				repoID, worktreeID, "", 0, map[string]string{
					"engine": d.Engine,
					"error":  err.Error(),
				})
		}
	}
}

// recoverTestClone drops the per-worktree source DB (and its
// fingerprint-keyed test-clone family) so the next prepare cold-builds
// it fresh. Uses the engine driver's DropMatching helper which prefix-
// matches the slug-rendered base name — same primitive TeardownDatabases
// uses on `wt delete`, but without the surrounding hook + git plumbing.
func recoverTestClone(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	switch fam, _ := engine.Canonical(d.Engine); fam {
	case engine.FamilyMySQL:
		return recoverTestCloneMySQL(ctx, cfg, d, tplCtx, repoID, worktreeID, st)
	case engine.FamilyPostgres:
		return recoverTestClonePostgres(ctx, cfg, d, tplCtx, repoID, worktreeID, st)
	case engine.FamilyMongo:
		return recoverTestCloneMongo(ctx, cfg, d, tplCtx, repoID, worktreeID, st)
	case engine.FamilyRedis:
		return recoverTestCloneRedis(ctx, cfg, d, tplCtx, repoID, worktreeID, st)
	case engine.FamilyES:
		return recoverTestCloneES(ctx, cfg, d, tplCtx, repoID, worktreeID, st)
	}
	return nil
}

// recoverTestCloneMySQL drops the MySQL source DB family for stale-
// worktree recovery. Extracted verbatim from recoverTestClone's
// `mysql` switch arm.
func recoverTestCloneMySQL(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	if cfg.Connections.Mysql == nil {
		return nil
	}
	drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
	if err != nil {
		return err
	}
	defer func() { _ = drv.Close() }()
	name, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return err
	}
	dropped, err := drv.DropMatching(ctx, name)
	if err != nil {
		return err
	}
	emitRecoveryDrop(ctx, st, repoID, worktreeID, d.Engine, name, len(dropped))
	return nil
}

// recoverTestClonePostgres drops the Postgres source DB family for
// stale-worktree recovery. Extracted verbatim from recoverTestClone's
// `postgres` switch arm.
func recoverTestClonePostgres(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	if cfg.Connections.Postgres == nil {
		return nil
	}
	drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
	if err != nil {
		return err
	}
	defer func() { _ = drv.Close() }()
	name, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return err
	}
	dropped, err := drv.DropMatching(ctx, name)
	if err != nil {
		return err
	}
	emitRecoveryDrop(ctx, st, repoID, worktreeID, d.Engine, name, len(dropped))
	return nil
}

// recoverTestCloneMongo drops the MongoDB source DB family for stale-
// worktree recovery. Extracted verbatim from recoverTestClone's
// `mongodb` switch arm.
func recoverTestCloneMongo(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	if cfg.Connections.Mongodb == nil {
		return nil
	}
	drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
	if err != nil {
		return err
	}
	defer func() { _ = drv.Close(ctx) }()
	name, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return err
	}
	dropped, err := drv.DropMatching(ctx, name)
	if err != nil {
		return err
	}
	emitRecoveryDrop(ctx, st, repoID, worktreeID, d.Engine, name, len(dropped))
	return nil
}

// recoverTestCloneRedis drops the Redis key prefix for stale-worktree
// recovery. Extracted verbatim from recoverTestClone's `redis` arm.
func recoverTestCloneRedis(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	if cfg.Connections.Redis == nil {
		return nil
	}
	if d.KeyPrefix == "" {
		return nil
	}
	drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
	if err != nil {
		return err
	}
	defer func() { _ = drv.Close() }()
	prefix, err := template.Render(d.KeyPrefix, tplCtx)
	if err != nil {
		return err
	}
	dropped, err := drv.DropPrefix(ctx, prefix)
	if err != nil {
		return err
	}
	emitRecoveryDrop(ctx, st, repoID, worktreeID, d.Engine, prefix, dropped)
	return nil
}

// recoverTestCloneES drops the Elasticsearch / OpenSearch index prefix
// for stale-worktree recovery. Extracted verbatim from
// recoverTestClone's `elasticsearch` switch arm.
func recoverTestCloneES(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	if cfg.Connections.Elasticsearch == nil {
		return nil
	}
	if d.KeyPrefix == "" {
		return nil
	}
	drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
	if err != nil {
		return err
	}
	prefix, err := template.Render(d.KeyPrefix, tplCtx)
	if err != nil {
		return err
	}
	dropped, err := drv.DropMatching(ctx, prefix)
	if err != nil {
		return err
	}
	emitRecoveryDrop(ctx, st, repoID, worktreeID, d.Engine, prefix, len(dropped))
	return nil
}

// recoverBranchScoped drops the active namespace WITHOUT capturing
// it first. The captured copy would otherwise clobber the durable
// per-branch backup with partially-migrated data — exactly the
// scenario this recovery is trying to undo. The durable copy is
// left intact; the next prepare re-fills from it.
func recoverBranchScoped(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	worktreePath string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	scope, _, ok := branchScopeFor(d.Engine)
	if !ok {
		return nil
	}
	active, err := activeNamespace(d, scope, worktreePath)
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

	exists, err := eng.drv.Exists(ctx, active)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := eng.drv.Drop(ctx, active); err != nil {
		return err
	}
	_ = st.ClearActiveBranch(ctx, worktreeID, active)
	_ = st.WriteEvent(ctx, store.LevelInfo, "wt_recovery_drop",
		fmt.Sprintf("%s: dropped active %s (branch_scoped; durable kept)", d.Engine, active),
		repoID, worktreeID, "", 0, map[string]string{
			"engine": d.Engine,
			"mode":   "branch_scoped",
			"active": active,
		})
	return nil
}

func emitRecoveryDrop(ctx context.Context, st *store.Store, repoID, worktreeID int64, eng, target string, count int) {
	if st == nil {
		return
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, "wt_recovery_drop",
		fmt.Sprintf("%s: dropped %s* (%d)", eng, target, count),
		repoID, worktreeID, "", 0, map[string]any{
			"engine": eng,
			"target": target,
			"count":  count,
		})
}
