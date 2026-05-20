// Package prepare orchestrates the per-worktree bring-up: ensure
// the source DB exists, optionally load a dump, run the framework's
// migrate command, snapshot the source for cache reuse, fan out
// into N paratest clone DBs. Ported from
// `crates/treeman-prepare/src/lib.rs`.
//
// Crucially: `repoRoot` here is the MAIN checkout root, NOT the
// linked-worktree path. The Rust v0.3.x watcher passed the worktree
// path in by mistake which broke dump-file resolution (dumps live
// in the main checkout). The Go port forces the caller to be
// explicit about which root is which.
package prepare

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/dumpload"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	"github.com/stubbedev/treeman/internal/migrations/runner"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// Outcome — one row per database the orchestrator handled.
type Outcome struct {
	Engine       string
	SourceDB     string
	TemplateName string
	Fingerprint  string
	CacheHit     bool
	Clones       []string
}

// Run drives prepare for every database declared by cfg.Databases.
//
// `mainRepoRoot` is the main-checkout path (dump file lives there;
// framework migrate runs there). `worktreePath` is the linked
// worktree path the slug was derived from — used only for the
// `{slug}` context.
func Run(
	ctx context.Context,
	cfg *config.Config,
	mainRepoRoot string,
	worktreePath string,
	sl slug.Slug,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) ([]Outcome, error) {
	ctx2 := context.Background()
	_ = ctx2
	tplCtx := template.FromSlug(sl)
	var outcomes []Outcome
	for _, d := range cfg.Databases {
		switch d.Engine {
		case "mysql", "mariadb", "tidb":
			o, err := prepareMySQL(ctx, cfg, d, tplCtx, mainRepoRoot, st, repoID, worktreeID, inheritedEnv)
			if err != nil {
				return outcomes, err
			}
			outcomes = append(outcomes, o)
		default:
			// Mongo/redis/es don't need a source DB build — they're
			// scoped purely by name on prepare and dropped on
			// teardown. Skip for now.
		}
	}
	return outcomes, nil
}

func prepareMySQL(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	mainRepoRoot string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	if cfg.Connections.Mysql == nil {
		return Outcome{}, fmt.Errorf("connections.mysql not configured")
	}
	sourceDB, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render name_template: %w", err)
	}

	drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
	if err != nil {
		return Outcome{}, err
	}
	defer drv.Close()

	version, _ := drv.EngineVersion(ctx)

	// Build fingerprint inputs.
	migrationsHash := ""
	dumpHash := ""
	lockfileHashes := map[string]string{}
	framework := ""
	hashMode := ""
	if d.Migrations != nil {
		framework = d.Migrations.Framework
	}
	if d.Dump != nil {
		dp := filepath.Join(mainRepoRoot, d.Dump.Path)
		hashes, _ := snapshot.LockfileHashesFor([]string{dp})
		dumpHash = hashes[filepath.Base(dp)]
	}

	key := snapshot.New(d.Engine, version, sourceDB, framework, hashMode, migrationsHash, dumpHash, lockfileHashes)
	templateName := key.TemplateName()

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=mysql source=%s template=%s", sourceDB, templateName),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "mysql",
			"source_db":   sourceDB,
			"template":    templateName,
			"fingerprint": key.Fingerprint(),
		})

	// Cold build: drop+recreate source, load dump, run migrate,
	// snapshot for cache, fan out paratest clones.
	if _, err := drv.DropMatching(ctx, sourceDB); err != nil {
		return Outcome{}, err
	}
	if err := drv.EnsureDB(ctx, sourceDB); err != nil {
		return Outcome{}, err
	}
	if d.Dump != nil {
		dp := filepath.Join(mainRepoRoot, d.Dump.Path)
		if _, err := dumpload.LoadMySQL(ctx, drv.DB, sourceDB, dp); err != nil {
			return Outcome{}, fmt.Errorf("load dump %s: %w", dp, err)
		}
	}
	if d.Migrations != nil {
		out, err := runner.Run(ctx, framework, mainRepoRoot, sourceDB, runner.ModeUp, nil, inheritedEnv)
		if err != nil {
			return Outcome{}, fmt.Errorf("migrate source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			return Outcome{}, fmt.Errorf("migrate source %s exit %d: %s", sourceDB, out.ExitCode, out.StderrTail)
		}
	}

	// Build the template snapshot, then clone it into paratest DBs.
	if err := drv.SnapshotCreate(ctx, sourceDB, templateName); err != nil {
		return Outcome{}, fmt.Errorf("snapshot create %s → %s: %w", sourceDB, templateName, err)
	}

	clones, err := resolveCloneNames(d.Paratest, tplCtx, mainRepoRoot)
	if err != nil {
		return Outcome{}, err
	}
	for _, c := range clones {
		if err := drv.SnapshotRestore(ctx, templateName, c); err != nil {
			return Outcome{}, fmt.Errorf("restore %s → %s: %w", templateName, c, err)
		}
	}

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("clones=%d", len(clones)),
		repoID, worktreeID, "", 0, map[string]string{
			"source_db":   sourceDB,
			"template":    templateName,
			"clones":      fmt.Sprintf("%d", len(clones)),
		})

	return Outcome{
		Engine:       d.Engine,
		SourceDB:     sourceDB,
		TemplateName: templateName,
		Fingerprint:  key.Fingerprint(),
		CacheHit:     false,
		Clones:       clones,
	}, nil
}

func resolveCloneNames(p *config.ParatestSpec, tplCtx template.Context, repoRoot string) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	var n uint32
	if p.Clones.Auto {
		n = testfw.DetectedCloneCount(repoRoot)
		if n == 0 {
			n = testfw.NumCPUs()
		}
	} else {
		n = p.Clones.Fixed
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]string, 0, n)
	for i := uint32(1); i <= n; i++ {
		name, err := template.Render(p.NameTemplate, tplCtx.WithN(int(i)))
		if err != nil {
			return nil, fmt.Errorf("render paratest name_template: %w", err)
		}
		out = append(out, name)
	}
	return out, nil
}

// TeardownDatabases drops every per-worktree namespace declared by
// cfg.Databases. Errors per-engine are logged + swallowed so a
// missing redis doesn't block dropping mysql.
//
// Ported from `crates/treeman-prepare/src/teardown.rs`.
func TeardownDatabases(
	ctx context.Context,
	cfg *config.Config,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	tplCtx := template.FromSlug(slug.Slug{Value: sl, Source: slug.SourceTicket})

	for _, d := range cfg.Databases {
		err := teardownOne(ctx, cfg, d, tplCtx, sl, repoID, worktreeID, st)
		if err != nil {
			_ = st.WriteEvent(ctx, store.LevelWarn, "db_teardown_error",
				err.Error(), repoID, worktreeID, "", 0, nil)
		}
	}
	return nil
}

func teardownOne(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	switch d.Engine {
	case "mysql", "mariadb", "tidb":
		if cfg.Connections.Mysql == nil {
			return fmt.Errorf("connections.mysql not configured")
		}
		drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
		if err != nil {
			return err
		}
		defer drv.Close()
		name, err := template.Render(d.NameTemplate, tplCtx)
		if err != nil {
			return err
		}
		dropped, err := drv.DropMatching(ctx, name)
		if err != nil {
			return err
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_drop",
			fmt.Sprintf("mysql: %s (%d)", name, len(dropped)),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": "mysql", "slug": sl, "target": name, "count": len(dropped),
			})
		return nil
	case "mongodb":
		if cfg.Connections.Mongodb == nil {
			return fmt.Errorf("connections.mongodb not configured")
		}
		drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
		if err != nil {
			return err
		}
		defer drv.Close(ctx)
		name, err := template.Render(d.NameTemplate, tplCtx)
		if err != nil {
			return err
		}
		dropped, err := drv.DropMatching(ctx, name)
		if err != nil {
			return err
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_drop",
			fmt.Sprintf("mongodb: %s (%d)", name, len(dropped)),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": "mongodb", "slug": sl, "target": name, "count": len(dropped),
			})
		return nil
	case "redis":
		if cfg.Connections.Redis == nil {
			return fmt.Errorf("connections.redis not configured")
		}
		if d.Namespaces == nil {
			return fmt.Errorf("redis: missing namespaces.db_index_template")
		}
		drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
		if err != nil {
			return err
		}
		defer drv.Close()
		idxStr, err := template.Render(d.Namespaces.DbIndexTemplate, tplCtx)
		if err != nil {
			return err
		}
		var idx uint8
		_, err = fmt.Sscanf(idxStr, "%d", &idx)
		if err != nil {
			return fmt.Errorf("redis db index parse %q: %w", idxStr, err)
		}
		if err := drv.FlushDB(ctx, idx); err != nil {
			return err
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_flush",
			fmt.Sprintf("redis: db%d", idx),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": "redis", "slug": sl, "target": fmt.Sprintf("db%d", idx),
			})
		return nil
	case "elasticsearch", "opensearch":
		if cfg.Connections.Elasticsearch == nil {
			return fmt.Errorf("connections.elasticsearch not configured")
		}
		if d.Namespaces == nil {
			return fmt.Errorf("es: missing namespaces.index_prefix_template")
		}
		drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
		if err != nil {
			return err
		}
		prefix, err := template.Render(d.Namespaces.IndexPrefixTemplate, tplCtx)
		if err != nil {
			return err
		}
		dropped, err := drv.DropMatching(ctx, prefix)
		if err != nil {
			return err
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_drop",
			fmt.Sprintf("elasticsearch: %s (%d)", prefix, len(dropped)),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": "elasticsearch", "slug": sl, "target": prefix, "count": len(dropped),
			})
		return nil
	}
	return nil
}
