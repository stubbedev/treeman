package config

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/template"
)

// Validate runs cross-field consistency checks that can't be
// expressed in YAML tags or per-type UnmarshalYAML hooks. Caller is
// expected to invoke this after LoadLayered / LoadLayeredForWorktree
// so a broken config surfaces at load time rather than at the first
// driver-connect / DB-name-render that trips over it.
//
// All findings are gathered into a single error via errors.Join so
// users see every problem in one pass instead of fixing them one at
// a time.
func (c *Config) Validate() error {
	var errs []error

	if c.Connections.Mysql != nil {
		errs = appendIfErr(errs, c.Connections.Mysql.validate("connections.mysql"))
	}
	if c.Connections.Postgres != nil {
		errs = appendIfErr(errs, c.Connections.Postgres.validate("connections.postgres"))
	}
	if c.Connections.Mongodb != nil {
		errs = appendIfErr(errs, c.Connections.Mongodb.validate("connections.mongodb"))
	}
	if c.Connections.Redis != nil {
		errs = appendIfErr(errs, c.Connections.Redis.validate("connections.redis"))
	}
	if c.Connections.Elasticsearch != nil {
		errs = appendIfErr(errs, c.Connections.Elasticsearch.validate("connections.elasticsearch"))
	}
	if c.Connections.S3 != nil {
		errs = appendIfErr(errs, c.Connections.S3.validate("connections.s3"))
	}
	for name, m := range c.Connections.MysqlNamed {
		errs = appendIfErr(errs, m.validate("connections.mysql."+name))
	}
	for name, r := range c.Connections.RedisNamed {
		errs = appendIfErr(errs, r.validate("connections.redis."+name))
	}

	for i := range c.Databases {
		errs = appendIfErr(errs, c.Databases[i].validate(fmt.Sprintf("databases[%d]", i), len(c.Databases)))
	}
	for i := range c.Databases {
		errs = appendIfErr(errs, c.validateConnectionSelector(fmt.Sprintf("databases[%d]", i), &c.Databases[i]))
	}

	if n := len(c.MainWorktree.Databases); n > len(c.Databases) {
		errs = appendIfErr(errs, fmt.Errorf(
			"main_worktree.databases: overlay has %d entries but only %d top-level databases declared",
			n, len(c.Databases)))
	}
	for i := range c.MainWorktree.Databases {
		errs = appendIfErr(errs, c.MainWorktree.Databases[i].validate(
			fmt.Sprintf("main_worktree.databases[%d]", i)))
	}

	// branch_scoped on the main worktree only works when its active
	// namespace is slug-free. The main checkout's `.env` is NOT patched
	// (patches are skipped for the main worktree), so the app keeps
	// connecting to the bare name the repo-root `.env` already targets —
	// treeman must swap that exact name. A slug-bearing main template (or
	// no main overlay at all) renders a per-slug name the app never
	// connects to, so the swap lifecycle runs against a database nothing
	// uses and branch_scoped silently does nothing on the main worktree.
	// Catch the misconfiguration at load instead of failing silently.
	if c.MainWorktree.Enabled {
		for i := range c.Databases {
			if !c.Databases[i].BranchScoped {
				continue
			}
			tmpl, field := mainActiveTemplate(c, i)
			if strings.Contains(tmpl, "{slug") {
				errs = appendIfErr(errs, fmt.Errorf(
					"databases[%d]: branch_scoped + main_worktree.enabled requires a slug-free main_worktree.databases[%d].%s (got %q) — the main worktree's .env is not patched, so treeman must swap the bare name the app already connects to",
					i,
					i,
					field,
					tmpl,
				))
			}
		}
	}

	allowedPorts := c.PortSlotNames()
	for name, spec := range c.Ports {
		errs = appendIfErr(errs, validatePortSlot(name, spec))
	}

	for i := range c.Patches {
		errs = appendIfErr(errs, validatePatch(c.Patches[i], fmt.Sprintf("patches[%d]", i), allowedPorts))
	}

	errs = appendIfErr(errs, validateNotifications(c.Notifications))

	return errors.Join(errs...)
}

// validateNotifications rejects unknown `events:` buckets and an
// unrecognised `backend:`, so a typo (`stabel`, `notifysend`) surfaces
// at config-load instead of silently never firing.
func validateNotifications(n NotificationsConfig) error {
	var errs []error
	allowedBuckets := []string{"stable", "up", "down", "failed"}
	for i, b := range n.Events {
		if !slices.Contains(allowedBuckets, b) {
			errs = appendIfErr(errs, fmt.Errorf(
				"notifications.events[%d]: unknown bucket %q (allowed: %s)",
				i, b, strings.Join(allowedBuckets, ", ")))
		}
	}
	switch n.Backend {
	case "", "auto", "notify-send", "osascript", "none":
	default:
		errs = appendIfErr(errs, fmt.Errorf(
			"notifications.backend: unknown backend %q (allowed: auto, notify-send, osascript, none)", n.Backend))
	}
	return errors.Join(errs...)
}

// PortSlotNames returns the declared port slot names in stable
// (sorted) order. Used by config-load validation to decide which
// `{port_<name>}` tokens patches may reference, and by the
// allocator to drive its per-slot allocation loop.
func (c *Config) PortSlotNames() []string {
	if len(c.Ports) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Ports))
	for name := range c.Ports {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// mainActiveTemplate returns the effective main-worktree active-namespace
// template for databases[i] — the overlay value when set, else the base
// — plus the field name it came from (`name_template` for name-scoped
// engines, `key_prefix` for prefix-scoped). Used to check the main
// worktree's branch_scoped active namespace is slug-free.
func mainActiveTemplate(c *Config, i int) (tmpl, field string) {
	d := c.Databases[i]
	fam, _ := engine.Canonical(d.Engine)
	prefixScoped := fam.Scope() == engine.ScopePrefix
	var ov DatabaseOverlay
	if i < len(c.MainWorktree.Databases) {
		ov = c.MainWorktree.Databases[i]
	}
	if prefixScoped {
		if ov.KeyPrefix != "" {
			return ov.KeyPrefix, "key_prefix"
		}
		return d.KeyPrefix, "key_prefix"
	}
	if ov.NameTemplate != "" {
		return ov.NameTemplate, "name_template"
	}
	return d.NameTemplate, "name_template"
}

func validatePortSlot(name string, spec PortSpec) error {
	if name == "" {
		return errors.New("ports: slot name must be non-empty")
	}
	if spec.Range.Min == 0 || spec.Range.Max == 0 {
		return fmt.Errorf("ports[%s]: range [min, max] must be non-zero", name)
	}
	if spec.Range.Min > spec.Range.Max {
		return fmt.Errorf("ports[%s]: range invalid (min %d > max %d)", name, spec.Range.Min, spec.Range.Max)
	}
	return nil
}

// validateTemplate wraps template.Validate with a path-prefixed error
// so users see exactly which YAML field carries the typo.
func validateTemplate(path, tmpl string, sc template.Scope) error {
	if err := template.Validate(tmpl, sc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func validatePatch(p Patch, path string, allowedPorts []string) error {
	var errs []error
	for k, v := range p.Set {
		errs = appendIfErr(errs, validateTemplate(
			fmt.Sprintf("%s.set[%s]", path, k), v, template.Scope{AllowedPorts: allowedPorts}))
	}
	return errors.Join(errs...)
}

func appendIfErr(errs []error, err error) []error {
	if err == nil {
		return errs
	}
	return append(errs, err)
}

// validate guards the MySQL connection knobs the driver interpolates
// into DDL. charset/collation land verbatim in CREATE DATABASE
// statements, so values beyond plain identifier characters are
// rejected at load time instead of reaching the SQL layer.
func (c *MysqlConn) validate(path string) error {
	err := c.ContainerRef.validate(path)
	var errs []error
	errs = appendIfErr(errs, err)
	for _, f := range []struct{ name, val string }{
		{"charset", c.Charset},
		{"collation", c.Collation},
	} {
		if f.val == "" {
			continue
		}
		if !safeMySQLIdent(f.val) {
			errs = append(errs, fmt.Errorf(
				"%s.%s: %q is not a valid MySQL identifier (letters, digits, underscore)",
				path, f.name, f.val))
		}
	}
	return errors.Join(errs...)
}

// safeMySQLIdent accepts the shape every real charset/collation name
// has (utf8mb4, utf8mb4_0900_ai_ci, binary, latin1_swedish_ci).
func safeMySQLIdent(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return s != ""
}

// validate enforces that any templates carried in the overlay parse
// the same way as their base-config counterparts. Empty templates are
// fine — they simply inherit.
func (o DatabaseOverlay) validate(path string) error {
	var errs []error
	if o.NameTemplate != "" {
		errs = appendIfErr(errs, validateTemplate(path+".name_template", o.NameTemplate, template.Scope{}))
	}
	if o.KeyPrefix != "" {
		errs = appendIfErr(errs, validateTemplate(path+".key_prefix", o.KeyPrefix, template.Scope{}))
	}
	if o.TestClones != nil && o.TestClones.NameTemplate != "" {
		errs = appendIfErr(errs, validateCloneTemplate(path+".test_clones.name_template", o.TestClones.NameTemplate))
	}
	return errors.Join(errs...)
}

// validateCloneTemplate checks a test_clones.name_template: known keys
// only, and exactly one {n}. Without {n} every clone renders the same
// name; teardown matches clones as <before {n}><digits><after {n}>, so
// a second {n} would make its names unmatchable.
func validateCloneTemplate(path, tmpl string) error {
	if err := validateTemplate(path, tmpl, template.Scope{AllowN: true}); err != nil {
		return err
	}
	keys, err := template.Keys(tmpl)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if n := countKey(keys, "n"); n != 1 {
		return fmt.Errorf("%s: %q must contain {n} exactly once (found %d) so each clone gets its own name", path, tmpl, n)
	}
	return nil
}

// validateWorktreeScoped checks that a top-level name_template /
// key_prefix renders a different namespace per worktree. Without a
// slug key every worktree shares one database, and tearing one down
// drops it for all. The main_worktree overlay is exempt: the main
// worktree is a single checkout and may bind a bare name.
func validateWorktreeScoped(path, tmpl string) error {
	keys, err := template.Keys(tmpl)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if countKey(keys, "slug")+countKey(keys, "slug_dash") == 0 {
		return fmt.Errorf("%s: %q must contain {slug} or {slug_dash} so each worktree gets its own namespace", path, tmpl)
	}
	return nil
}

func countKey(keys []string, key string) int {
	n := 0
	for _, k := range keys {
		if k == key {
			n++
		}
	}
	return n
}

// validate enforces that container / compose_service are not both
// set; both can be empty (= use cfg.Host directly).
func (r ContainerRef) validate(path string) error {
	if r.Container != "" && r.ComposeService != "" {
		return fmt.Errorf("%s: container and compose_service are mutually exclusive (%q + %q)",
			path, r.Container, r.ComposeService)
	}
	return nil
}

// validate enforces that Engine is present and recognized, that
// NameTemplate is set for engines that scope by database name, and
// that every template string only references placeholders the
// rendering stage actually populates. The template checks catch
// typos (`{target-db}`, `{slag}`, `{n}` outside test_clones) at
// load time instead of at the first prepare run.
// validateS3 enforces the object-store constraints for an `engine: s3`
// database. key_prefix renders the per-worktree bucket name. The object
// store has no schema, so dump/migrate/seed are rejected (use a
// postcreate hook to populate). branch_scoped IS supported — each branch
// keeps its own durable bucket copied server-side — but test_clones
// (fingerprint-cached snapshot fan-out) is not.
func (d DatabaseConfig) validateS3(path string) error {
	if d.KeyPrefix == "" {
		return fmt.Errorf(
			"%s: key_prefix is required for engine \"s3\" (renders the per-worktree bucket name; AWS bucket-naming rules apply)",
			path,
		)
	}
	// Enforce a non-trivial literal prefix (everything before the first
	// `{` template token). Teardown matches buckets by literal prefix
	// across the whole AWS account, so a generic "dev" / "test" would
	// reap unrelated buckets. Mirrors the runtime guard in
	// s3.Driver.DropMatching.
	literal := d.KeyPrefix
	if i := strings.Index(literal, "{"); i >= 0 {
		literal = literal[:i]
	}
	const minLiteral = 6
	if len(literal) < minLiteral {
		return fmt.Errorf(
			"%s: key_prefix literal portion %q is too short (%d < %d) — S3 buckets share an account-wide namespace; use a project-specific literal like \"myapp-{slug}\" so teardown can't reap unrelated buckets",
			path,
			literal,
			len(literal),
			minLiteral,
		)
	}
	if d.Dump != nil {
		return fmt.Errorf(
			"%s: engine \"s3\" does not support `dump:` (object store has no dump format; use a postcreate hook to populate)",
			path,
		)
	}
	if d.TestClones != nil {
		return fmt.Errorf("%s: engine \"s3\" does not support `test_clones:` (no snapshot fan-out)", path)
	}
	if d.Migrate != nil {
		return fmt.Errorf(
			"%s: engine \"s3\" does not support `migrate:` (object stores have no schema; use a postcreate hook to populate)",
			path,
		)
	}
	if d.Seed != nil {
		return fmt.Errorf("%s: engine \"s3\" does not support `seed:` (use a postcreate hook to populate the bucket with fixtures)", path)
	}
	return nil
}

// validateFile enforces the file-backed family's constraints (sqlite
// / duckdb). The rendered name_template IS the database file path, so
// it must stay relative and inside the worktree (or under
// connections.sqlite.base_dir). There is no server-side namespace, so
// key_prefix and branch_scoped don't apply; dump / migrate / seed /
// test_clones all work, with one caveat: duckdb files can't execute
// .sql dumps from Go, so a duckdb entry seeds by copying a base file.
func (d DatabaseConfig) validateFile(path string) error {
	if strings.HasPrefix(d.NameTemplate, "/") || strings.HasPrefix(d.NameTemplate, "\\") {
		return fmt.Errorf(
			"%s: name_template %q must be a relative path (the file is placed inside the worktree, or under connections.sqlite.base_dir)",
			path, d.NameTemplate,
		)
	}
	for seg := range strings.SplitSeq(d.NameTemplate, "/") {
		if seg == ".." {
			return fmt.Errorf(
				"%s: name_template %q must not escape the worktree (no %q segments)",
				path, d.NameTemplate, "..",
			)
		}
	}
	if d.KeyPrefix != "" {
		return fmt.Errorf(
			"%s: engine %q does not support `key_prefix:` (file-backed engines scope by file path, not key prefix)",
			path, d.Engine,
		)
	}
	if d.BranchScoped {
		return fmt.Errorf(
			"%s: engine %q does not support `branch_scoped:` (no server to hold a per-branch durable namespace; the default per-worktree model is the isolation)",
			path,
			d.Engine,
		)
	}
	if d.Engine == "duckdb" {
		for _, dump := range d.Dump {
			if strings.HasSuffix(strings.ToLower(dump.Path), ".sql") {
				return fmt.Errorf(
					"%s: engine \"duckdb\" cannot execute .sql dump %q (no SQL executable from Go) — seed by copying a base .duckdb file, or use engine \"sqlite\" for .sql dumps",
					path,
					dump.Path,
				)
			}
		}
	}
	return nil
}

// validateFamily dispatches the families with constraint rules beyond
// the shared engine checks. Families without extra rules fall through
// with no error.
func (d DatabaseConfig) validateFamily(fam engine.Family, path string) error {
	switch fam {
	case engine.FamilyS3:
		return d.validateS3(path)
	case engine.FamilyFile:
		return d.validateFile(path)
	default:
		return nil
	}
}

// validateNamespace checks the field that names the per-worktree
// namespace: name_template for name-scoped engines, key_prefix for
// prefix-scoped ones. It must be set, and (outside the file family,
// whose default base is the worktree itself) reference a slug key.
func (d DatabaseConfig) validateNamespace(fam engine.Family, path string) error {
	if fam.Scope() == engine.ScopePrefix {
		if d.KeyPrefix == "" {
			return fmt.Errorf("%s: key_prefix is required for engine %q (used to compute the per-worktree namespace)", path, d.Engine)
		}
		return validateWorktreeScoped(path+".key_prefix", d.KeyPrefix)
	}
	if d.NameTemplate == "" {
		return fmt.Errorf("%s: name_template is required for engine %q (used to compute the per-worktree database name)", path, d.Engine)
	}
	if fam == engine.FamilyFile {
		return nil
	}
	return validateWorktreeScoped(path+".name_template", d.NameTemplate)
}

func (d DatabaseConfig) validate(path string, dbCount int) error {
	var errs []error
	if d.Engine == "" {
		return fmt.Errorf("%s: engine is required (one of: %s)", path, engine.KnownList())
	}
	fam, ok := engine.Canonical(d.Engine)
	if !ok {
		return fmt.Errorf("%s: unknown engine %q (allowed: %s)", path, d.Engine, engine.KnownList())
	}
	if err := d.validateNamespace(fam, path); err != nil {
		return err
	}
	// Per-family constraint checks (S3, file-backed).
	if err := d.validateFamily(fam, path); err != nil {
		return err
	}

	if d.NameTemplate != "" {
		errs = appendIfErr(errs, validateTemplate(path+".name_template", d.NameTemplate, template.Scope{}))
	}
	if d.KeyPrefix != "" {
		errs = appendIfErr(errs, validateTemplate(path+".key_prefix", d.KeyPrefix, template.Scope{}))
	}
	stepEnv := template.Scope{AllowTargetDB: true, DBCount: dbCount}
	if d.Migrate != nil {
		for k, v := range d.Migrate.Env {
			errs = appendIfErr(errs, validateTemplate(
				fmt.Sprintf("%s.migrate.env[%s]", path, k), v,
				stepEnv))
		}
	}
	if d.Seed != nil {
		for k, v := range d.Seed.Env {
			errs = appendIfErr(errs, validateTemplate(
				fmt.Sprintf("%s.seed.env[%s]", path, k), v,
				stepEnv))
		}
	}
	if d.Rollback != nil {
		for k, v := range d.Rollback.Env {
			errs = appendIfErr(errs, validateTemplate(
				fmt.Sprintf("%s.rollback.env[%s]", path, k), v,
				stepEnv))
		}
	}
	if d.TestClones != nil && d.TestClones.NameTemplate != "" {
		errs = appendIfErr(errs, validateCloneTemplate(path+".test_clones.name_template", d.TestClones.NameTemplate))
	}

	// branch_scoped and the test-clone fanout are mutually exclusive. A
	// branch_scoped database is a stateful per-branch snapshot the app
	// mutates in place; test clones are throwaway, reproducible copies
	// pre-warmed for parallel test workers. The two models contradict
	// each other (one swaps live data per branch, the other fans a
	// migrations-only template into N disposable replicas), so reject
	// the combination at config-load instead of silently ignoring one.
	if d.BranchScoped {
		if d.TestClones != nil {
			errs = appendIfErr(errs, fmt.Errorf(
				"%s: branch_scoped and test_clones are mutually exclusive — a branch_scoped database is a stateful per-branch snapshot, not a reproducible test-clone source",
				path,
			))
		}
		if d.Fanout > 0 {
			errs = appendIfErr(errs, fmt.Errorf(
				"%s: branch_scoped databases do not fan out — remove `fanout` (it only applies to the test-clone path)", path))
		}
	}

	errs = append(errs, d.validatePrewarm(path)...)
	errs = append(errs, d.validatePhysicalCloneMinBytes(path)...)
	return errors.Join(errs...)
}

// PrewarmCapableEngines lists the engine families whose driver
// implements the spare capabilities (engineconn.SnapshotCreator +
// SpareClaimer): Postgres claims spares via a constant-time catalog
// rename, MySQL via a staged physical clone. It lives here because
// engineconn imports config — a cycle would result the other way —
// and engineconn's tests cross-check that every listed family's conn
// really implements both interfaces, so the set can't drift.
var PrewarmCapableEngines = map[string]bool{
	"mysql":    true,
	"postgres": true,
}

// validatePrewarm guards `databases[].prewarm`. Accepts any family
// whose driver implements the spare capabilities (#53) and rejects
// the rest rather than silently ignoring, so the knob never reads as
// dead config.
func (d DatabaseConfig) validatePrewarm(path string) []error {
	if d.Prewarm == 0 {
		return nil
	}
	var errs []error
	fam, ok := engine.Canonical(d.Engine)
	if !ok || !PrewarmCapableEngines[string(fam)] {
		errs = append(errs, fmt.Errorf(
			"%s: prewarm is not supported for engine %q (capable: mysql, postgres) — it has no cheap spare-claim path",
			path, d.Engine))
	}
	if d.BranchScoped {
		errs = append(errs, fmt.Errorf(
			"%s: branch_scoped and prewarm are mutually exclusive — branch_scoped databases bypass the template cache that spares are cloned from",
			path,
		))
	}
	return errs
}

// validatePhysicalCloneMinBytes guards `databases[].physical_clone_min_bytes`.
// The threshold only feeds MySQL's physical-vs-logical strategy probe;
// reject rather than silently ignore on other engines so the knob
// never reads as dead config.
func (d DatabaseConfig) validatePhysicalCloneMinBytes(path string) []error {
	if d.PhysicalCloneMinBytes == nil {
		return nil
	}
	var errs []error
	if fam, ok := engine.Canonical(d.Engine); !ok || fam != engine.FamilyMySQL {
		errs = append(errs, fmt.Errorf(
			"%s: physical_clone_min_bytes is mysql-only — engine %q has no physical clone path",
			path, d.Engine))
	}
	if *d.PhysicalCloneMinBytes < 0 {
		errs = append(errs, fmt.Errorf("%s: physical_clone_min_bytes must be >= 0", path))
	}
	return errs
}
