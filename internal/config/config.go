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
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"

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
	// per-worktree clone databases, run migrations.
	Connections ConnectionsConfig `yaml:"connections,omitempty"`

	// Snapshot cache settings: where post-migration template
	// snapshots are cached on disk, plus retention/eviction policy.
	Snapshots SnapshotsConfig `yaml:"snapshots,omitempty"`

	// Worktree creation/deletion behaviour: root path, symlink mirrors,
	// async vs sync semantics for hooks.
	Worktrees WorktreesConfig `yaml:"worktrees,omitempty"`

	// EnvSources is the ordered list of `.env*` files the credential
	// resolver consults when looking up DB passwords and other
	// secrets. Later entries override earlier ones. Empty falls back
	// to the default search order:
	//   .env → .env.local → .env.test → .env.testing →
	//   .env.test.local → .env.testing.local
	// Per-worktree rewriting of these files lives in `patches:`.
	EnvSources []string `yaml:"env_sources,omitempty"`

	// Files to rewrite inside each worktree with per-worktree values
	// (slug-substituted DB names, cache prefixes, etc.). Supports
	// dotenv key=value files, phpunit.xml `<env>` blocks, generic
	// YAML, JSON, TOML, and INI. Each patched file is wired through
	// git's clean/smudge filter so the rewrite is hidden from
	// `git status` while still letting `git pull` / `git checkout`
	// overwrite the file on incoming changes (the smudge filter
	// re-applies the per-worktree value on the way back to the
	// working tree).
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

	// DebounceMs is the file-watcher debounce window in
	// milliseconds. Coalesces editor save bursts into one re-prep
	// dispatch. Default 500.
	DebounceMs uint64 `yaml:"debounce_ms,omitempty"`

	// User-defined migration frameworks keyed by name. Use this when
	// the built-in framework presets don't cover your tool — declare
	// the markers, migration dirs, file pattern, and hash policy
	// explicitly.
	Frameworks map[string]CustomFramework `yaml:"frameworks,omitempty"`

	// Logs retention. Daemon-side prune drops rows older than
	// `keep_days` from the events, hook_runs, and hook_log_chunks
	// tables on a fixed interval. Set 0 to keep forever (no prune).
	Logs LogsConfig `yaml:"logs,omitempty"`

	// AutoFetch policy. Daemon-side periodic `git fetch --all --prune`
	// per registered repo, followed by a `git merge --ff-only @{u}`
	// per active worktree. Skips dirty trees, non-ff branches, and
	// upstreamless branches. Enabled by default at a 15-minute cadence.
	AutoFetch AutoFetchConfig `yaml:"auto_fetch,omitempty"`

	// MainWorktree opts the repo's main checkout (repo root) into the
	// same watcher-driven prepare/migrate/teardown lifecycle that
	// linked `.worktrees/<slug>` checkouts already get. Off by default
	// — flipping it on for an existing repo will start creating
	// per-branch databases when the user switches branches at the
	// repo root.
	MainWorktree MainWorktreeConfig `yaml:"main_worktree,omitempty"`

	// Ports declares per-worktree port slots. Each entry is a named
	// slot with a port range; treeman allocates a free port per slot
	// at `wt create` time and exposes it via the `{port_<name>}`
	// template token (usable in `patches[].set[*]` values).
	// Persisted in SQLite so the assignment survives across daemon
	// restarts; freed on `wt delete`.
	//
	// Use slot names that match the role they fill in your app (e.g.
	// `octane`, `webpack`, `reverb`) — the name shows up in every
	// `{port_<name>}` reference and in `wt show` output.
	Ports map[string]PortSpec `yaml:"ports,omitempty"`

	// Status configures the `treeman status` widget output (icons,
	// labels, hover lines, custom bar formats). Lives in the global
	// config since the widget aggregates worktrees across every repo.
	Status StatusConfig `yaml:"status,omitempty"`
}

// StatusConfig configures `treeman status` — the bar/waybar widget
// that aggregates worktree health across every registered repo. Each
// active worktree falls into one of four buckets:
//
//	stable  — ready (last finalize succeeded, or never ran)
//	up      — being prepared (finalize in progress)
//	down    — being torn down (teardown in progress)
//	failed  — last finalize errored
//
// All knobs below feed the `{key}` template syntax used elsewhere in
// `.treeman.yaml` (no separate templating engine). The built-in
// `--format` values are `icon`, `hover`, `waybar`, and `json`; entries
// in `formats` add or override named single-line formats.
type StatusConfig struct {
	// Icons holds the glyph for each bucket. Exposed to format
	// templates as `{icon_stable}` / `{icon_up}` / `{icon_down}` /
	// `{icon_failed}`, plus `{icon}` for the worst non-empty bucket.
	// Defaults to Nerd Font glyphs; set an icon to a single space to
	// suppress it (an empty string falls back to the default).
	Icons StatusBuckets `yaml:"icons,omitempty"`

	// Labels holds the text label for each bucket. Exposed as
	// `{label_stable}` etc. Defaults to the bucket name.
	Labels StatusBuckets `yaml:"labels,omitempty"`

	// Separator joins the segments of the built-in `icon` line and is
	// exposed to templates as `{sep}`. Default " | ".
	Separator string `yaml:"separator,omitempty"`

	// Header is the `{key}` template for each repo heading in the
	// built-in `hover` format. Tokens: `{repo}`, `{total}`,
	// `{stable}`, `{up}`, `{down}`, `{failed}` (repo-scoped counts).
	// Default "{repo}  ({total})".
	Header string `yaml:"header,omitempty"`

	// Row is the `{key}` template for each worktree line in the
	// built-in `hover` format. Tokens: `{branch}`, `{slug}`,
	// `{state}`, `{bucket}`, `{main}`, `{state_suffix}`, `{path}`,
	// `{icon}`. Default "  {main}{branch}{state_suffix}".
	Row string `yaml:"row,omitempty"`

	// MainMarker is substituted for `{main}` on a repo's main-worktree
	// row (empty string on linked worktrees). Default "★ ".
	MainMarker string `yaml:"main_marker,omitempty"`

	// Formats declares named single-line `{key}` templates selectable
	// with `treeman status --format <name>`. A name matching a
	// built-in (`icon`/`waybar`) overrides it. Available tokens match
	// the `icon` line: `{total}`, `{stable}`, `{up}`, `{down}`,
	// `{failed}`, `{icon_*}`, `{icon}`, `{label_*}`, `{class}`,
	// `{sep}`. A flat template cannot express the multi-line hover
	// body — customize that with `header` / `row` instead.
	Formats map[string]string `yaml:"formats,omitempty"`
}

// StatusBuckets carries one string per worktree bucket. Reused for
// both the `icons` and `labels` maps so the four bucket names stay in
// lockstep across the schema.
type StatusBuckets struct {
	Stable string `yaml:"stable,omitempty"`
	Up     string `yaml:"up,omitempty"`
	Down   string `yaml:"down,omitempty"`
	Failed string `yaml:"failed,omitempty"`
}

// AutoFetchConfig — `auto_fetch:` block. Periodic daemon-side
// `git fetch --all --prune` per registered repo, followed by a best-
// effort `git merge --ff-only @{u}` per active worktree. Pull is
// safe: it skips when the working tree is dirty, when the branch
// has no upstream, or when the merge would not be a fast-forward.
// Each failure logs a warning and the loop continues.
//
// Use `enabled: false` (per-repo `.treeman.yaml` override) to opt a
// repo out, e.g. when a separate scheduler already pulls or when
// network policy forbids unsolicited fetches.
type AutoFetchConfig struct {
	// Enabled toggles the loop. Default true. A pointer would let the
	// schema distinguish "unset" from "explicit false", but defaults
	// are applied centrally in applyDefaults and there is no
	// inheritance subtlety — a missing key means "use default".
	Enabled *bool `yaml:"enabled,omitempty"`

	// Cadence in minutes. Default 15. Minimum 1 (values below are
	// clamped at use-site to avoid a runaway tight loop on a typo).
	IntervalMinutes uint32 `yaml:"interval_minutes,omitempty"`

	// Mode controls how a worktree's HEAD is advanced after fetch.
	//   - "ff" (default): `git merge --ff-only @{u}`. Refuses on
	//     divergence; user's unpushed commits are never touched.
	//   - "rebase": `git rebase --autostash @{u}`. Replays local
	//     commits on top of upstream. Auto-aborts on conflict so the
	//     working tree never lands in a half-rebased state.
	// Opt-in only — keep the safe ff path as the default.
	Mode string `yaml:"mode,omitempty" jsonschema:"enum=ff,enum=rebase"`
}

// IsEnabled returns the resolved on/off value, treating nil
// (`enabled:` unset) as the default-on case.
func (a AutoFetchConfig) IsEnabled() bool {
	if a.Enabled == nil {
		return true
	}
	return *a.Enabled
}

// ResolvedMode normalises Mode into the canonical "ff" / "rebase"
// values, defaulting to "ff" for any unknown / empty string.
func (a AutoFetchConfig) ResolvedMode() string {
	switch a.Mode {
	case "rebase":
		return "rebase"
	default:
		return "ff"
	}
}

// MainWorktreeConfig — `main_worktree:` block. Opt-in handle that
// promotes the repo root into a first-class worktree so the daemon's
// HEAD watcher, file watcher, and prepare orchestration treat it the
// same as a `.worktrees/<slug>` linked checkout. Off by default; flip
// `enabled: true` to start producing per-branch databases at the
// repo root.
//
// One knob today (Enabled). The branch-switch policy is implicit:
// FinalizeWorktree re-runs against the new branch's slug, which uses
// the snapshot cache so a re-visit of any previously-seen branch is
// a near-instant clone. Drop/keep policies will land as additional
// fields once their teardown plumbing exists — premature wiring of
// `on_branch_switch` would have shipped a config surface no code
// consumed.
type MainWorktreeConfig struct {
	// Enabled toggles main-wt enrollment for this repo. Default false
	// — every existing install sees zero behaviour change until the
	// flag is set. Once true, the daemon ensures a worktrees row with
	// is_main=1 exists for the repo, spawns the per-wt HEAD + file
	// watchers against the repo root, and re-runs prepare on every
	// branch switch.
	Enabled bool `yaml:"enabled,omitempty"`

	// Databases is a sparse, index-aligned overlay over `databases:`.
	// Each entry's set fields replace the same field on the main-
	// indexed database when finalize runs against the main worktree;
	// unset fields inherit the top-level value. Linked worktrees are
	// untouched.
	//
	// Common uses: a different `name_template` so the main wt's app
	// DB lives at `app_dev_{slug}` while linked wts use `app_{slug}`;
	// disabling test-clone fanout in main (`test_clones.clones: 0`)
	// because the main checkout doesn't run parallel test workers.
	//
	// Overlay length must be <= len(databases). Entries beyond the
	// declared count are a config error caught at load time.
	Databases []DatabaseOverlay `yaml:"databases,omitempty"`
}

// DatabaseOverlay holds the subset of `DatabaseConfig` fields that
// can be tweaked per-context (main wt vs linked wt). Engine + Dump
// are intentionally absent — changing engines per-context produces
// schema chaos, and main / linked worktrees should always share the
// same seed dump for snapshot-cache coherence.
//
// Field semantics:
//
//   - Strings: empty value means "inherit". Setting to a non-empty
//     value replaces the base template.
//   - TestClones: nil pointer means "inherit". A non-nil value
//     replaces the entire spec (Clones + NameTemplate).
//   - Fanout: nil pointer means "inherit". A non-nil value (including
//     0) replaces the base — necessary because uint32(0) is a valid
//     "use per-engine default" sentinel that's distinct from "no
//     override".
type DatabaseOverlay struct {
	NameTemplate string          `yaml:"name_template,omitempty"`
	KeyPrefix    string          `yaml:"key_prefix,omitempty"`
	TestClones   *TestClonesSpec `yaml:"test_clones,omitempty"`
	Fanout       *uint32         `yaml:"fanout,omitempty"`
}

// LogsConfig — `logs:` block. One knob: how long to keep events +
// hook_runs + their attached log chunks. The daemon's prune loop
// applies the cutoff per table; chunks cascade via FK so dropping
// a hook_runs row also drops its captured stdout/stderr.
type LogsConfig struct {
	// KeepDays sets the retention window in days. Rows older than
	// `now - keep_days * 24h` are removed on each daemon prune tick.
	// 0 disables pruning (keep forever). Default 14.
	KeepDays int `yaml:"keep_days,omitempty"`
}

// DaemonConfig — `daemon:` block. Only `log_level` is user-tunable;
// the socket path and event-log location are derived from
// $XDG_RUNTIME_DIR / $XDG_STATE_HOME respectively and are not
// configurable through YAML (one source of truth — the runtime
// dirs the OS already manages).
type DaemonConfig struct {
	// Log level for daemon stderr: `debug`, `info` (default),
	// `warn`, `error`. Read once at startup; reload by restarting
	// the daemon. Hook output is always captured regardless.
	LogLevel string `yaml:"log_level,omitempty"`
}

// ConnectionsConfig — `connections:` block.
type ConnectionsConfig struct {
	// MySQL / MariaDB / TiDB connection. Set when any `databases:`
	// entry uses one of those engines. Treeman dials this server to
	// create clones, dump templates.
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
	// CREATE/DROP databases (for clones).
	User string `yaml:"user"`

	// Password is either a literal value or a `$NAME` / `${NAME}`
	// reference to an env var. Refs are resolved from
	// `env_sources:` files + the process env + (last resort) the
	// container's own env vars when a ContainerRef is set.
	//
	// Use a ref in any setup beyond local dev — embedding a literal
	// password in committed YAML is a security anti-pattern.
	Password string `yaml:"password,omitempty"`

	// Maximum open connections in the daemon's pool to this server.
	// Defaults to a per-engine safe value. Raise only if the server
	// is provisioned for it (max_connections raised, etc.).
	PoolMax uint32 `yaml:"pool_max,omitempty"`

	ContainerRef `yaml:",inline"`
}

// UnmarshalYAML accepts either a bare DSN string
// (`mysql://user:pass@host:port/db`) or the structured object.
// DSN form is for the common dev case where one URL captures
// everything. Use the structured form when you need fine-grained
// fields like `container`, `pool_max`, or container refs.
//
// DSN trade-offs:
//   - Password is embedded in the URL — fine for dev, never for prod.
//     For prod use `password: $ENVNAME` in the structured form.
//   - Container/compose refs aren't expressible in a single URL —
//     drop down to the structured form when you need them.
func (c *MysqlConn) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return parseMysqlDSN(node.Value, c)
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("mysql connection (line %d): want a DSN string or mapping", node.Line)
	}
	type alias MysqlConn
	return node.Decode((*alias)(c))
}

// JSONSchema for MysqlConn: scalar DSN OR full structured object.
func (MysqlConn) JSONSchema() *jsonschema.Schema {
	r := &jsonschema.Reflector{Anonymous: true, ExpandedStruct: true, FieldNameTag: "yaml"}
	obj := r.Reflect(&struct {
		Host         string       `yaml:"host,omitempty"`
		Port         uint16       `yaml:"port,omitempty"`
		User         string       `yaml:"user"`
		Password     string       `yaml:"password,omitempty"`
		PoolMax      uint32       `yaml:"pool_max,omitempty"`
		ContainerRef ContainerRef `yaml:",inline"`
	}{})
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "DSN: `mysql://user:pass@host:port/dbname`. Equivalent to the structured form below."},
			obj,
		},
		Description: "MySQL connection — bare DSN string OR structured object.",
	}
}

// parseMysqlDSN fills cfg from a URL-form DSN.
func parseMysqlDSN(dsn string, cfg *MysqlConn) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse mysql DSN: %w", err)
	}
	if u.Scheme != "mysql" && u.Scheme != "mariadb" {
		return fmt.Errorf("mysql DSN: scheme must be mysql(:|maria:)//, got %q", u.Scheme)
	}
	cfg.Host = u.Hostname()
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return fmt.Errorf("mysql DSN port: %w", err)
		}
		if n < 0 || n > 65535 {
			return fmt.Errorf("mysql DSN port out of range: %d", n)
		}
		cfg.Port = uint16(n)

	}
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Password = pw
		}
	}
	return nil
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

	// Password is either a literal value or a `$NAME` / `${NAME}`
	// env-var reference. See MysqlConn.Password for the resolution
	// order and the security warning about literals.
	Password string `yaml:"password,omitempty"`

	// Maximum open connections in the daemon's pool.
	PoolMax      uint32 `yaml:"pool_max,omitempty"`
	ContainerRef `       yaml:",inline"`
}

// UnmarshalYAML accepts either a bare DSN string
// (`postgres://user:pass@host:port/db`) or the structured object.
// Mirrors MysqlConn — see its doc-comment for the trade-offs.
func (c *PostgresConn) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return parsePostgresDSN(node.Value, c)
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("postgres connection (line %d): want a DSN string or mapping", node.Line)
	}
	type alias PostgresConn
	return node.Decode((*alias)(c))
}

// JSONSchema for PostgresConn: scalar DSN OR full structured object.
func (PostgresConn) JSONSchema() *jsonschema.Schema {
	r := &jsonschema.Reflector{Anonymous: true, ExpandedStruct: true, FieldNameTag: "yaml"}
	obj := r.Reflect(&struct {
		Host         string       `yaml:"host,omitempty"`
		Port         uint16       `yaml:"port,omitempty"`
		User         string       `yaml:"user"`
		Password     string       `yaml:"password,omitempty"`
		PoolMax      uint32       `yaml:"pool_max,omitempty"`
		ContainerRef ContainerRef `yaml:",inline"`
	}{})
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{
				Type:        "string",
				Description: "DSN: `postgres://user:pass@host:port/dbname?sslmode=disable`. Equivalent to the structured form below.",
			},
			obj,
		},
		Description: "Postgres connection — bare DSN string OR structured object.",
	}
}

// parsePostgresDSN fills cfg from a URL-form DSN.
func parsePostgresDSN(dsn string, cfg *PostgresConn) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse postgres DSN: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("postgres DSN: scheme must be postgres(ql)://, got %q", u.Scheme)
	}
	cfg.Host = u.Hostname()
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return fmt.Errorf("postgres DSN port: %w", err)
		}
		if n < 0 || n > 65535 {
			return fmt.Errorf("postgres DSN port out of range: %d", n)
		}
		cfg.Port = uint16(n)

	}
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Password = pw
		}
	}
	return nil
}

// MongoConn — `mongodb://…` URI. When a ContainerRef is set, the
// URI's host/port are rewritten at dial time.
type MongoConn struct {
	// MongoDB connection URI (`mongodb://[user:pass@]host:port/[...]`).
	// Required. When a ContainerRef is set, host/port are rewritten
	// at dial time using the container's published mapping or IP.
	URI string `yaml:"uri"`

	// Maximum open connections in the driver's pool (maxPoolSize).
	// Defaults to the driver's own default when unset. Same knob as
	// the SQL engines' pool_max.
	PoolMax      uint32 `yaml:"pool_max,omitempty"`
	ContainerRef `       yaml:",inline"`
}

func (c *MongoConn) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		c.URI = node.Value
		return nil
	}
	type alias MongoConn
	return node.Decode((*alias)(c))
}

func (MongoConn) JSONSchema() *jsonschema.Schema { return uriOrMap("mongodb", "uri") }

// RedisConn — `redis://…` URL. Same ContainerRef semantics as MongoConn.
type RedisConn struct {
	// Redis connection URL (`redis://[:pass@]host:port[/db]`).
	// Required. ContainerRef rewrites host/port at dial time.
	URL string `yaml:"url"`

	// Maximum connections in the driver's pool (PoolSize). Defaults to
	// the driver's own default when unset. Same knob as the SQL
	// engines' pool_max.
	PoolMax      uint32 `yaml:"pool_max,omitempty"`
	ContainerRef `       yaml:",inline"`
}

func (c *RedisConn) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		c.URL = node.Value
		return nil
	}
	type alias RedisConn
	return node.Decode((*alias)(c))
}

func (RedisConn) JSONSchema() *jsonschema.Schema { return uriOrMap("redis", "url") }

// EsConn — Elasticsearch / OpenSearch HTTP URL. Same ContainerRef
// semantics as MongoConn.
type EsConn struct {
	// Elasticsearch / OpenSearch HTTP URL
	// (`http://host:9200` or `https://...`). Required.
	// ContainerRef rewrites host/port at dial time.
	URL string `yaml:"url"`

	// Maximum simultaneous HTTP connections to the cluster
	// (http.Transport.MaxConnsPerHost). Defaults to the Go HTTP
	// default when unset. Same knob as the SQL engines' pool_max.
	PoolMax      uint32 `yaml:"pool_max,omitempty"`
	ContainerRef `       yaml:",inline"`
}

func (c *EsConn) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		c.URL = node.Value
		return nil
	}
	type alias EsConn
	return node.Decode((*alias)(c))
}

func (EsConn) JSONSchema() *jsonschema.Schema { return uriOrMap("elasticsearch", "url") }

// uriOrMap builds a polymorphic schema for the Mongo/Redis/ES
// connection blocks: scalar URI string OR a structured object that
// pairs the URI field with the embedded ContainerRef.
func uriOrMap(engine, urlField string) *jsonschema.Schema {
	objProps := orderedmap.New[string, *jsonschema.Schema]()
	objProps.Set(urlField, &jsonschema.Schema{Type: "string"})
	objProps.Set(
		"pool_max",
		&jsonschema.Schema{
			Type:        "integer",
			Minimum:     json.Number("0"),
			Description: "Max connections in the driver's pool. Optional override; unset lets the driver default + the server-aware clone fanout govern concurrency.",
		},
	)
	objProps.Set("container", &jsonschema.Schema{Type: "string"})
	objProps.Set("compose_service", &jsonschema.Schema{Type: "string"})
	objProps.Set("compose_project", &jsonschema.Schema{Type: "string"})
	objProps.Set("container_engine", &jsonschema.Schema{Type: "string"})
	objProps.Set("container_network", &jsonschema.Schema{Type: "string"})
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "Bare URL/URI. Equivalent to `{" + urlField + ": <this string>}`."},
			{Type: "object", Properties: objProps, Required: []string{urlField}, AdditionalProperties: jsonschema.FalseSchema},
		},
		Description: engine + " connection — bare URL string OR structured object.",
	}
}

// SnapshotsConfig — `snapshots:` block. Carries the retention /
// eviction policies for cached engine templates. Snapshot state
// lives entirely in SQLite + the engines themselves (template DBs);
// no on-disk cache directory needs configuring.
//
// `CapPerRepo` is the hard cap that triggers eviction on every
// `RecordSnapshot`. LRU rows above the cap are dropped immediately
// (in a background goroutine) so a busy worktree workflow never
// accumulates unbounded cached templates per repo.
//
// `KeepPerSource`, `MaxAgeDays`, `MaxTotalGb`, `GcIntervalMinutes`
// drive the periodic daemon-side sweep; they're not consulted by
// the inline-on-write eviction path.
type SnapshotsConfig struct {
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

// PortSpec — one entry in the top-level `ports:` block. Declares the
// inclusive `[min, max]` TCP port range treeman will allocate from
// when assigning this slot to a new worktree. Treeman picks the
// first free port in the range that is both (a) not bound by an
// active TCP listener on `127.0.0.1` and (b) not already recorded
// in the `worktree_ports` table for another live worktree.
//
// YAML shape:
//
//	ports:
//	  octane:
//	    range: [8000, 8999]
//	  webpack:
//	    range: [3000, 3999]
//
// or the shorthand form:
//
//	ports:
//	  reverb: [6001, 6999]
type PortSpec struct {
	// Range is the inclusive [min, max] port range. Min and max must
	// satisfy 1 ≤ min ≤ max ≤ 65535. Required.
	Range PortRange `yaml:"range"`
}

// PortRange is an inclusive `[min, max]` port range.
type PortRange struct {
	Min uint16
	Max uint16
}

// UnmarshalYAML accepts either the structured `{range: [min, max]}`
// form or the shorthand `[min, max]` two-element sequence.
func (p *PortSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		return p.Range.UnmarshalYAML(node)
	case yaml.MappingNode:
		type alias PortSpec
		return node.Decode((*alias)(p))
	default:
		return fmt.Errorf("port spec (line %d): want a mapping or [min, max] sequence", node.Line)
	}
}

// UnmarshalYAML accepts a `[min, max]` two-element sequence.
func (r *PortRange) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode || len(node.Content) != 2 {
		return fmt.Errorf("port range (line %d): want a [min, max] two-element sequence", node.Line)
	}
	var nums [2]uint16
	for i, child := range node.Content {
		if child.Kind != yaml.ScalarNode {
			return fmt.Errorf("port range (line %d): entries must be integers", child.Line)
		}
		n, err := strconv.ParseUint(child.Value, 10, 16)
		if err != nil {
			return fmt.Errorf("port range (line %d): %q: %w", child.Line, child.Value, err)
		}
		nums[i] = uint16(n)
	}
	r.Min, r.Max = nums[0], nums[1]
	return nil
}

// JSONSchema documents the shorthand-or-mapping shape.
func (PortSpec) JSONSchema() *jsonschema.Schema {
	rangeSchema := &jsonschema.Schema{
		Type:        "array",
		Items:       &jsonschema.Schema{Type: "integer", Minimum: json.Number("1"), Maximum: json.Number("65535")},
		MinItems:    intp(2),
		MaxItems:    intp(2),
		Description: "Inclusive [min, max] TCP port range.",
	}
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("range", rangeSchema)
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			rangeSchema,
			{
				Type:                 "object",
				Properties:           props,
				Required:             []string{"range"},
				AdditionalProperties: jsonschema.FalseSchema,
				Description:          "Structured port-spec mapping.",
			},
		},
		Description: "Per-worktree port slot: an inclusive [min, max] range. Shorthand `[min, max]` is accepted; the long form is `{range: [min, max]}`.",
	}
}

// intp is a small helper for jsonschema.Schema's *uint64 minimum /
// maximum fields. (json.Number can be used for unbounded ints; intp
// is used for MinItems / MaxItems which take *uint64.)
func intp(v uint64) *uint64 { return &v }

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

// Patch — one entry in the top-level `patches:` block. Each entry
// targets one file under the worktree root and rewrites it with
// per-worktree values via the `set:` map. Values are template
// strings that accept `{slug}`, `{slug_dash}`,
// `{slug_redis_queue}`, `{slug_redis_cache}`. Validated at
// config-load time — unknown keys fail loud.
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
// Each patched file is wired through git's clean/smudge filter so
// the rewrite is hidden from `git status` while still letting
// `git pull` / `git checkout` overwrite the file on incoming
// changes — the daemon's HEAD watcher (or the smudge filter itself)
// re-applies the patch after. The file must be tracked by git for
// the filter to engage; gitignored files are patched in-place
// without any git interaction.
type Patch struct {
	// File path relative to the worktree root. Required.
	File string `yaml:"file"`

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
//   - on-create-before-engines — during `wt create`, after patches +
//     bring-in (copies/links), BEFORE engine prepare. Standard
//     home of dependency installs (composer/yarn/pip) so migrate
//     can find vendor/.
//   - on-create-after-engines — during `wt create`, after engine
//     prepare. Use when actions need a populated database
//     (cache warming, seed verification).
//   - on-delete-before-engines — during `wt delete`, BEFORE DB
//     drop. Graceful shutdown: drain queues, docker compose stop.
//   - on-delete-after-engines — during `wt delete`, AFTER DB drop +
//     git worktree remove. External notifications (Slack, CDN
//     purge) that should announce only once the data is gone.
//   - on-checkout — fires when the HEAD watcher sees a branch
//     switch inside an existing worktree. Re-runs in addition to
//     the regular finalize-on-HEAD-change behaviour.
//   - on-file-change — fires when any `databases[].inputs[]` glob
//     matches a filesystem event. Each action can optionally
//     `match: <label>` to filter by the input entry's label.
//
// The map shape lets new triggers be added without touching every
// existing config. Daemon execution is always non-blocking from the
// CLI's perspective — each list of actions dispatches in parallel.
type HooksConfig struct {
	// OnCreateBeforeEngines — actions fire after worktree create +
	// patches + bring-in, before engine prepare.
	OnCreateBeforeEngines []Action `yaml:"on-create-before-engines,omitempty"`

	// OnCreateAfterEngines — actions fire after engine prepare completes.
	OnCreateAfterEngines []Action `yaml:"on-create-after-engines,omitempty"`

	// OnDeleteBeforeEngines — actions fire before DB drop on delete.
	OnDeleteBeforeEngines []Action `yaml:"on-delete-before-engines,omitempty"`

	// OnDeleteAfterEngines — actions fire after DB drop + worktree
	// remove on delete.
	OnDeleteAfterEngines []Action `yaml:"on-delete-after-engines,omitempty"`

	// OnCheckout — actions fire when the HEAD watcher detects a
	// branch switch inside an existing worktree.
	OnCheckout []Action `yaml:"on-checkout,omitempty"`

	// OnFileChange — actions fire when any `databases[].inputs[]`
	// glob matches a filesystem event. Each action can optionally
	// `match: <label>` to fire only when the matched input entry
	// carries that label; actions without `match:` fire for every
	// input event (any engine, any label).
	//
	// The subprocess receives extra env vars naming the trigger:
	//   TREEMAN_WATCH_PATH   — absolute path that fired
	//   TREEMAN_WATCH_MODE   — auto | delta | rebuild
	//   TREEMAN_WATCH_LABEL  — the label on the matched watch entry (or "")
	//   TREEMAN_WATCH_ENGINE — engine of the owning database (mysql, postgres, …)
	//   TREEMAN_WATCH_DB_NAME — rendered name_template of the owning database
	OnFileChange []FilteredAction `yaml:"on-file-change,omitempty"`
}

// Action — one entry under `hooks.{setup,teardown}.actions`. Every
// action is a mapping; there are no shorthand forms.
//
//   - `run` is the work, as either a single shell string (one
//     command) or a list of shell strings (sequenced steps chained
//     with `&&`).
//   - `cwd` is the group-level working directory; all steps in the
//     action share it. Use multiple actions if you need different
//     cwds.
//   - `container` / `compose_service` (mutually exclusive) wrap the
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
	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           props,
		Required:             []string{"run"},
		AdditionalProperties: jsonschema.FalseSchema,
		Description:          "One action: a `run` (string or list of strings) plus optional cwd + container wrapping.",
	}
}

// UnmarshalYAML decodes one action map. Accepts both `run: string`
// and `run: [string, ...]`. Rejects the legacy `background:`,
// `steps:`, `in_container:`, and `engine:` keys loudly so old
// configs surface a clear error rather than silently mis-parse.
func (a *Action) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf(
			"action (line %d): want a mapping; bare strings + list shorthands have been removed — wrap each action in `{ run: ... }`",
			node.Line,
		)
	}
	for i := 0; i < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "background":
			return fmt.Errorf("action: `background:` is no longer supported — hooks are always async (line %d)", node.Content[i].Line)
		case "steps":
			return fmt.Errorf("action: `steps:` is no longer supported — use `run:` with a list of strings (line %d)", node.Content[i].Line)
		case "in_container":
			return fmt.Errorf("action: `in_container:` alias removed — use `container:` (line %d)", node.Content[i].Line)
		case "engine":
			return fmt.Errorf("action: `engine:` alias removed — use `container_engine:` (line %d)", node.Content[i].Line)
		}
	}
	var raw struct {
		Run            yaml.Node `yaml:"run"`
		Cwd            string    `yaml:"cwd"`
		Container      string    `yaml:"container"`
		ComposeService string    `yaml:"compose_service"`
		ComposeProject string    `yaml:"compose_project"`
		Engine         string    `yaml:"container_engine"`
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
	a.Container = raw.Container
	a.ComposeService = raw.ComposeService
	a.ComposeProject = raw.ComposeProject
	a.Engine = raw.Engine
	if a.Container != "" && a.ComposeService != "" {
		return fmt.Errorf("action (line %d): set either `container:` or `compose_service:`, not both", node.Line)
	}
	return nil
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
	// `{slug}`, `{slug_dash}`, `{slug_redis_queue}`, `{slug_redis_cache}`
	// (see the `template` package for definitions). Example:
	// `app_{slug}` → `app_feature-x`. Required for engines that
	// scope by database name (MySQL, Postgres, Mongo). Validated at
	// config-load time — typos fail loud.
	NameTemplate string `yaml:"name_template,omitempty"`

	// Source dump used to seed clones. Path is relative to the repo
	// root. Treeman hashes this file into the snapshot key, so
	// changes invalidate the cache.
	Dump *DumpSpec `yaml:"dump,omitempty"`

	// Migrate is the shell command that brings a freshly-loaded
	// source DB up to the current schema. Required when any input
	// glob matches migration files; optional otherwise (e.g. a DB
	// that's purely seed-driven).
	Migrate *Step `yaml:"migrate,omitempty"`

	// Seed is the shell command that populates non-migration data
	// (fixtures, ES mappings, Redis warm-cache keys, etc.). Runs
	// AFTER dump-load + migrate as the final cold-build step,
	// before treeman snapshots the populated state into the
	// template DB.
	Seed *Step `yaml:"seed,omitempty"`

	// Inputs declare every file that determines this database's
	// template state. Each entry:
	//   1. Contributes a hash to the snapshot fingerprint (so any
	//      change auto-invalidates the cached template).
	//   2. Subscribes fsnotify so changes trigger a re-prep.
	//   3. Carries an optional `label:` that `hooks.on-file-change`
	//      actions can match against.
	//
	// Glob patterns are repo-root-relative. Hash mode is per-entry:
	// `filename` for append-only files (Laravel migrations, …),
	// `checksum` for files edited in place (seeders, lockfiles).
	// Default is checksum.
	//
	// Cache-hit vs cold-build is derived purely from the input
	// hashes — there's no separate `on: rebuild` knob. If you want
	// to force a rebuild, change an input.
	Inputs []Input `yaml:"inputs,omitempty"`

	// Test-clone fanout: how many parallel database clones to
	// pre-warm for paratest/pytest-xdist/Jest workers/etc.
	TestClones *TestClonesSpec `yaml:"test_clones,omitempty"`

	// KeyPrefix scopes every key/index a worktree creates under a
	// per-worktree prefix. Used by engines that don't scope by
	// database name — Redis (key prefix in DB 0) and Elasticsearch
	// / OpenSearch (index-name prefix). Example: `{slug}:` →
	// `feature-x:`. The app must honour the prefix — Laravel's
	// `CACHE_PREFIX`, Rails `Rails.cache.options[:namespace]`,
	// ioredis `keyPrefix`, etc.
	//
	// Supports the same placeholders as `name_template`: `{slug}`,
	// `{slug_dash}`, `{slug_redis_queue}`, `{slug_redis_cache}`.
	// Validated at config-load time.
	KeyPrefix string `yaml:"key_prefix,omitempty"`

	// Outer concurrency cap for clone restore + DropMatching.
	// Defaults to the per-engine safe value from internal/prepare.
	// Range 0–64. Raise only if the server is provisioned
	// (max_connections, PG pg_database lock contention, etc.).
	Fanout uint32 `yaml:"fanout,omitempty" jsonschema:"minimum=0,maximum=64"`

	// BranchScoped turns this database into a git-for-databases
	// working copy: the app always talks to one stable ACTIVE
	// namespace, while treeman keeps a DURABLE per-branch copy of its
	// contents and swaps them in/out as the branch changes. One flag,
	// the whole lifecycle — no per-feature sub-fields, no
	// user-configured durable names (treeman derives them internally).
	//
	// The active namespace is fixed per checkout, so the app's
	// connection string (`.env`) is patched once and never churns:
	//   - main worktree → the `main_worktree.databases[].name_template`
	//     overlay (typically a bare, unprefixed name the repo-root app
	//     already points at, e.g. `kontainer`).
	//   - linked worktree → `name_template` (or `key_prefix` for
	//     prefix-scoped engines) rendered against the worktree's
	//     branch-independent slug, so switching branches inside the
	//     worktree doesn't rename its DB.
	//
	// Lifecycle, driven by HEAD changes + create/delete:
	//   - create / first switch onto a branch → seed the active
	//     namespace from the branch's own durable copy (resume) or,
	//     failing that, from its parent branch's data (tracked
	//     upstream, via the main overlay or a sibling worktree), or
	//     `dump.path`, or empty.
	//   - switch off a branch → capture the active namespace into that
	//     branch's durable copy first (manual data changes live on).
	//   - switch back → restore that durable copy. `treeman db reset`
	//     drops the durable copy + re-seeds from the live parent.
	//
	// Engine-agnostic: name-scoped engines (MySQL, Postgres, MongoDB)
	// swap whole databases; prefix-scoped engines (Redis,
	// Elasticsearch/OpenSearch) swap the key/index namespace under the
	// rendered `key_prefix`. All five participate.
	//
	// Mutually exclusive with `test_clones` / `fanout`: a branch_scoped
	// database is a stateful per-branch snapshot the app mutates in
	// place, not a reproducible source for throwaway parallel-test
	// clones. Config-load rejects the combination.
	//
	// Postgres caveats (both stem from `CREATE DATABASE … TEMPLATE`,
	// which needs exclusive access to its source):
	//   - Capturing a branch on a switch briefly force-disconnects every
	//     session on the active database (the swap fences it and
	//     terminates backends), so the app momentarily loses its DB
	//     connections and must reconnect. Expected; not data loss.
	//   - Seeding a branch from its parent branch's LIVE database fails
	//     while other sessions are connected to the parent (treeman does
	//     NOT force-terminate another worktree's connections). Close
	//     those connections, or set `dump.path` to seed from the dump.
	// MySQL/MongoDB copy logically and have neither constraint.
	BranchScoped bool `yaml:"branch_scoped,omitempty"`
}

// Input declares one source of file state that contributes to the
// template fingerprint AND triggers a re-prep when it changes.
// Replaces the older split between `migrations.migration_dirs`,
// `migrations.file_globs`, `migrations.lockfiles`, and
// `databases[].watch[]`. Unifying makes the cache-key derivation
// transparent: every input is hashed; nothing else is. Watches
// always trigger best-effort re-prep — there's no separate
// `on: rebuild` override.
type Input struct {
	// Glob pattern (relative to repo root). Supports `**` recursion.
	// Required.
	Glob string `yaml:"glob"`

	// Optional label that `hooks.on-file-change` actions can match
	// against via their `match:` field. Multiple entries can share a
	// label so one action handles a logical group of file types.
	Label string `yaml:"label,omitempty"`

	// Hash mode for files matching this glob:
	//   `checksum` (default) — full content hash. Detects edits.
	//     Right for lockfiles, seeders, factories, fixtures.
	//   `filename`            — hash of the filename only. Cheaper.
	//     Right for append-only directories (Laravel/Rails/Django
	//     migrations where existing files never change).
	Hash string `yaml:"hash,omitempty" jsonschema:"enum=checksum,enum=filename"`
}

// JSONSchema documents the polymorphic shape: an Input is either a
// bare glob string (shorthand for `{glob: <string>}`) or a full
// `{glob, label, hash}` mapping.
func (Input) JSONSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("glob", &jsonschema.Schema{Type: "string", Description: "Glob pattern (repo-root-relative). Required."})
	props.Set("label", &jsonschema.Schema{Type: "string", Description: "Optional label for `hooks.on-file-change` matchers."})
	props.Set(
		"hash",
		&jsonschema.Schema{
			Type:        "string",
			Enum:        []any{"checksum", "filename"},
			Description: "Hash mode: checksum (default) or filename (append-only files).",
		},
	)
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "Bare glob string. Equivalent to `{glob: <this string>}` with default hash mode."},
			{
				Type:                 "object",
				Properties:           props,
				Required:             []string{"glob"},
				AdditionalProperties: jsonschema.FalseSchema,
				Description:          "Full input mapping with optional label + hash mode.",
			},
		},
		Description: "One source of file state for the template fingerprint. Bare string OR full mapping.",
	}
}

// UnmarshalYAML accepts either a bare glob string or a mapping.
func (i *Input) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		i.Glob = node.Value
		return nil
	case yaml.MappingNode:
		type alias Input
		return node.Decode((*alias)(i))
	default:
		return fmt.Errorf("input (line %d): want a string or mapping", node.Line)
	}
}

// FilteredAction is an Action with an optional `match:` that
// filters which watch labels can trigger it. Used by
// `hooks.on-file-change`.
type FilteredAction struct {
	// Match restricts the action to a set of watch labels. Accepts
	// either a single string (`match: migrations`) or a list of
	// strings (`match: [migrations, seeders]`). Empty/missing means
	// the action fires for ANY watch event (any engine, any label).
	Match []string `yaml:"match,omitempty"`

	// Embedded Action: same shape as the universal hooks Action
	// (run, cwd, container, …).
	Action `yaml:",inline"`
}

// JSONSchema documents the string|[]string polymorphism for `match:`.
func (FilteredAction) JSONSchema() *jsonschema.Schema {
	// Pull in Action's schema for the base shape, then layer `match:`
	// on top so editor hints describe the whole filtered action.
	base := Action{}.JSONSchema()
	if base.Properties != nil {
		base.Properties.Set("match", &jsonschema.Schema{
			OneOf: []*jsonschema.Schema{
				{Type: "string", Description: "Single watch label to match."},
				{
					Type:        "array",
					Items:       &jsonschema.Schema{Type: "string"},
					Description: "Set of watch labels; the action fires when any of them matches.",
				},
			},
			Description: "Restrict this action to watch events carrying one of the named labels. Omit to fire for every event.",
		})
	}
	base.Description = "On-file-change action with optional label filter. Same shape as a hook Action plus a `match:` field (string or list)."
	return base
}

// UnmarshalYAML peels off the `match:` field (accepting either a
// scalar or a sequence) and delegates the remaining keys to
// Action's UnmarshalYAML.
func (f *FilteredAction) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("on-file-change action (line %d): want a mapping", node.Line)
	}
	// Pull the `match:` value out separately so we can accept both
	// scalar + sequence forms. Other keys are decoded by Action.
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "match" {
			continue
		}
		v := node.Content[i+1]
		switch v.Kind {
		case yaml.ScalarNode:
			if v.Value != "" {
				f.Match = []string{v.Value}
			}
		case yaml.SequenceNode:
			f.Match = make([]string, 0, len(v.Content))
			for _, child := range v.Content {
				if child.Kind != yaml.ScalarNode {
					return fmt.Errorf("on-file-change (line %d): every `match` entry must be a string label", child.Line)
				}
				f.Match = append(f.Match, child.Value)
			}
		default:
			return fmt.Errorf("on-file-change (line %d): `match` must be a string or list of strings", v.Line)
		}
		break
	}
	return f.Action.UnmarshalYAML(node)
}

// Matches reports whether this action's label filter accepts an
// event carrying `label`. An empty Match list (wildcard) matches
// everything; otherwise the label must equal one of the listed
// strings.
func (f FilteredAction) Matches(label string) bool {
	if len(f.Match) == 0 {
		return true
	}
	return slices.Contains(f.Match, label)
}

// Step is one user-declared shell command executed against a target
// database. Used by both `databases[].migrate:` and
// `databases[].seed:` — the same shape, different lifecycle slots.
//
// The framework's CLI reads its target DB from its own config — for
// most stacks that's a connection string the user already keeps in
// `.env` (or wires up in `config/database.yml`, `data-source.ts`,
// etc.). Treeman builds per-worktree template databases with names
// like `myapp_template_feature-x` and needs to redirect the command
// at *that* DB, not the one the committed `.env` references. The
// `Env` map says which env-var names to override; values are
// rendered through the same `template` pass that produces the
// per-run DB name. The scaffold convention is to set `DB_NAME`
// (Laravel's preset uses the framework-native `DB_DATABASE`); the
// user weaves `${DB_NAME}` into the relevant slot of their Run
// command (typically inside a DSN) or wires their config to read
// it. Supported substitutions:
//
//	{target_db}         — resolved per-run database name / key prefix
//	{slug}              — the slug value
//	{slug_dash}         — slug with underscores → hyphens
//	{slug_redis_queue}  — slug-derived Redis queue DB index (6..15)
//	{slug_redis_cache}  — slug-derived Redis cache DB index (6..15)
//
// Unknown keys fail loud at config-load time. Treeman also exports
// `TREEMAN_TARGET_DB` to the subprocess unconditionally as a safety
// net for tooling that wants the resolved name without a custom env
// mapping.
type Step struct {
	// Run is the shell command treeman invokes via `sh -c`.
	// Required; an empty Run aborts `prepare` with a clear error.
	Run string `yaml:"run"`

	// Env is a map of env-var names to value templates. Each entry
	// is set on the subprocess (overriding the framework's config
	// file). See the type doc-comment for the full list of supported
	// `{placeholder}` keys; literal values pass through unchanged.
	Env map[string]string `yaml:"env,omitempty"`
}

// DumpSpec — `dump:` sub-block of a DatabaseConfig. Accepts either
// a bare string (`dump: storage/dumps/seed.sql.gz`) or a full
// mapping (`dump: { path: ..., optional: true }`).
//
// Supported engines + formats:
//
//   - MySQL/MariaDB/TiDB: plain `.sql` text dumps (mysqldump output)
//   - Postgres:            plain `.sql` text dumps (pg_dump --format=plain)
//   - MongoDB:             `mongodump --archive` archives (binary)
//   - Elasticsearch/OS:    `_bulk`-format NDJSON
//
// Compression (gzip / zstd / bzip2 / xz) is auto-detected from the
// file's magic bytes — extension is not consulted. Same single
// `dump:` field works for `seed.sql`, `seed.sql.gz`, `seed.sql.zst`,
// `dump.archive.gz`, etc.
type DumpSpec struct {
	// Path to the dump file relative to the repo root. The file is
	// hashed into the snapshot key so changes invalidate cached
	// snapshots.
	Path string `yaml:"path"`

	// When true, a missing dump file is not an error — treeman will
	// build the template from migrations / seed alone. Use for
	// greenfield projects that haven't created a baseline dump yet.
	Optional bool `yaml:"optional,omitempty"`

	// SourceDB names the database the archive was originally dumped
	// from (e.g. `production` if you ran `mongodump --db=production`).
	// MongoDB only — treeman remaps that DB's collections into the
	// per-worktree target DB via mongorestore's --nsFrom/--nsTo.
	// Leave empty to skip the rename (the archive must already use
	// the target DB name). Ignored by MySQL/Postgres/ES drivers.
	SourceDB string `yaml:"source_db,omitempty"`
}

// JSONSchema documents the bare-string-or-mapping shape.
func (DumpSpec) JSONSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("path", &jsonschema.Schema{Type: "string", Description: "Dump file path, repo-root-relative. Required."})
	props.Set("optional", &jsonschema.Schema{Type: "boolean", Description: "When true, missing dump is not an error."})
	props.Set(
		"source_db",
		&jsonschema.Schema{
			Type:        "string",
			Description: "MongoDB only: the database the archive was dumped from; remapped into the per-worktree target DB via mongorestore --nsFrom/--nsTo. Ignored by other engines.",
		},
	)
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "Bare path string. Equivalent to `{path: <this string>}`."},
			{
				Type:                 "object",
				Properties:           props,
				Required:             []string{"path"},
				AdditionalProperties: jsonschema.FalseSchema,
				Description:          "Full dump mapping with optional `optional` + `source_db`.",
			},
		},
		Description: "Source dump file. Bare string OR `{path, optional, source_db}` mapping.",
	}
}

// UnmarshalYAML accepts either a bare path string or a mapping.
func (d *DumpSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		d.Path = node.Value
		return nil
	case yaml.MappingNode:
		type alias DumpSpec
		return node.Decode((*alias)(d))
	default:
		return fmt.Errorf("dump (line %d): want a string or mapping", node.Line)
	}
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

	// Template for clone database names. Supports the same
	// placeholders as `databases[].name_template` plus `{n}` —
	// the 0-based clone index (only valid here). Required.
	// Example: `app_{slug}_test_{n}`.
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
	return errors.New("clones: want scalar")
}

// WatcherPath is the internal projection of an Input that the
// fsnotify driver subscribes to. Users never write this type
// directly — they declare `databases[].inputs[]` and treeman
// aggregates them into WatcherPaths at watcher-start time.
type WatcherPath struct {
	// Filesystem glob (relative to repo root). Supports `**` recursion.
	Glob string `yaml:"-" json:"-"`
	// Label passes through from the originating Input so hook
	// matchers can filter on it.
	Label string `yaml:"-" json:"-"`
	// DBIndex is the index of the originating database in cfg.Databases.
	DBIndex int `yaml:"-" json:"-"`
}

// CustomFramework — `frameworks:` entry, lets users declare
// migration frameworks treeman doesn't know about natively. Consumed
// only by `treeman fw detect` and `treeman init` for scaffolding; at
// runtime treeman reads `databases[].inputs[]` directly.
type CustomFramework struct {
	// Files (relative to repo root) whose presence indicates this
	// framework is in use. Used by `treeman fw detect` to pick the
	// framework when scaffolding a new config.
	// Example: `["alembic.ini", "migrations/env.py"]`.
	Markers []string `yaml:"markers"`

	// Glob patterns for the directories holding migration files.
	// Emitted as `inputs[]` entries during `treeman init`.
	MigrationDirs []string `yaml:"migration_dirs"`

	// Glob pattern for individual migration files within
	// `migration_dirs`. Example: `[0-9]*_*.py` (alembic) or
	// `V*__*.sql` (flyway).
	FilePattern string `yaml:"file_pattern"`

	// Hash strategy applied to the migration files: `filename`
	// (default) or `checksum`. Maps to the `hash:` field on each
	// emitted Input.
	HashMode string `yaml:"hash_mode,omitempty" jsonschema:"enum=filename,enum=checksum"`

	// Lockfiles whose contents are folded into the snapshot hash
	// (e.g. `requirements.txt`, `pyproject.toml`, `composer.lock`).
	// Emitted as `inputs[]` entries with label `lockfile`.
	Lockfiles []string `yaml:"lockfiles,omitempty"`

	// Optional hint about the database engine this framework
	// targets — `mysql`, `postgres`, etc. Pre-fills the engine field
	// in `treeman init` when this framework is detected.
	EngineHint string `yaml:"engine_hint,omitempty"`
}

// LoadGlobal returns the user-global config alone (no repo or
// repo-local overlay). Used by the daemon at startup to read fields
// that need to be live before any repo is known — currently just
// daemon.log_level. Missing global file → defaults-only Config.
func LoadGlobal() (Config, error) {
	var cfg Config
	applyDefaults(&cfg)
	if g, ok := globalConfigPath(); ok {
		if err := mergeYAMLFile(&cfg, g); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
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
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("config invalid: %w", err)
	}
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
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("config invalid: %w", err)
	}
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

// applyDefaults fills in the canonical defaults: async_create true,
// retention defaults.
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
	if cfg.Snapshots.CapPerRepo == 0 {
		cfg.Snapshots.CapPerRepo = 8
	}
	if cfg.Snapshots.KeepPerSource == 0 {
		cfg.Snapshots.KeepPerSource = 500
	}
	if cfg.Snapshots.MaxAgeDays == 0 {
		cfg.Snapshots.MaxAgeDays = 30
	}
	if cfg.Snapshots.MaxTotalGb == 0 {
		cfg.Snapshots.MaxTotalGb = 50
	}
	if cfg.Snapshots.GcIntervalMinutes == 0 {
		cfg.Snapshots.GcIntervalMinutes = 60
	}
	if cfg.DebounceMs == 0 {
		cfg.DebounceMs = 500
	}
	if cfg.AutoFetch.IntervalMinutes == 0 {
		cfg.AutoFetch.IntervalMinutes = 15
	}
	if cfg.Logs.KeepDays == 0 {
		// 14d covers a working sprint of postmortem context without
		// letting hook log BLOBs bloat the SQLite file unboundedly.
		// Set explicitly to a negative value to mean "never prune"
		// (handled by callers as <= 0).
		cfg.Logs.KeepDays = 14
	}
	applyStatusDefaults(cfg)
}

// applyStatusDefaults fills the `status:` block defaults — separator,
// templates, per-bucket labels, and Nerd Font icon glyphs. Split out
// of applyDefaults to keep that function's branch count under the
// cyclomatic-complexity gate.
func applyStatusDefaults(cfg *Config) {
	if cfg.Status.Separator == "" {
		cfg.Status.Separator = " | "
	}
	if cfg.Status.Header == "" {
		cfg.Status.Header = "{repo}  ({total})"
	}
	if cfg.Status.Row == "" {
		cfg.Status.Row = "  {main}{branch}{state_suffix}"
	}
	if cfg.Status.MainMarker == "" {
		cfg.Status.MainMarker = "★ "
	}
	if cfg.Status.Labels.Stable == "" {
		cfg.Status.Labels.Stable = "stable"
	}
	if cfg.Status.Labels.Up == "" {
		cfg.Status.Labels.Up = "up"
	}
	if cfg.Status.Labels.Down == "" {
		cfg.Status.Labels.Down = "down"
	}
	if cfg.Status.Labels.Failed == "" {
		cfg.Status.Labels.Failed = "failed"
	}
	// Nerd Font glyphs per bucket (Material Design, U+F14Cx). Written
	// as \U escapes so they survive source round-trips. Set an icon to
	// a single space in YAML to suppress it (empty re-triggers these).
	if cfg.Status.Icons.Stable == "" {
		cfg.Status.Icons.Stable = "\U000f14cf" // md-circle (stable)
	}
	if cfg.Status.Icons.Up == "" {
		cfg.Status.Icons.Up = "\U000f14ca" // md-arrow-up (preparing)
	}
	if cfg.Status.Icons.Down == "" {
		cfg.Status.Icons.Down = "\U000f14cb" // md-arrow-down (teardown)
	}
	if cfg.Status.Icons.Failed == "" {
		cfg.Status.Icons.Failed = "\U000f14cc" // md-alert (failed)
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
