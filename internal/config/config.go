// Package config loads `.treeman.yaml` plus the layered global +
// per-repo + per-worktree-local overrides.
//
// YAML round-trip parity is a requirement: loading and re-emitting
// a config must not silently drop unknown fields, so adding a new
// field requires touching both the struct and the test fixtures.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the top-level structure of a `.treeman.yaml` plus the
// global `~/.config/treeman/config.yaml`.
type Config struct {
	Daemon      DaemonConfig               `yaml:"daemon,omitempty"`
	Connections ConnectionsConfig          `yaml:"connections,omitempty"`
	Snapshots   SnapshotsConfig            `yaml:"snapshots,omitempty"`
	Repo        *RepoBlock                 `yaml:"repo,omitempty"`
	Slug        SlugRules                  `yaml:"slug,omitempty"`
	Worktrees   WorktreesConfig            `yaml:"worktrees,omitempty"`
	EnvScoping  EnvScoping                 `yaml:"env_scoping,omitempty"`
	Databases   []DatabaseConfig           `yaml:"databases,omitempty"`
	Hooks       HooksConfig                `yaml:"hooks,omitempty"`
	Watcher     WatcherConfig              `yaml:"watcher,omitempty"`
	Frameworks  map[string]CustomFramework `yaml:"frameworks,omitempty"`
}

// DaemonConfig — `daemon:` block.
type DaemonConfig struct {
	Socket    string `yaml:"socket,omitempty"`
	LogLevel  string `yaml:"log_level,omitempty"`
	DbLogPath string `yaml:"db_log_path,omitempty"`
}

// ConnectionsConfig — `connections:` block.
type ConnectionsConfig struct {
	Mysql         *MysqlConn    `yaml:"mysql,omitempty"`
	Postgres      *PostgresConn `yaml:"postgres,omitempty"`
	Mongodb       *MongoConn    `yaml:"mongodb,omitempty"`
	Redis         *RedisConn    `yaml:"redis,omitempty"`
	Elasticsearch *EsConn       `yaml:"elasticsearch,omitempty"`
}

// MysqlConn — host/port/user. `Password` is runtime-only; never
// serialised. The resolver fills it from the repo's `.env*` files
// + process env.
//
// `Container` (optional): when set, treeman runs `docker inspect`
// (or `podman inspect`, configurable via `container_engine`) on
// the container and uses its bridge-network IP in place of `Host`
// before dialing. Lets you connect to a DB running in docker that
// has no published port (so `host:port` from the host fails) as
// long as treeman runs on the same machine and can route to the
// docker bridge network (the default for `bridge` driver on Linux;
// requires `host.docker.internal` workaround on macOS).
type MysqlConn struct {
	Host            string  `yaml:"host,omitempty"`
	Port            uint16  `yaml:"port,omitempty"`
	User            string  `yaml:"user"`
	PasswordEnv     *string `yaml:"password_env,omitempty"`
	Password        string  `yaml:"-"`
	PoolMax         uint32  `yaml:"pool_max,omitempty"`
	Container       string  `yaml:"container,omitempty"`
	ContainerEngine string  `yaml:"container_engine,omitempty" jsonschema:"enum=docker,enum=podman"` // "docker"|"podman"; default "docker"
}

// PostgresConn — same shape as MysqlConn.
type PostgresConn struct {
	Host            string  `yaml:"host,omitempty"`
	Port            uint16  `yaml:"port,omitempty"`
	User            string  `yaml:"user"`
	PasswordEnv     *string `yaml:"password_env,omitempty"`
	Password        string  `yaml:"-"`
	PoolMax         uint32  `yaml:"pool_max,omitempty"`
	Container       string  `yaml:"container,omitempty"`
	ContainerEngine string  `yaml:"container_engine,omitempty" jsonschema:"enum=docker,enum=podman"`
}

// MongoConn — `mongodb://…` URI. When `Container` is set the URI's
// host is rewritten to the container IP at dial time.
type MongoConn struct {
	URI             string `yaml:"uri"`
	Container       string `yaml:"container,omitempty"`
	ContainerEngine string `yaml:"container_engine,omitempty" jsonschema:"enum=docker,enum=podman"`
}

// RedisConn — `redis://…` URL. Same Container semantics as MongoConn.
type RedisConn struct {
	URL             string `yaml:"url"`
	Container       string `yaml:"container,omitempty"`
	ContainerEngine string `yaml:"container_engine,omitempty" jsonschema:"enum=docker,enum=podman"`
}

// EsConn — Elasticsearch / OpenSearch HTTP URL. Same Container
// semantics as MongoConn.
type EsConn struct {
	URL             string `yaml:"url"`
	Container       string `yaml:"container,omitempty"`
	ContainerEngine string `yaml:"container_engine,omitempty" jsonschema:"enum=docker,enum=podman"`
}

// SnapshotsConfig — `snapshots:` block.
type SnapshotsConfig struct {
	CacheDir  string          `yaml:"cache_dir,omitempty"`
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
	CapPerRepo        uint32 `yaml:"cap_per_repo,omitempty"`
	KeepPerSource     uint32 `yaml:"keep_per_source,omitempty"`
	MaxAgeDays        uint32 `yaml:"max_age_days,omitempty"`
	MaxTotalGb        uint32 `yaml:"max_total_gb,omitempty"`
	GcIntervalMinutes uint32 `yaml:"gc_interval_minutes,omitempty"`
}

// RepoBlock — `repo:` block.
type RepoBlock struct {
	Name string `yaml:"name"`
}

// SlugRules — placeholder for future per-repo slug overrides.
type SlugRules struct {
	Override string `yaml:"override,omitempty"`
}

// WorktreesConfig — `worktrees:` block.
type WorktreesConfig struct {
	Root        string   `yaml:"root,omitempty"`
	Links       []string `yaml:"links,omitempty"`
	AsyncCreate *bool    `yaml:"async_create,omitempty"`
	AsyncDelete *bool    `yaml:"async_delete,omitempty"`
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
	Files        []string   `yaml:"files,omitempty"`
	Sources      []string   `yaml:"sources,omitempty"`
	SkipWorktree *bool      `yaml:"skip_worktree,omitempty"`
	Patches      []EnvPatch `yaml:"patches,omitempty"`
}

// EnvPatch — one `(key, template)` pair.
type EnvPatch struct {
	Key      string `yaml:"key"`
	Template string `yaml:"template"`
}

// HooksConfig — `hooks:` block.
type HooksConfig struct {
	// `precreate` is the only sync phase. List of single steps.
	Precreate []SingleStep `yaml:"precreate,omitempty"`

	// The async phases. Each `HookEntry` is one group (sequence
	// within, parallel across).
	Postcreate []HookEntry `yaml:"postcreate,omitempty"`
	Predelete  []HookEntry `yaml:"predelete,omitempty"`
	Postdelete []HookEntry `yaml:"postdelete,omitempty"`
}

// SingleStep — `{ run: "...", cwd: "..." }`. The bare-string YAML
// shorthand is decoded into one of these too.
type SingleStep struct {
	Run string `yaml:"run"`
	Cwd string `yaml:"cwd,omitempty"`
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
//   - mapping              → group of one with cwd
//   - sequence of children → group sequence (chained with `&&`)
type HookEntry struct {
	Steps []SingleStep
}

// UnmarshalYAML decides which of the three shapes applies based on
// the node kind. Strict: anything else is a typo we want to surface
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
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == "background" {
				return fmt.Errorf("hook entry: `background` is no longer supported — hooks are always async; group commands by nesting them in a list to express sequencing (line %d)", node.Content[i].Line)
			}
		}
		var s SingleStep
		if err := node.Decode(&s); err != nil {
			return err
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

// DatabaseConfig — one `databases:` entry. The `engine` discriminator
// gates which sub-fields are valid.
//
// `Fanout` is the optional override for the outer concurrency cap
// during clone restore + DropMatching. Leave unset (omitempty / 0)
// to use the safe per-engine default from internal/prepare. Raise
// only if the server is provisioned for it (max_connections raised,
// PG pg_database lock contention acceptable, etc.).
type DatabaseConfig struct {
	Engine       string          `yaml:"engine" jsonschema:"enum=mysql,enum=mariadb,enum=tidb,enum=postgres,enum=postgresql,enum=mongodb,enum=redis,enum=elasticsearch,enum=opensearch"`
	NameTemplate string          `yaml:"name_template,omitempty"`
	Dump         *DumpSpec       `yaml:"dump,omitempty"`
	Migrations   *MigrationSpec  `yaml:"migrations,omitempty"`
	TestClones   *TestClonesSpec `yaml:"test_clones,omitempty"`
	Namespaces   *Namespaces     `yaml:"namespaces,omitempty"`
	Fanout       uint32          `yaml:"fanout,omitempty" jsonschema:"minimum=0,maximum=64"`
}

// DumpSpec — `dump:` sub-block of a DatabaseConfig.
type DumpSpec struct {
	Path     string `yaml:"path"`
	Optional bool   `yaml:"optional,omitempty"`
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
	Framework     string   `yaml:"framework"`                                                    // free-form label for logs + downstream tooling
	MigrationDirs []string `yaml:"migration_dirs,omitempty"`                                     // glob patterns relative to repo root
	FileGlobs     []string `yaml:"file_globs,omitempty"`                                         // glob patterns for migration files within those dirs
	Lockfiles     []string `yaml:"lockfiles,omitempty"`                                          // files whose hash invalidates the snapshot
	HashMode      string   `yaml:"hash_mode,omitempty" jsonschema:"enum=filename,enum=checksum"` // "filename" (default) | "checksum"
	OnModify      string   `yaml:"on_modify,omitempty" jsonschema:"enum=rebuild,enum=delta"`     // "rebuild" (default) | "delta"
}

// TestClonesSpec — `test_clones:` sub-block. Used by every parallel
// test runner (paratest, pest, pytest-xdist, Jest workers, Go
// `-parallel`, cargo nextest, …). `clones` is either `auto` (treeman
// reads the project's worker-count config) or an explicit integer.
type TestClonesSpec struct {
	Clones       ClonesSetting `yaml:"clones,omitempty"`
	NameTemplate string        `yaml:"name_template"`
}

// ClonesSetting — `clones: auto | <integer>`.
type ClonesSetting struct {
	Auto  bool
	Fixed uint32
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
	DbIndexTemplate     string `yaml:"db_index_template,omitempty"`
	IndexPrefixTemplate string `yaml:"index_prefix_template,omitempty"`
	PrefixTemplate      string `yaml:"prefix_template,omitempty"`
}

// WatcherConfig — `watcher:` block.
type WatcherConfig struct {
	Paths      []WatcherPath `yaml:"paths,omitempty"`
	DebounceMs uint64        `yaml:"debounce_ms,omitempty"`
	Binlog     BinlogConfig  `yaml:"binlog,omitempty"`
}

// BinlogConfig — `watcher.binlog:` block. Controls the MySQL
// binary-log tailer that replays DDL + DML events from the source
// database onto cached template + paratest clone databases. Off by
// default; enabling requires a server configured with
// `binlog_format=ROW` and a replication-privileged user.
type BinlogConfig struct {
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
	Glob string `yaml:"glob"`
	On   string `yaml:"on,omitempty" jsonschema:"enum=auto,enum=delta,enum=rebuild"` // "auto" | "delta" | "rebuild"
}

// CustomFramework — `frameworks:` entry, lets users declare
// migration frameworks treeman doesn't know about natively.
type CustomFramework struct {
	Markers       []string `yaml:"markers"`
	MigrationDirs []string `yaml:"migration_dirs"`
	FilePattern   string   `yaml:"file_pattern"`
	HashMode      string   `yaml:"hash_mode,omitempty" jsonschema:"enum=filename,enum=checksum"`
	OnModify      string   `yaml:"on_modify,omitempty" jsonschema:"enum=rebuild,enum=delta"`
	Lockfiles     []string `yaml:"lockfiles,omitempty"`
	EngineHint    string   `yaml:"engine_hint,omitempty"`
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
