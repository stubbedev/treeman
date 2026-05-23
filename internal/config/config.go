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

	// Env-file credential resolution. Controls which `.env*` files
	// the resolver consults when looking up DB passwords and other
	// secrets. Patching of env / yaml / json / phpunit.xml files for
	// per-worktree scoping lives in the top-level `patches:` block.
	EnvScoping EnvScoping `yaml:"env_scoping,omitempty"`

	// Files to rewrite inside each worktree with per-worktree values
	// (slug-substituted DB names, cache prefixes, etc.). Supports
	// dotenv key=value files, phpunit.xml `<env>` blocks, generic
	// YAML, and generic JSON. When `skip_worktree: true` (default)
	// each patched file gets `git update-index --skip-worktree`
	// applied so the rewrite doesn't show up as a dirty file.
	//
	// Re-applied on every `treeman wt finalize` so a branch switch
	// inside an existing worktree re-evaluates each patch against
	// the new HEAD's slug.
	Patches []Patch `yaml:"patches,omitempty"`

	// One entry per database the project owns. Each entry pairs an
	// engine with a dump path, migration source, test-clone fanout,
	// and optional namespace template.
	Databases []DatabaseConfig `yaml:"databases,omitempty"`

	// Lifecycle hooks fired around worktree create/delete. Two phases:
	// `setup` (after create) and `teardown` (before delete). Run
	// async by default; `worktrees.async_create` / `async_delete`
	// control whether the CLI blocks on completion.
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
	// created. Defaults to `.worktrees`. Override with e.g.
	// `../foo-worktrees` for the sibling-dir convention.
	Root string `yaml:"root,omitempty"`

	// Paths (relative to the main worktree) to *symlink* into each
	// new worktree on create. Use for committed-in-main-only caches
	// that the worktree should read but never mutate per-branch —
	// e.g. `node_modules`, `vendor`. The symlink points at the main
	// worktree's copy so all worktrees share one on-disk cache.
	// Glob meta-characters expand against the main worktree root.
	Links []string `yaml:"links,omitempty"`

	// Paths (relative to the main worktree) to *copy* into each new
	// worktree on create. Use for gitignored files that the worktree
	// needs in its own copy so it can be patched per-branch without
	// affecting the main worktree's copy — e.g. `.env`, `.env.local`.
	// Glob meta-characters expand against the main worktree root.
	// Directories are recursed; existing destinations are left alone
	// (idempotent re-runs).
	Copies []string `yaml:"copies,omitempty"`

}

// EnvScoping — `env_scoping:` block. After the patches-block
// refactor, this carries only the credential resolver's READ list.
// `.env*` rewriting lives in the top-level `patches:` block.
//
// `Sources` is the ordered list the credential resolver consults
// when looking up DB passwords and other secrets. Later layers
// override earlier ones (so a `.env.testing.local` override beats
// the committed `.env.testing` baseline). When empty, the resolver
// falls back to the default search order:
//
//	.env  →  .env.local  →  .env.test  →  .env.testing
//	     →  .env.test.local  →  .env.testing.local
type EnvScoping struct {
	// READ list — paths the credential resolver consults in order
	// when looking up DB passwords and other secrets. Later layers
	// override earlier ones. When empty, the resolver falls back to
	// the default ordered search:
	// `.env` → `.env.local` → `.env.test` → `.env.testing` →
	// `.env.test.local` → `.env.testing.local`.
	Sources []string `yaml:"sources,omitempty"`
}

// Patch — one entry in the top-level `patches:` block. Each entry
// targets one file under the worktree root and rewrites it with
// per-worktree values via the `set:` map. Values are template
// strings that accept `{slug}`, `{slug_dash}`, `{slug_upper}`,
// `{slug_redis_queue}`, `{slug_redis_cache}`, `{repo}`, `{branch}`.
//
// The driver is picked from `format:` when set, otherwise auto-
// detected from the file extension:
//
//	dotenv  — `.env`, `.env.*`
//	phpunit — `.xml`, `.xml.dist` (phpunit.xml `<env>` blocks)
//	yaml    — `.yaml`, `.yml`
//	json    — `.json`
//	toml    — `.toml`
//	ini     — `.ini`, `.cfg`
//
// Path syntax inside `set:` is uniform across drivers:
//
//	dotenv / phpunit  — flat key (e.g. `DB_DATABASE`)
//	ini               — `section.key` (top-level keys allowed too)
//	yaml / json / toml — dotted path, optionally with `[N]` indices
//	                    (e.g. `services[0].host`)
//
// When `skip_worktree` is true (default), treeman calls
// `git update-index --skip-worktree` on the patched file so the
// rewrite doesn't show up as a dirty file. The file must be tracked
// by git for the skip-worktree call to do anything; gitignored
// files are patched in-place without any git interaction.
type Patch struct {
	// File path relative to the worktree root. Required.
	File string `yaml:"file"`

	// When true (default), apply `git update-index --skip-worktree`
	// after patching so the file doesn't show in `git status`.
	SkipWorktree *bool `yaml:"skip_worktree,omitempty"`

	// Driver name. Optional — leave unset to auto-detect from the
	// file extension. Explicit when the extension is ambiguous
	// (e.g. `phpunit` for a `.xml` that isn't standard XML).
	Format string `yaml:"format,omitempty" jsonschema:"enum=dotenv,enum=phpunit,enum=yaml,enum=json,enum=toml,enum=ini"`

	// Key → value-template map. Path syntax depends on the driver
	// (see the type doc-comment).
	Set map[string]string `yaml:"set,omitempty"`
}

// HooksConfig — `hooks:` block. A flat map keyed by trigger name.
// Each key's value is a list of Actions that fire when that trigger
// happens. Actions in the same list run in parallel; the trigger
// key itself encodes BOTH the lifecycle phase AND the timing point,
// so there's no separate `when:` field anywhere.
//
// Triggers (all optional — omit any you don't need):
//
//   • setup-before-engines — during `wt create`, after patches +
//     bring-in (copies/links), BEFORE engine prepare. Standard
//     home of dependency installs (composer/yarn/pip) so migrate
//     can find vendor/.
//   • setup-after-engines — during `wt create`, after engine
//     prepare. Use when actions need a populated database
//     (cache warming, seed verification).
//   • teardown-before-engines — during `wt delete`, BEFORE DB
//     drop. Graceful shutdown: drain queues, docker compose stop.
//   • teardown-after-engines — during `wt delete`, AFTER DB drop +
//     git worktree remove. External notifications (Slack, CDN
//     purge) that should announce only once the data is gone.
//   • on-head-change — fires when the HEAD watcher sees a branch
//     switch inside an existing worktree. Re-runs in addition to
//     the regular finalize-on-HEAD-change behaviour.
//   • on-watch — fires when any `watcher.paths` or
//     `databases[].watch` glob matches a filesystem event. Runs
//     alongside the engine re-prep.
//
// The map shape lets new triggers be added without touching every
// existing config. Daemon execution is always non-blocking from the
// CLI's perspective — each list of actions dispatches in parallel.
type HooksConfig struct {
	// SetupBeforeEngines — actions fire after worktree create +
	// patches + bring-in, before engine prepare.
	SetupBeforeEngines []Action `yaml:"setup-before-engines,omitempty"`

	// SetupAfterEngines — actions fire after engine prepare completes.
	SetupAfterEngines []Action `yaml:"setup-after-engines,omitempty"`

	// TeardownBeforeEngines — actions fire before DB drop on delete.
	TeardownBeforeEngines []Action `yaml:"teardown-before-engines,omitempty"`

	// TeardownAfterEngines — actions fire after DB drop + worktree
	// remove on delete.
	TeardownAfterEngines []Action `yaml:"teardown-after-engines,omitempty"`

	// OnHeadChange — actions fire when the HEAD watcher detects a
	// branch switch inside an existing worktree.
	OnHeadChange []Action `yaml:"on-head-change,omitempty"`

	// OnWatch — actions fire when any watcher.paths /
	// databases[].watch glob matches an event.
	OnWatch []Action `yaml:"on-watch,omitempty"`
}

// Action — one entry under `hooks.{setup,teardown}.actions`. Every
// action is a mapping; there are no shorthand forms.
//
//   • `run` is the work, as either a single shell string (one
//     command) or a list of shell strings (sequenced steps chained
//     with `&&`).
//   • `cwd` is the group-level working directory; all steps in the
//     action share it. Use multiple actions if you need different
//     cwds.
//   • `container` / `compose_service` (mutually exclusive) wrap the
//     whole action in `<engine> exec` / `<engine> compose exec` so
//     it runs inside the named container. `in_container` is an
//     accepted alias for `container`. `engine` is an alias for
//     `container_engine`.
//
// Actions in the same `actions:` list run in parallel; steps within
// one action run sequentially.
type Action struct {
	// Run is the shell command(s) for this action. The YAML accepts
	// either a single string or a list of strings; either way the
	// in-memory form is the list. Required.
	Run []string `yaml:"run"`

	// Cwd is the working directory for every step in this action.
	// Relative paths resolve against the worktree root (host) or
	// against the container's WORKDIR (when wrapped). Optional.
	Cwd string `yaml:"cwd,omitempty"`

	// Container name or ID. When set, every step runs via
	// `<engine> exec <id> sh -c '<cmd>'`. Mutually exclusive with
	// ComposeService.
	Container string `yaml:"container,omitempty"`

	// ComposeService is the docker-compose service name. Treeman
	// resolves the running container via the standard compose
	// labels and wraps every step in `<engine> compose exec`.
	// Mutually exclusive with Container.
	ComposeService string `yaml:"compose_service,omitempty"`

	// ComposeProject is the docker-compose project (`-p` flag).
	// Defaults to $COMPOSE_PROJECT_NAME / parent directory name.
	// Only meaningful with ComposeService.
	ComposeProject string `yaml:"compose_project,omitempty"`

	// Engine is the container engine binary: `docker` (default),
	// `podman`, `nerdctl`, `finch`, `orbctl`.
	Engine string `yaml:"container_engine,omitempty"`
}

// JSONSchema describes the YAML shape — one map per action, with
// `run` accepting string OR []string.
func (Action) JSONSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("run", &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "Single shell command executed via `sh -c`."},
			{Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "Ordered list of shell commands chained with `&&`."},
		},
		Description: "Shell work for this action. String = single step; list = sequenced steps chained with `&&`. Required.",
	})
	props.Set("cwd", &jsonschema.Schema{
		Type:        "string",
		Description: "Working directory for every step in this action. Relative paths resolve against the worktree root (host) or container WORKDIR (when wrapped).",
	})
	props.Set("container", &jsonschema.Schema{
		Type:        "string",
		Description: "Container name or ID. When set, every step is wrapped in `<engine> exec`. Mutually exclusive with `compose_service`.",
	})
	props.Set("in_container", &jsonschema.Schema{
		Type:        "string",
		Description: "Alias for `container`.",
	})
	props.Set("compose_service", &jsonschema.Schema{
		Type:        "string",
		Description: "Docker Compose service name. Wraps every step in `<engine> compose exec`. Mutually exclusive with `container`.",
	})
	props.Set("compose_project", &jsonschema.Schema{
		Type:        "string",
		Description: "Docker Compose project name (`-p` flag). Defaults to $COMPOSE_PROJECT_NAME or parent dir name. Only meaningful with `compose_service`.",
	})
	props.Set("container_engine", &jsonschema.Schema{
		Type:        "string",
		Description: "Container engine binary used for the exec wrap: `docker` (default), `podman`, `nerdctl`, `finch`, `orbctl`.",
	})
	props.Set("engine", &jsonschema.Schema{
		Type:        "string",
		Description: "Alias for `container_engine`.",
	})
	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           props,
		Required:             []string{"run"},
		AdditionalProperties: jsonschema.FalseSchema,
		Description:          "One action: a `run` (string or list of strings) plus optional cwd + container wrapping.",
	}
}

// UnmarshalYAML decodes one action map. Accepts both `run: string`
// and `run: [string, ...]`. Also accepts the `in_container` /
// `engine` aliases and rejects the legacy `background:` field +
// the obsolete `steps:` keyword loudly so old configs surface a
// clear error rather than silently mis-parse.
func (a *Action) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("action (line %d): want a mapping; bare strings + list shorthands have been removed — wrap each action in `{ run: ... }`", node.Line)
	}
	for i := 0; i < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "background":
			return fmt.Errorf("action: `background:` is no longer supported — hooks are always async (line %d)", node.Content[i].Line)
		case "steps":
			return fmt.Errorf("action: `steps:` is no longer supported — use `run:` with a list of strings (line %d)", node.Content[i].Line)
		}
	}
	var raw struct {
		Run            yaml.Node `yaml:"run"`
		Cwd            string    `yaml:"cwd"`
		Container      string    `yaml:"container"`
		ContainerAlt   string    `yaml:"in_container"`
		ComposeService string    `yaml:"compose_service"`
		ComposeProject string    `yaml:"compose_project"`
		Engine         string    `yaml:"container_engine"`
		EngineAlt      string    `yaml:"engine"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	switch raw.Run.Kind {
	case yaml.ScalarNode:
		a.Run = []string{raw.Run.Value}
	case yaml.SequenceNode:
		a.Run = make([]string, 0, len(raw.Run.Content))
		for _, child := range raw.Run.Content {
			if child.Kind != yaml.ScalarNode {
				return fmt.Errorf("action (line %d): every entry in `run:` must be a string", child.Line)
			}
			a.Run = append(a.Run, child.Value)
		}
	case 0:
		return fmt.Errorf("action (line %d): `run:` is required", node.Line)
	default:
		return fmt.Errorf("action (line %d): `run:` must be a string or list of strings", node.Line)
	}
	a.Cwd = raw.Cwd
	a.Container = firstNonEmpty(raw.Container, raw.ContainerAlt)
	a.ComposeService = raw.ComposeService
	a.ComposeProject = raw.ComposeProject
	a.Engine = firstNonEmpty(raw.Engine, raw.EngineAlt)
	if a.Container != "" && a.ComposeService != "" {
		return fmt.Errorf("action (line %d): set either `container:` or `compose_service:`, not both", node.Line)
	}
	return nil
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

	// Per-database watch list. When a file matching one of these
	// globs changes, the daemon re-prepares THIS database only. With
	// `on: rebuild` the cache-hit shortcut is skipped and the
	// template rebuilds from scratch; with `on: auto` (default) the
	// usual `migrations.on_modify` logic applies. Top-level
	// `watcher.paths` still works and applies to every database.
	Watch []WatcherPath `yaml:"watch,omitempty"`

	// Shell command run AFTER dump-load + migrations as the final
	// step of the cold-build path, BEFORE treeman snapshots the
	// populated database into its fingerprint-keyed template. Use
	// for engines that don't have a dump primitive (Mongo, Redis,
	// Elasticsearch) — declare seed scripts here and they run once
	// per fingerprint, then their output is cached + cloned for
	// every worktree.
	//
	// Same env-substitution shape as `migrations.migrate.env`:
	// `{target_db}` expands to the per-run database/namespace name.
	Seed *SeedSpec `yaml:"seed,omitempty"`
}

// SeedSpec — `databases[].seed:` sub-block. Declares the shell
// command treeman runs to populate a database's template state with
// non-migration data (fixtures, ES index mappings, Mongo seed
// documents, Redis warm-cache keys, etc.). Same shape as
// `migrations.migrate` so the same runner can execute both.
type SeedSpec struct {
	// Run is the shell command treeman invokes via `sh -c`. Example:
	// `node scripts/seed.js`. Required.
	Run string `yaml:"run"`

	// Env is a map of env-var names to value templates. Each entry
	// is set on the seed subprocess. `{target_db}` is substituted
	// with the resolved per-run database name; literal values pass
	// through unchanged.
	Env map[string]string `yaml:"env,omitempty"`
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
// input the runtime needs (the migrate command, its env overrides,
// migration directories, file globs, lockfiles, hash mode, on-modify
// policy) is read verbatim from this struct. There is no implicit
// fallback — leaving e.g. MigrationDirs empty means treeman has no
// migration source for the hash, so the snapshot key won't change
// when files do; leaving Migrate.Run empty makes `prepare` error.
//
// `treeman init` populates these fields from the framework presets
// in internal/migrations/framework; `treeman fw detect` lists the
// presets so you can hand-copy fields in. After scaffolding the YAML
// is the only source of truth.
type MigrationSpec struct {
	// Migrate is the shell command treeman runs to apply migrations
	// against a target database, plus the env-var overrides that
	// redirect the framework's CLI at that database. Required.
	Migrate *MigrationMigrate `yaml:"migrate,omitempty"`

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

// MigrationMigrate — `migrations.migrate:` sub-block. Declares the
// shell command treeman invokes to apply migrations and the env-var
// overrides that point the framework's CLI at the per-run template
// database.
//
// The framework's migrate command reads its target DB from the
// framework's own config (Laravel: `DB_DATABASE` in `.env`; Rails:
// `DATABASE`; Django: `DJANGO_DB_NAME`; etc.). Treeman builds
// per-worktree template databases with names like
// `myapp_template_feature-x` and needs to redirect the migrate
// command at *that* DB, not the one the committed `.env` references.
// The `Env` map says which env-var names to override; the value
// template `{target_db}` is substituted at runtime with the resolved
// database name. No other placeholders are supported.
type MigrationMigrate struct {
	// Run is the shell command treeman invokes via `sh -c`. Example:
	// `php artisan migrate --force`. Required; an empty Run aborts
	// `prepare` with a clear error rather than falling back to a
	// hardcoded default.
	Run string `yaml:"run"`

	// Env is a map of env-var names to value templates. Each entry
	// is set on the migrate subprocess (overriding the framework's
	// config file). `{target_db}` is substituted with the resolved
	// per-run database name; literal values pass through unchanged.
	Env map[string]string `yaml:"env,omitempty"`
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

// Namespaces — engine-specific namespacing.
type Namespaces struct {
	// Redis: legacy per-worktree DB index. Template must render to
	// an integer in 0-15. No template caching and no test_clones
	// fanout in this mode — kept for backward compat. Prefer
	// `prefix_template` for new configs.
	DbIndexTemplate string `yaml:"db_index_template,omitempty"`

	// Elasticsearch / OpenSearch: template producing the index-
	// name prefix. All indexes the app creates are scoped under
	// this prefix per worktree. Example: `app_{slug}_`.
	IndexPrefixTemplate string `yaml:"index_prefix_template,omitempty"`

	// Redis (preferred) and other prefix-isolated engines: template
	// producing the key prefix every worktree key lives under. One
	// Redis logical DB (0) holds every worktree's data, isolated
	// by prefix — lifts the 16-DB cap, works on cluster mode, and
	// enables full template caching + parallel fanout. The app must
	// honour the prefix (Laravel's `CACHE_PREFIX`, Rails
	// `Rails.cache.options[:namespace]`, ioredis `keyPrefix`).
	// Example: `{slug}:` produces `feature-x:` and the app reads
	// keys as `feature-x:cache:foo`.
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

// WatcherPath — one `paths:` entry under `watcher:` or `databases[].watch`.
type WatcherPath struct {
	// Filesystem glob (relative to repo root) the watcher monitors.
	// Supports `**` recursion. Example:
	// `database/migrations/**/*.sql`.
	Glob string `yaml:"glob"`

	// Invalidation strategy when a matching file changes:
	//   `auto`    — defer to the matching DatabaseConfig's `on_modify`.
	//   `delta`   — keep the cached template, replay only new files.
	//   `rebuild` — drop the template AND skip the cache-hit shortcut,
	//               rebuilding from the dump no matter what the
	//               database's `on_modify` says.
	// Default: `auto`.
	On string `yaml:"on,omitempty" jsonschema:"enum=auto,enum=delta,enum=rebuild"`

	// DBIndex is the index of the database in cfg.Databases this watch
	// belongs to. Populated by the daemon at watcher-start time when
	// aggregating top-level watcher.paths (DBIndex = -1, "applies to
	// every DB") with per-DB watches (DBIndex = i for databases[i]).
	// Not serialised — users only ever set Glob + On in YAML.
	DBIndex int `yaml:"-" json:"-"`
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
// skip_worktree true, retention defaults.
func applyDefaults(cfg *Config) {
	if cfg.Daemon.LogLevel == "" {
		cfg.Daemon.LogLevel = "info"
	}
	if cfg.Worktrees.Root == "" {
		// `<main>/.worktrees/` is the default sibling-style layout.
		// Override per-repo with e.g. `worktrees.root: ../foo-worktrees`
		// for the parent-dir convention.
		cfg.Worktrees.Root = ".worktrees"
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
