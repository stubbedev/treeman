// Package config loads `.treeman.yaml` plus the layered global +
// per-repo + per-worktree-local overrides. Ported from
// `crates/treeman-core/src/config.rs`.
//
// Schema is the canonical Rust shape; YAML round-trip parity is a
// requirement, so any new field added on either side needs the
// other side to follow.
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
type MysqlConn struct {
	Host        string  `yaml:"host"`
	Port        uint16  `yaml:"port"`
	User        string  `yaml:"user"`
	PasswordEnv *string `yaml:"password_env,omitempty"`
	Password    string  `yaml:"-"`
	PoolMax     uint32  `yaml:"pool_max,omitempty"`
}

// PostgresConn — same shape as MysqlConn.
type PostgresConn struct {
	Host        string  `yaml:"host"`
	Port        uint16  `yaml:"port"`
	User        string  `yaml:"user"`
	PasswordEnv *string `yaml:"password_env,omitempty"`
	Password    string  `yaml:"-"`
	PoolMax     uint32  `yaml:"pool_max,omitempty"`
}

// MongoConn — `mongodb://…` URI.
type MongoConn struct {
	URI string `yaml:"uri"`
}

// RedisConn — `redis://…` URL.
type RedisConn struct {
	URL string `yaml:"url"`
}

// EsConn — Elasticsearch / OpenSearch HTTP URL.
type EsConn struct {
	URL string `yaml:"url"`
}

// SnapshotsConfig — `snapshots:` block.
type SnapshotsConfig struct {
	CacheDir  string          `yaml:"cache_dir,omitempty"`
	Retention RetentionConfig `yaml:"retention,omitempty"`
}

// RetentionConfig — `snapshots.retention:` policies.
type RetentionConfig struct {
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
type EnvScoping struct {
	Files        []string   `yaml:"files,omitempty"`
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
type DatabaseConfig struct {
	Engine       string         `yaml:"engine"`
	NameTemplate string         `yaml:"name_template,omitempty"`
	Dump         *DumpSpec      `yaml:"dump,omitempty"`
	Migrations   *MigrationSpec `yaml:"migrations,omitempty"`
	Paratest     *ParatestSpec  `yaml:"paratest,omitempty"`
	Namespaces   *Namespaces    `yaml:"namespaces,omitempty"`
}

// DumpSpec — `dump:` sub-block of a DatabaseConfig.
type DumpSpec struct {
	Path     string `yaml:"path"`
	Optional bool   `yaml:"optional,omitempty"`
}

// MigrationSpec — `migrations:` sub-block.
type MigrationSpec struct {
	Framework string `yaml:"framework"`
	Dir       string `yaml:"dir,omitempty"`
}

// ParatestSpec — `paratest:` sub-block.
type ParatestSpec struct {
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
}

// WatcherPath — one `paths:` entry.
type WatcherPath struct {
	Glob string `yaml:"glob"`
	On   string `yaml:"on,omitempty"` // "auto" | "delta" | "rebuild"
}

// CustomFramework — `frameworks:` entry, lets users declare
// migration frameworks treeman doesn't know about natively.
type CustomFramework struct {
	Markers       []string `yaml:"markers"`
	MigrationDirs []string `yaml:"migration_dirs"`
	FilePattern   string   `yaml:"file_pattern"`
	HashMode      string   `yaml:"hash_mode,omitempty"`
	OnModify      string   `yaml:"on_modify,omitempty"`
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
	return cfg, nil
}

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

// applyDefaults sets the same defaults the Rust schemars-derived
// defaults produced: async_create + async_delete true,
// skip_worktree true, retention defaults.
func applyDefaults(cfg *Config) {
	if cfg.Daemon.LogLevel == "" {
		cfg.Daemon.LogLevel = "info"
	}
	if cfg.Worktrees.Root == "" {
		cfg.Worktrees.Root = "../worktrees"
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
