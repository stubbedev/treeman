// Package config loads `.treeman.yaml` plus the layered global +
// per-repo + per-worktree-local overrides.
//
// YAML round-trip parity is a requirement: loading and re-emitting
// a config must not silently drop unknown fields, so adding a new
// field requires touching both the struct and the test fixtures.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"gopkg.in/yaml.v3"
)

// Config is the top-level structure of a `.treeman.yaml` plus the
// global `~/.config/treeman/config.yaml`.
type Config struct {
	// Daemon process settings: socket path, log level, log database
	// location. Typically lives in the user-global config.
	Daemon DaemonConfig `yaml:"daemon,omitempty"`

	// Connection blocks per supported engine (MySQL, Postgres,
	// MongoDB, Redis, Elasticsearch). Treeman dials these to create
	// per-worktree clone databases, run migrations, and tail binlogs.
	Connections ConnectionsConfig `yaml:"connections,omitempty"`

	// Snapshot cache settings: where post-migration template
	// snapshots are cached on disk, plus retention/eviction policy.
	Snapshots SnapshotsConfig `yaml:"snapshots,omitempty"`

	// Identifies this repository in the registry. The `name` field is
	// used in registry keys, snapshot paths, and slug templates.
	// Defaults to the directory's basename when unset.
	Repo *RepoBlock `yaml:"repo,omitempty"`

	// Per-repo slug-template overrides. Slugs are the short
	// identifier (typically derived from the branch name) used in
	// database names, container labels, and on-disk paths.
	Slug SlugRules `yaml:"slug,omitempty"`

	// Worktree creation/deletion behaviour: root path, symlink mirrors,
	// async vs sync semantics for hooks.
	Worktrees WorktreesConfig `yaml:"worktrees,omitempty"`

	// .env file scoping rules. Controls which env files get rewritten
	// with the worktree slug, which are read as credential sources,
	// and any user-defined key/template patches.
	EnvScoping EnvScoping `yaml:"env_scoping,omitempty"`

	// One entry per database the project owns. Each entry pairs an
	// engine with a dump path, migration source, test-clone fanout,
	// and optional namespace template.
	Databases []DatabaseConfig `yaml:"databases,omitempty"`

	// Lifecycle hooks fired around worktree create/delete. `precreate`
	// runs sync (and can abort the operation); the other three phases
	// run async.
	Hooks HooksConfig `yaml:"hooks,omitempty"`

	// File-system + binlog watcher settings. The watcher invalidates
	// cached snapshots when migration files or DDL events change.
	Watcher WatcherConfig `yaml:"watcher,omitempty"`

	// User-defined migration frameworks keyed by name. Use this when
	// the built-in framework presets don't cover your tool — declare
	// the markers, migration dirs, file pattern, and hash policy
	// explicitly.
	Frameworks map[string]CustomFramework `yaml:"frameworks,omitempty"`
}

// DaemonConfig — `daemon:` block.
type DaemonConfig struct {
	// Unix-socket path the daemon listens on. Defaults to
	// `$XDG_RUNTIME_DIR/treeman.sock`. CLI clients dial the same
	// path; override only if the runtime dir is unwritable (CI, some
	// containers) or you want a per-project daemon.
	Socket string `yaml:"socket,omitempty"`

	// Log level for daemon stderr: `trace`, `debug`, `info` (default),
	// `warn`, `error`. Hook output is always captured regardless.
	LogLevel string `yaml:"log_level,omitempty"`

	// Path to the SQLite log database the daemon writes structured
	// events to. Defaults to `$XDG_STATE_HOME/treeman/logs.db`.
	// Inspected via `treeman logs query` / the `logs_query` MCP tool.
	DbLogPath string `yaml:"db_log_path,omitempty"`
}

// ConnectionsConfig — `connections:` block.
type ConnectionsConfig struct {
	// MySQL / MariaDB / TiDB connection. Set when any `databases:`
	// entry uses one of those engines. Treeman dials this server to
	// create clones, dump templates, and tail binlogs.
	Mysql *MysqlConn `yaml:"mysql,omitempty"`

	// PostgreSQL connection. Required when any `databases:` entry
	// uses `engine: postgres` (or the `postgresql` alias).
	Postgres *PostgresConn `yaml:"postgres,omitempty"`

	// MongoDB connection. URI form (`mongodb://...`); host/port get
	// rewritten at dial time when a `container` ref is set.
	Mongodb *MongoConn `yaml:"mongodb,omitempty"`

	// Redis connection. URL form (`redis://...`); ContainerRef
	// semantics match MongoDB.
	Redis *RedisConn `yaml:"redis,omitempty"`

	// Elasticsearch / OpenSearch connection. HTTP URL form.
	Elasticsearch *EsConn `yaml:"elasticsearch,omitempty"`
}

// MysqlConn — host/port/user. `Password` is runtime-only; never
// serialised. The resolver fills it from the repo's `.env*` files
// + process env.
//
// `Container` (optional): when set, treeman runs `<engine> inspect`
// on the container and uses either its published host-port mapping
// (preferred — works on macOS / Windows Docker Desktop) or its
// bridge-network IP (Linux) in place of `Host` before dialing.
//
// `ComposeService` is the alternative reference for docker-compose
// users: treeman matches the standard compose labels
// (`com.docker.compose.service` + `com.docker.compose.project`) and
// uses the matching running container. `ComposeProject` defaults to
// $COMPOSE_PROJECT_NAME when unset.
//
// `Network` (optional) pins which docker network's IP to use when
// the container is attached to several (compose adds a project
// network alongside any explicit ones).
//
// `ContainerEngine` is the engine binary — `docker` (default),
// `podman`, `nerdctl`, `finch`, `orbctl`. Any binary that supports
// `inspect` and `ps --filter label=...` works.
//
// When treeman itself runs inside a container (devcontainer, CI,
// etc.) and the engine socket isn't reachable, the inspect path is
// skipped and the configured `Host` is used as-is — which on a
// shared compose network is just the sibling service name. As a
// final fallback `host.docker.internal` is probed for host-loopback
// reachability.
type ContainerRef struct {
	// Container name or ID. When set, treeman runs
	// `<engine> inspect` on this container and rewrites the host/port
	// using its published port mapping (macOS, Windows Docker
	// Desktop) or bridge-network IP (Linux). Mutually exclusive with
	// `compose_service`.
	Container string `yaml:"container,omitempty"`

	// Docker Compose service name. Treeman finds the matching
	// container by the standard compose labels
	// (`com.docker.compose.service` + `com.docker.compose.project`).
	// Mutually exclusive with `container`.
	ComposeService string `yaml:"compose_service,omitempty"`

	// Docker Compose project name (the `-p` flag value). Defaults to
	// $COMPOSE_PROJECT_NAME, which compose itself derives from the
	// parent directory name. Only meaningful with `compose_service`.
	ComposeProject string `yaml:"compose_project,omitempty"`

	// Container engine binary: `docker` (default), `podman`,
	// `nerdctl`, `finch`, `orbctl`. Any binary that supports
	// `inspect` and `ps --filter label=...` works.
	ContainerEngine string `yaml:"container_engine,omitempty"`

	// Docker network name. When the container is attached to several
	// networks (compose adds a project network alongside any
	// explicit ones) this pins which network's IP treeman uses.
	Network string `yaml:"container_network,omitempty"`
}

// MysqlConn — host/port/user. `Password` is runtime-only; never
// serialised. The resolver fills it from the repo's `.env*` files
// + process env (and finally the container's own env when a
// ContainerRef is set).
type MysqlConn struct {
	// Hostname or IP the daemon dials. Defaults to `127.0.0.1`.
	// Ignored when `container` or `compose_service` is set and the
	// engine socket is reachable — the container's published port or
	// network IP is used instead.
	Host string `yaml:"host,omitempty"`

	// TCP port. Defaults to 3306. Same container-rewriting rules as
	// `host`.
	Port uint16 `yaml:"port,omitempty"`

	// Database user. Required. Should have privileges to
	// CREATE/DROP databases (for clones) and SHOW BINARY LOGS +
	// REPLICATION SLAVE (when the binlog watcher is enabled).
	User string `yaml:"user"`

	// Environment variable name holding the password. The daemon
	// reads it from the process env, the repo's `.env*` files, or
	// (last resort) the container's own env vars when a ContainerRef
	// is set.
	PasswordEnv *string `yaml:"password_env,omitempty"`

	// Resolved password — runtime-only. The `yaml:"-"` tag keeps it
	// out of serialised YAML so secrets never leak into snapshots.
	Password string `yaml:"-"`

	// Maximum open connections in the daemon's pool to this server.
	// Defaults to a per-engine safe value. Raise only if the server
	// is provisioned for it (max_connections raised, etc.).
	PoolMax      uint32 `yaml:"pool_max,omitempty"`
	ContainerRef `yaml:",inline"`
}

// PostgresConn — same shape as MysqlConn.
type PostgresConn struct {
	// Hostname or IP. Defaults to `127.0.0.1`. ContainerRef overrides
	// it at dial time when set.
	Host string `yaml:"host,omitempty"`

	// TCP port. Defaults to 5432.
	Port uint16 `yaml:"port,omitempty"`

	// Database role. Required. Needs CREATEDB to clone, and
	// REPLICATION when wire-protocol replay is enabled.
	User string `yaml:"user"`

	// Env var name holding the password. Same resolution order as
	// MysqlConn.PasswordEnv.
	PasswordEnv *string `yaml:"password_env,omitempty"`

	// Resolved password — runtime-only, never serialised.
	Password string `yaml:"-"`

	// Maximum open connections in the daemon's pool.
	PoolMax      uint32 `yaml:"pool_max,omitempty"`
	ContainerRef `yaml:",inline"`
}

// MongoConn — `mongodb://…` URI. When a ContainerRef is set, the
// URI's host/port are rewritten at dial time.
type MongoConn struct {
	// MongoDB connection URI (`mongodb://[user:pass@]host:port/[...]`).
	// Required. When a ContainerRef is set, host/port are rewritten
	// at dial time using the container's published mapping or IP.
	URI          string `yaml:"uri"`
	ContainerRef `yaml:",inline"`
}

// RedisConn — `redis://…` URL. Same ContainerRef semantics as MongoConn.
type RedisConn struct {
	// Redis connection URL (`redis://[:pass@]host:port[/db]`).
	// Required. ContainerRef rewrites host/port at dial time.
	URL          string `yaml:"url"`
	ContainerRef `yaml:",inline"`
}

// EsConn — Elasticsearch / OpenSearch HTTP URL. Same ContainerRef
// semantics as MongoConn.
type EsConn struct {
	// Elasticsearch / OpenSearch HTTP URL
	// (`http://host:9200` or `https://...`). Required.
	// ContainerRef rewrites host/port at dial time.
	URL          string `yaml:"url"`
	ContainerRef `yaml:",inline"`
}

// SnapshotsConfig — `snapshots:` block.
type SnapshotsConfig struct {
	// Directory where cached template snapshots (dumps, lockfiles,
	// hash manifests) live. Defaults to
	// `$XDG_CACHE_HOME/treeman/snapshots`. Should be on the same
	// filesystem as the worktrees root for fast hardlink-based
	// restores.
	CacheDir string `yaml:"cache_dir,omitempty"`

	// Retention/eviction policies controlling how many snapshots
	// per repo are kept and how aggressively they're pruned.
	Retention RetentionConfig `yaml:"retention,omitempty"`
}

// RetentionConfig — `snapshots.retention:` policies.
//
// `CapPerRepo` is the hard cap that triggers eviction on every
// `RecordSnapshot`. LRU rows above the cap are dropped immediately
// (in a background goroutine) so a busy worktree workflow never
// accumulates unbounded cached templates per repo.
//
// `KeepPerSource`, `MaxAgeDays`, `MaxTotalGb`, `GcIntervalMinutes`
// drive the periodic daemon-side sweep; they're not consulted by
// the inline-on-write eviction path.
type RetentionConfig struct {
	// Hard cap on cached snapshots per repository. Eviction runs
	// inline on every `RecordSnapshot`: rows above the cap (LRU
	// order) are dropped immediately in a background goroutine.
	// Default 8. Set to 0 to disable the inline cap (rely on the
	// periodic sweep only).
	CapPerRepo uint32 `yaml:"cap_per_repo,omitempty"`

	// Periodic-sweep policy: keep at most N snapshots per `source`
	// (a stable key derived from migration content). Default 500.
	KeepPerSource uint32 `yaml:"keep_per_source,omitempty"`

	// Periodic-sweep policy: drop snapshots older than N days.
	// Default 30.
	MaxAgeDays uint32 `yaml:"max_age_days,omitempty"`

	// Periodic-sweep policy: drop oldest snapshots once the cache
	// dir exceeds N gigabytes on disk. Default 50.
	MaxTotalGb uint32 `yaml:"max_total_gb,omitempty"`

	// Cadence (minutes) of the daemon's periodic snapshot-sweep
	// goroutine. Default 60.
	GcIntervalMinutes uint32 `yaml:"gc_interval_minutes,omitempty"`
}

// RepoBlock — `repo:` block.
type RepoBlock struct {
	// Repository identifier. Used in registry keys, snapshot cache
	// paths, slug templates, and database name templates. Defaults
	// to the directory's basename when unset. Should be stable
	// across machines so snapshots cached by one developer are
	// reusable by another.
	Name string `yaml:"name"`
}

// SlugRules — placeholder for future per-repo slug overrides.
type SlugRules struct {
	// Forced slug value, bypassing the branch-name derivation.
	// Mostly useful in CI where the branch name might be noisy
	// (`HEAD`, detached, PR-merge ref) and you want a deterministic
	// slug for the run.
	Override string `yaml:"override,omitempty"`
}

// WorktreesConfig — `worktrees:` block.
type WorktreesConfig struct {
	// Path (relative to the main worktree) where new worktrees are
	// created. Defaults to `.worktrees` to match the
	// `dotfiles gwt` zsh convention. Override with e.g.
	// `../foo-worktrees` for the sibling-dir convention.
	Root string `yaml:"root,omitempty"`

	// Files/directories from the main worktree to symlink into each
	// new worktree on create. Useful for committed-in-main-only
	// caches (e.g. `node_modules`, `vendor`) and tool configs that
	// shouldn't be duplicated.
	Links []string `yaml:"links,omitempty"`

	// When true (default), postcreate hooks run asynchronously: the
	// `worktree create` command returns immediately and the daemon
	// drives the hooks in the background. Set false for CI where you
	// want a synchronous failure if a hook breaks.
	AsyncCreate *bool `yaml:"async_create,omitempty"`

	// When true (default), predelete + postdelete hooks run async.
	AsyncDelete *bool `yaml:"async_delete,omitempty"`
}

// EnvScoping — `env_scoping:` block.
//
// `Files` is the WRITE list — `.env*` files that get patched with
// the per-worktree slug. `Sources` is the READ list used by the
// credential resolver: every path is read in order and later layers
// override earlier ones (so a `.env.testing.local` override beats
// the committed `.env.testing` baseline). When `Sources` is empty,
// the resolver falls back to the default search order:
//
//	.env  →  .env.local  →  .env.test  →  .env.testing
//	     →  .env.test.local  →  .env.testing.local
type EnvScoping struct {
	// WRITE list — `.env*` files that get rewritten in each new
	// worktree to embed the per-worktree slug (so DB_NAME, REDIS_DB,
	// etc. point at the cloned resources instead of the shared
	// ones). Each entry is a path relative to the worktree root.
	Files []string `yaml:"files,omitempty"`

	// READ list — paths the credential resolver consults in order
	// when looking up DB passwords and other secrets. Later layers
	// override earlier ones. When empty, the resolver falls back to
	// the default ordered search:
	// `.env` → `.env.local` → `.env.test` → `.env.testing` →
	// `.env.test.local` → `.env.testing.local`.
	Sources []string `yaml:"sources,omitempty"`

	// When true (default), treeman skips writing into the main
	// worktree's env files — only the per-worktree copies get
	// patched. Set false to also apply patches to the main worktree
	// (rare; mostly for single-worktree projects).
	SkipWorktree *bool `yaml:"skip_worktree,omitempty"`

	// Extra key/template pairs to apply on top of the default
	// slug-substitution patches. Use this for env keys treeman
	// doesn't know about by default (e.g. queue names, S3 prefixes).
	Patches []EnvPatch `yaml:"patches,omitempty"`
}

// EnvPatch — one `(key, template)` pair.
type EnvPatch struct {
	// Env variable key to overwrite, e.g. `DB_NAME`, `REDIS_DB`,
	// `S3_BUCKET_PREFIX`.
	Key string `yaml:"key"`

	// Template for the new value. Supports `{slug}` (the
	// per-worktree slug), `{repo}`, and `{branch}` placeholders.
	// Example: `app_{slug}` produces `app_feature-x` for a slug of
	// `feature-x`.
	Template string `yaml:"template"`
}

// HooksConfig — `hooks:` block.
type HooksConfig struct {
	// Sync phase: runs before the worktree is created. A non-zero
	// exit from any step aborts creation. Steps are list-shaped
	// single commands (no grouping, no container wrapping); use this
	// for cheap host-side checks (e.g. `command -v node`).
	Precreate []SingleStep `yaml:"precreate,omitempty"`

	// Async phase: runs after worktree create. Each entry is one
	// group; steps within a group run sequentially, groups run in
	// parallel. Groups can be wrapped in `<engine> exec` via
	// container/compose metadata.
	Postcreate []HookEntry `yaml:"postcreate,omitempty"`

	// Async phase: runs before worktree delete. Same grouping
	// semantics as postcreate. Use for graceful shutdown
	// (`docker compose stop`, draining queues).
	Predelete []HookEntry `yaml:"predelete,omitempty"`

	// Async phase: runs after worktree delete. Same grouping
	// semantics. Use for cleanup of resources outside the worktree
	// (CDN purges, Slack notifications).
	Postdelete []HookEntry `yaml:"postdelete,omitempty"`
}

// SingleStep — `{ run: "...", cwd: "..." }`. The bare-string YAML
// shorthand is decoded into one of these too.
type SingleStep struct {
	// Shell command to execute via `sh -c`. Required. Inherits the
	// daemon's environment plus any patches applied by EnvScoping.
	Run string `yaml:"run"`

	// Working directory (relative to the worktree root, or absolute).
	// Defaults to the worktree root when unset.
	Cwd string `yaml:"cwd,omitempty"`
}

// JSONSchema overrides the reflection-generated schema so editor
// hinting accepts both YAML shapes the UnmarshalYAML below decodes:
// a bare scalar string or a `{ run, cwd }` mapping.
func (SingleStep) JSONSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("run", &jsonschema.Schema{
		Type:        "string",
		Description: "Shell command to execute via `sh -c`. Required. Inherits the daemon's environment plus any EnvScoping patches.",
	})
	props.Set("cwd", &jsonschema.Schema{
		Type:        "string",
		Description: "Working directory (relative to the worktree root, or absolute). Defaults to the worktree root.",
	})
	return &jsonschema.Schema{
		Description: "A single command. Either a bare string (shorthand for `{ run: \"<cmd>\" }`) or a `{ run, cwd }` mapping.",
		OneOf: []*jsonschema.Schema{
			{
				Type:        "string",
				Description: "Bare command form. Equivalent to `{ run: \"<this string>\" }`. Executed via `sh -c` in the worktree root.",
			},
			{
				Type:                 "object",
				Properties:           props,
				Required:             []string{"run"},
				AdditionalProperties: jsonschema.FalseSchema,
				Description:          "Mapping form. Set `cwd` when the command needs to run in a subdirectory of the worktree.",
			},
		},
	}
}

// UnmarshalYAML accepts either a bare scalar (`"command"`) or a
// mapping (`{ run: ..., cwd: ... }`).
func (s *SingleStep) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		s.Run = node.Value
		return nil
	case yaml.MappingNode:
		type alias SingleStep
		return node.Decode((*alias)(s))
	default:
		return fmt.Errorf("step: want scalar or mapping, got node kind %d", node.Kind)
	}
}

// HookEntry — one of:
//   - bare command string  → group of one
//   - mapping with `run:`  → group of one with cwd
//   - mapping with `steps:` → explicit group; metadata fields
//     (container, compose_service, container_engine, ...) wrap all
//     steps in `docker exec` (or compose exec) at runtime.
//   - sequence of children → group sequence (chained with `&&`)
//
// `Container` / `ComposeService` are mutually exclusive. When set,
// every step in the group is wrapped in
// `<engine> exec [-w cwd] <id> sh -c '<cmd>'` so the work runs
// inside the named container instead of on the host. Use this to
// run install/migrate/seed commands inside the app dev container.
type HookEntry struct {
	Steps          []SingleStep
	Container      string
	ComposeService string
	ComposeProject string
	Engine         string
}

// JSONSchema overrides the reflection-generated schema so editor
// hinting accepts every shape UnmarshalYAML below decodes: bare
// command string, `{ run, cwd, ... }` mapping, `{ steps, ... }`
// group mapping, or a sequence of single-step children.
func (HookEntry) JSONSchema() *jsonschema.Schema {
	metaDescs := map[string]string{
		"in_container":     "Container name or ID to wrap every step in `<engine> exec`. Alias for `container`. Mutually exclusive with `compose_service`.",
		"container":        "Container name or ID to wrap every step in `<engine> exec`. Mutually exclusive with `compose_service`.",
		"compose_service":  "Docker Compose service name. Treeman finds the running container by the standard compose labels and wraps every step in `<engine> compose exec`.",
		"compose_project":  "Docker Compose project name (`-p` flag). Defaults to $COMPOSE_PROJECT_NAME / parent directory name. Only meaningful with `compose_service`.",
		"container_engine": "Container engine binary used for the exec wrap: `docker` (default), `podman`, `nerdctl`, `finch`, `orbctl`.",
		"engine":           "Alias for `container_engine`.",
	}
	addMeta := func(p *orderedmap.OrderedMap[string, *jsonschema.Schema]) {
		for _, k := range []string{
			"in_container", "container",
			"compose_service", "compose_project",
			"container_engine", "engine",
		} {
			p.Set(k, &jsonschema.Schema{Type: "string", Description: metaDescs[k]})
		}
	}

	runProps := orderedmap.New[string, *jsonschema.Schema]()
	runProps.Set("run", &jsonschema.Schema{
		Type:        "string",
		Description: "Shell command executed via `sh -c`. Wrapped in `<engine> exec` when container/compose metadata is set on this entry.",
	})
	runProps.Set("cwd", &jsonschema.Schema{
		Type:        "string",
		Description: "Working directory for the command. Relative paths resolve inside the worktree (host) or inside the container WORKDIR (when wrapped).",
	})
	addMeta(runProps)

	step := SingleStep{}.JSONSchema()
	stepsProps := orderedmap.New[string, *jsonschema.Schema]()
	stepsProps.Set("steps", &jsonschema.Schema{
		Type:        "array",
		Items:       step,
		Description: "Ordered list of single-step commands chained with `&&`. All steps share the entry's container/compose metadata.",
	})
	addMeta(stepsProps)

	return &jsonschema.Schema{
		Description: "One hook group. Groups run in parallel; steps within a group run sequentially. " +
			"Four shapes accepted: bare command string, `{ run, cwd, ...meta }` single-step mapping, " +
			"`{ steps, ...meta }` group mapping, or a sequence of single-step children.",
		OneOf: []*jsonschema.Schema{
			{
				Type:        "string",
				Description: "Bare command shorthand. Group of one step. No container wrapping possible in this form.",
			},
			{
				Type:                 "object",
				Properties:           runProps,
				Required:             []string{"run"},
				AdditionalProperties: jsonschema.FalseSchema,
				Description:          "Single-step mapping with optional container/compose metadata. Use when you want one command wrapped in `<engine> exec`.",
			},
			{
				Type:                 "object",
				Properties:           stepsProps,
				Required:             []string{"steps"},
				AdditionalProperties: jsonschema.FalseSchema,
				Description:          "Group form: explicit step list with shared container/compose metadata. All listed steps are wrapped in the same `<engine> exec` context.",
			},
			{
				Type:        "array",
				Items:       step,
				Description: "Sequence shorthand: list of single steps chained with `&&`. No container wrapping in this form — use the `{ steps, ... }` mapping if you need that.",
			},
		},
	}
}

// UnmarshalYAML decides which of the shapes applies based on the
// node kind. Strict: anything else is a typo we want to surface
// instead of silently coalesce.
func (h *HookEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		h.Steps = []SingleStep{{Run: node.Value}}
		return nil
	case yaml.MappingNode:
		// Reject the legacy `background:` field loudly. Hooks are
		// always async in v1.0+; if a YAML still carries that field
		// we want the user to see why their config no longer
		// parses.
		hasSteps := false
		for i := 0; i < len(node.Content); i += 2 {
			switch node.Content[i].Value {
			case "background":
				return fmt.Errorf("hook entry: `background` is no longer supported — hooks are always async; group commands by nesting them in a list to express sequencing (line %d)", node.Content[i].Line)
			case "steps":
				hasSteps = true
			}
		}
		if hasSteps {
			// Group form: { in_container/compose_service/..., steps: [...] }.
			var raw struct {
				Container      string       `yaml:"in_container"`
				ContainerAlt   string       `yaml:"container"`
				ComposeService string       `yaml:"compose_service"`
				ComposeProject string       `yaml:"compose_project"`
				Engine         string       `yaml:"container_engine"`
				EngineAlt      string       `yaml:"engine"`
				Steps          []SingleStep `yaml:"steps"`
			}
			if err := node.Decode(&raw); err != nil {
				return err
			}
			h.Container = firstNonEmpty(raw.Container, raw.ContainerAlt)
			h.ComposeService = raw.ComposeService
			h.ComposeProject = raw.ComposeProject
			h.Engine = firstNonEmpty(raw.Engine, raw.EngineAlt)
			h.Steps = raw.Steps
			if h.Container != "" && h.ComposeService != "" {
				return fmt.Errorf("hook entry (line %d): set either `in_container:` or `compose_service:`, not both", node.Line)
			}
			return nil
		}
		// Single-step form (existing): { run: ..., cwd: ..., [in_container/compose_service/...] }.
		var s SingleStep
		if err := node.Decode(&s); err != nil {
			return err
		}
		var meta struct {
			Container      string `yaml:"in_container"`
			ContainerAlt   string `yaml:"container"`
			ComposeService string `yaml:"compose_service"`
			ComposeProject string `yaml:"compose_project"`
			Engine         string `yaml:"container_engine"`
			EngineAlt      string `yaml:"engine"`
		}
		_ = node.Decode(&meta)
		h.Container = firstNonEmpty(meta.Container, meta.ContainerAlt)
		h.ComposeService = meta.ComposeService
		h.ComposeProject = meta.ComposeProject
		h.Engine = firstNonEmpty(meta.Engine, meta.EngineAlt)
		if h.Container != "" && h.ComposeService != "" {
			return fmt.Errorf("hook entry (line %d): set either `in_container:` or `compose_service:`, not both", node.Line)
		}
		h.Steps = []SingleStep{s}
		return nil
	case yaml.SequenceNode:
		var children []SingleStep
		for _, child := range node.Content {
			var s SingleStep
			if err := s.UnmarshalYAML(child); err != nil {
				return err
			}
			children = append(children, s)
		}
		h.Steps = children
		return nil
	default:
		return fmt.Errorf("hook entry: unsupported node kind %d", node.Kind)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// DatabaseConfig — one `databases:` entry. The `engine` discriminator
// gates which sub-fields are valid.
//
// `Fanout` is the optional override for the outer concurrency cap
// during clone restore + DropMatching. Leave unset (omitempty / 0)
// to use the safe per-engine default from internal/prepare. Raise
// only if the server is provisioned for it (max_connections raised,
// PG pg_database lock contention acceptable, etc.).
type DatabaseConfig struct {
	// Engine discriminator. Gates which connection block is dialed
	// and which sub-fields (dump, migrations, namespaces) are valid.
	// `postgresql` is an alias for `postgres`; `opensearch` is an
	// alias for `elasticsearch`.
	Engine string `yaml:"engine" jsonschema:"enum=mysql,enum=mariadb,enum=tidb,enum=postgres,enum=postgresql,enum=mongodb,enum=redis,enum=elasticsearch,enum=opensearch"`

	// Template for the per-worktree database/index name. Supports
	// `{slug}`, `{repo}`, `{branch}` placeholders. Example:
	// `app_{slug}` → `app_feature-x`. Required for engines that
	// scope by database name (MySQL, Postgres, Mongo).
	NameTemplate string `yaml:"name_template,omitempty"`

	// Source dump used to seed clones. Path is relative to the repo
	// root. Treeman hashes this file into the snapshot key, so
	// changes invalidate the cache.
	Dump *DumpSpec `yaml:"dump,omitempty"`

	// Migration source declaration. Treeman hashes the listed files
	// to decide whether a cached snapshot is still valid and replays
	// migrations into the template before cloning.
	Migrations *MigrationSpec `yaml:"migrations,omitempty"`

	// Test-clone fanout: how many parallel database clones to
	// pre-warm for paratest/pytest-xdist/Jest workers/etc.
	TestClones *TestClonesSpec `yaml:"test_clones,omitempty"`

	// Engine-specific namespacing templates (Redis db index,
	// Elasticsearch index prefix). Use when the engine doesn't
	// scope by database name.
	Namespaces *Namespaces `yaml:"namespaces,omitempty"`

	// Outer concurrency cap for clone restore + DropMatching.
	// Defaults to the per-engine safe value from internal/prepare.
	// Range 0–64. Raise only if the server is provisioned
	// (max_connections, PG pg_database lock contention, etc.).
	Fanout uint32 `yaml:"fanout,omitempty" jsonschema:"minimum=0,maximum=64"`
}

// DumpSpec — `dump:` sub-block of a DatabaseConfig.
type DumpSpec struct {
	// Path to the dump file relative to the repo root. Format must
	// match the engine — `.sql` for MySQL/Postgres, `.bson`/archive
	// for Mongo. The file is hashed into the snapshot key so changes
	// invalidate cached snapshots.
	Path string `yaml:"path"`

	// When true, a missing dump file is not an error — treeman will
	// build the template from migrations alone. Use for greenfield
	// projects that haven't created a baseline dump yet.
	Optional bool `yaml:"optional,omitempty"`
}

// MigrationSpec — `migrations:` sub-block. Fully declarative: every
// input the runtime needs (migration directories, file globs,
// lockfiles, hash mode, on-modify policy) is read from this struct,
// never inferred at runtime from `framework`. `framework` is a free-
// form label for logs + downstream tooling.
//
// `treeman init` emits these fields populated from the matching
// built-in preset; `treeman fw detect` lists the presets so you can
// copy fields in by hand. There is no implicit fallback — leaving
// e.g. MigrationDirs empty means treeman has no migration source
// for the hash, so the snapshot key won't change when files do.
type MigrationSpec struct {
	// Free-form label identifying the migration tool (`laravel`,
	// `flyway`, `golang-migrate`, `alembic`, etc.). Logged and
	// surfaced in `treeman fw detect`; never inspected by runtime
	// dispatch logic — every behavior is driven by the explicit
	// fields below.
	Framework string `yaml:"framework"`

	// Glob patterns (relative to repo root) for directories
	// containing migration files. Required for any behavior beyond
	// pure-dump templates.
	MigrationDirs []string `yaml:"migration_dirs,omitempty"`

	// Glob patterns for migration filenames within `migration_dirs`.
	// Example: `*.sql`, `*_*.up.sql`, `[0-9]*-*.sql`.
	FileGlobs []string `yaml:"file_globs,omitempty"`

	// Extra files whose contents are folded into the snapshot hash
	// (typically lockfiles like `composer.lock`, `package-lock.json`,
	// `go.sum`). Use when migrations are framework-managed and the
	// installed framework version itself affects schema.
	Lockfiles []string `yaml:"lockfiles,omitempty"`

	// Hash strategy. `filename` (default) hashes only the migration
	// filenames — fast, works when migrations are append-only.
	// `checksum` hashes file contents — required when migrations are
	// edited in-place during development.
	HashMode string `yaml:"hash_mode,omitempty" jsonschema:"enum=filename,enum=checksum"`

	// What to do when a migration file changes. `rebuild` (default)
	// drops the cached template and re-runs migrations from the
	// dump. `delta` runs only the new migrations on top of the
	// existing template — faster but assumes append-only history.
	OnModify string `yaml:"on_modify,omitempty" jsonschema:"enum=rebuild,enum=delta"`
}

// TestClonesSpec — `test_clones:` sub-block. Used by every parallel
// test runner (paratest, pest, pytest-xdist, Jest workers, Go
// `-parallel`, cargo nextest, …). `clones` is either `auto` (treeman
// reads the project's worker-count config) or an explicit integer.
type TestClonesSpec struct {
	// Number of test-clone databases to pre-warm. `auto` reads the
	// project's worker-count config (paratest's processes, pytest
	// -n, Jest maxWorkers). Explicit integer overrides; 0 disables
	// pre-warming entirely.
	Clones ClonesSetting `yaml:"clones,omitempty"`

	// Template for clone database names. Supports `{slug}`,
	// `{index}` (0-based clone index), `{repo}`. Required.
	// Example: `app_{slug}_test_{index}`.
	NameTemplate string `yaml:"name_template"`
}

// ClonesSetting — `clones: auto | <integer>`.
type ClonesSetting struct {
	Auto  bool
	Fixed uint32
}

// JSONSchema overrides the reflection-generated schema. The YAML
// shape is `auto` (string literal) or a non-negative integer; the
// reflected `{Auto, Fixed}` struct shape is an internal detail.
func (ClonesSetting) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Description: "Number of test-clone databases to pre-warm. " +
			"Either the literal string `auto` (treeman reads the project's " +
			"worker-count config) or a non-negative integer (0 disables pre-warming).",
		OneOf: []*jsonschema.Schema{
			{
				Type:        "string",
				Enum:        []any{"auto"},
				Description: "Read the worker count from the project's test runner config (paratest processes, pytest -n, Jest maxWorkers, …).",
			},
			{
				Type:        "integer",
				Minimum:     json.Number("0"),
				Description: "Explicit clone count. 0 disables pre-warming entirely.",
			},
		},
	}
}

// UnmarshalYAML parses `auto` or a non-negative integer.
func (c *ClonesSetting) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		if node.Value == "auto" || node.Value == "" {
			c.Auto = true
			return nil
		}
		var n uint32
		if err := node.Decode(&n); err != nil {
			return fmt.Errorf("clones: want 'auto' or non-negative integer, got %q", node.Value)
		}
		c.Fixed = n
		return nil
	}
	return fmt.Errorf("clones: want scalar")
}

// Namespaces — engine-specific namespacing (redis db-index,
// elasticsearch index prefix, etc.).
type Namespaces struct {
	// Redis-specific: template producing a numeric DB index per
	// worktree. Must evaluate to an integer in 0–15 (default Redis
	// config). Example: `{slug_hash16}` produces a stable hash in
	// the valid range.
	DbIndexTemplate string `yaml:"db_index_template,omitempty"`

	// Elasticsearch/OpenSearch-specific: template producing the
	// index-name prefix. All indexes the app creates are scoped by
	// this prefix per worktree. Example: `app_{slug}_`.
	IndexPrefixTemplate string `yaml:"index_prefix_template,omitempty"`

	// Generic key-prefix template for engines that scope by key
	// prefix rather than db/index (e.g. Redis key namespacing when
	// db-index isolation isn't enough).
	PrefixTemplate string `yaml:"prefix_template,omitempty"`
}

// WatcherConfig — `watcher:` block.
type WatcherConfig struct {
	// File-system globs the watcher monitors. When a matching file
	// changes the watcher invalidates affected snapshots (per the
	// glob's `on:` policy) so the next `prepare` rebuilds the
	// template.
	Paths []WatcherPath `yaml:"paths,omitempty"`

	// Debounce window in milliseconds. The watcher coalesces events
	// arriving within this window before invalidating snapshots,
	// which prevents thrashing when an editor saves many files at
	// once. Default 500.
	DebounceMs uint64 `yaml:"debounce_ms,omitempty"`

	// MySQL binary-log tailer settings. Disabled by default; enable
	// to replay DDL events from the source database onto cached
	// templates and test clones automatically.
	Binlog BinlogConfig `yaml:"binlog,omitempty"`
}

// BinlogConfig — `watcher.binlog:` block. Controls the MySQL
// binary-log tailer that replays DDL + DML events from the source
// database onto cached template + paratest clone databases. Off by
// default; enabling requires a server configured with
// `binlog_format=ROW` and a replication-privileged user.
type BinlogConfig struct {
	// Master switch. When false (default) the tailer is dormant —
	// no replication connection is opened, no DDL is replayed.
	Enabled bool `yaml:"enabled,omitempty"`
	// ServerID treeman registers as a fake replica. Must be unique
	// among all replicas the upstream server sees. Defaults to a
	// deterministic hash of the daemon's socket path so two
	// developers on the same host don't clash.
	ServerID uint32 `yaml:"server_id,omitempty"`
	// Flavor — "mysql" (default) or "mariadb".
	Flavor string `yaml:"flavor,omitempty" jsonschema:"enum=mysql,enum=mariadb"`
	// ApplyDDL toggles execution of DDL Query events. Default true.
	ApplyDDL *bool `yaml:"apply_ddl,omitempty"`
	// ApplyDML toggles execution of ROW events. Default false (DDL
	// replay is the high-value path; DML is a follow-up).
	ApplyDML *bool `yaml:"apply_dml,omitempty"`
}

// WatcherPath — one `paths:` entry.
type WatcherPath struct {
	// Filesystem glob (relative to repo root) the watcher monitors.
	// Supports `**` recursion. Example:
	// `database/migrations/**/*.sql`.
	Glob string `yaml:"glob"`

	// Invalidation strategy when a matching file changes:
	//   `auto`    — defer to the matching DatabaseConfig's `on_modify`.
	//   `delta`   — keep the cached template, replay only new files.
	//   `rebuild` — drop the template, replay everything from the dump.
	// Default: `auto`.
	On string `yaml:"on,omitempty" jsonschema:"enum=auto,enum=delta,enum=rebuild"`
}

// CustomFramework — `frameworks:` entry, lets users declare
// migration frameworks treeman doesn't know about natively.
type CustomFramework struct {
	// Files (relative to repo root) whose presence indicates this
	// framework is in use. Used by `treeman fw detect` to pick the
	// framework when no explicit MigrationSpec is configured.
	// Example: `["alembic.ini", "migrations/env.py"]`.
	Markers []string `yaml:"markers"`

	// Glob patterns for the directories holding migration files.
	// Copied into the MigrationSpec when detection picks this
	// framework.
	MigrationDirs []string `yaml:"migration_dirs"`

	// Glob pattern for individual migration files within
	// `migration_dirs`. Example: `[0-9]*_*.py` (alembic) or
	// `V*__*.sql` (flyway).
	FilePattern string `yaml:"file_pattern"`

	// Hash strategy applied to the migration files: `filename`
	// (default) or `checksum`. Same semantics as
	// MigrationSpec.HashMode.
	HashMode string `yaml:"hash_mode,omitempty" jsonschema:"enum=filename,enum=checksum"`

	// Change-handling policy: `rebuild` (default) or `delta`. Same
	// semantics as MigrationSpec.OnModify.
	OnModify string `yaml:"on_modify,omitempty" jsonschema:"enum=rebuild,enum=delta"`

	// Lockfiles whose contents are folded into the snapshot hash
	// (e.g. `requirements.txt`, `pyproject.toml`,
	// `composer.lock`).
	Lockfiles []string `yaml:"lockfiles,omitempty"`

	// Optional hint about the database engine this framework
	// targets — `mysql`, `postgres`, etc. Pre-fills the engine field
	// in `treeman init` when this framework is detected.
	EngineHint string `yaml:"engine_hint,omitempty"`
}

// LoadLayered reads global + repo + repo-local YAML files into a
// merged Config. Later layers override earlier. Missing files are
// ignored.
func LoadLayered(repoRoot string) (Config, error) {
	var cfg Config
	applyDefaults(&cfg)
	if g, ok := globalConfigPath(); ok {
		if err := mergeYAMLFile(&cfg, g); err != nil {
			return cfg, err
		}
	}
	if repoRoot != "" {
		if err := mergeYAMLFile(&cfg, filepath.Join(repoRoot, ".treeman.yaml")); err != nil {
			return cfg, err
		}
		if err := mergeYAMLFile(&cfg, filepath.Join(repoRoot, ".treeman.local.yaml")); err != nil {
			return cfg, err
		}
	}
	normaliseAliases(&cfg)
	return cfg, nil
}

// LoadLayeredForWorktree mirrors LoadLayered but also overlays the
// worktree's own `.treeman.local.yaml`. Used by the daemon's
// per-worktree fanout.
func LoadLayeredForWorktree(mainRoot, wtRoot string) (Config, error) {
	var cfg Config
	applyDefaults(&cfg)
	if g, ok := globalConfigPath(); ok {
		if err := mergeYAMLFile(&cfg, g); err != nil {
			return cfg, err
		}
	}
	if err := mergeYAMLFile(&cfg, filepath.Join(mainRoot, ".treeman.yaml")); err != nil {
		return cfg, err
	}
	if err := mergeYAMLFile(&cfg, filepath.Join(mainRoot, ".treeman.local.yaml")); err != nil {
		return cfg, err
	}
	if wtRoot != "" && wtRoot != mainRoot {
		if err := mergeYAMLFile(&cfg, filepath.Join(wtRoot, ".treeman.local.yaml")); err != nil {
			return cfg, err
		}
	}
	normaliseAliases(&cfg)
	return cfg, nil
}

// normaliseAliases collapses deprecated YAML keys onto their
// canonical fields after the layered merge completes. Currently a
// no-op; kept as a hook for future renames so the LoadLayered
// surface doesn't churn each time.
func normaliseAliases(cfg *Config) {}

func mergeYAMLFile(cfg *Config, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(false)
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// applyDefaults fills in the canonical defaults: async_create +
// async_delete true, skip_worktree true, retention defaults.
func applyDefaults(cfg *Config) {
	if cfg.Daemon.LogLevel == "" {
		cfg.Daemon.LogLevel = "info"
	}
	if cfg.Worktrees.Root == "" {
		// `<main>/.worktrees/` matches the dotfiles `gwt` zsh
		// convention so a treeman-controlled repo is drop-in for
		// the bash-hook flow. Override per-repo with e.g.
		// `worktrees.root: ../foo-worktrees` for the sibling-dir
		// convention.
		cfg.Worktrees.Root = ".worktrees"
	}
	if cfg.Worktrees.AsyncCreate == nil {
		t := true
		cfg.Worktrees.AsyncCreate = &t
	}
	if cfg.Worktrees.AsyncDelete == nil {
		t := true
		cfg.Worktrees.AsyncDelete = &t
	}
	if cfg.EnvScoping.SkipWorktree == nil {
		t := true
		cfg.EnvScoping.SkipWorktree = &t
	}
	if cfg.Snapshots.Retention.CapPerRepo == 0 {
		cfg.Snapshots.Retention.CapPerRepo = 8
	}
	if cfg.Snapshots.Retention.KeepPerSource == 0 {
		cfg.Snapshots.Retention.KeepPerSource = 500
	}
	if cfg.Snapshots.Retention.MaxAgeDays == 0 {
		cfg.Snapshots.Retention.MaxAgeDays = 30
	}
	if cfg.Snapshots.Retention.MaxTotalGb == 0 {
		cfg.Snapshots.Retention.MaxTotalGb = 50
	}
	if cfg.Snapshots.Retention.GcIntervalMinutes == 0 {
		cfg.Snapshots.Retention.GcIntervalMinutes = 60
	}
	if cfg.Watcher.DebounceMs == 0 {
		cfg.Watcher.DebounceMs = 500
	}
	if cfg.Watcher.Binlog.Flavor == "" {
		cfg.Watcher.Binlog.Flavor = "mysql"
	}
	if cfg.Watcher.Binlog.ApplyDDL == nil {
		t := true
		cfg.Watcher.Binlog.ApplyDDL = &t
	}
	if cfg.Watcher.Binlog.ApplyDML == nil {
		f := false
		cfg.Watcher.Binlog.ApplyDML = &f
	}
	if cfg.Watcher.Binlog.ServerID == 0 {
		// Stable per host: hash the daemon's effective socket path
		// (XDG_RUNTIME_DIR is per-user, so two users on one host get
		// distinct IDs). Values are kept in the 1k–1M range to leave
		// room for explicitly-numbered production replicas.
		var h uint32 = 2166136261
		for _, b := range []byte(os.Getenv("XDG_RUNTIME_DIR") + os.Getenv("USER")) {
			h ^= uint32(b)
			h *= 16777619
		}
		cfg.Watcher.Binlog.ServerID = 1000 + (h % 999000)
	}
}

func globalConfigPath() (string, bool) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "treeman", "config.yaml"), true
}
