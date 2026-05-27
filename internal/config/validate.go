package config

import (
	"errors"
	"fmt"
	"sort"

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
		errs = appendIfErr(errs, c.Connections.Mysql.ContainerRef.validate("connections.mysql"))
	}
	if c.Connections.Postgres != nil {
		errs = appendIfErr(errs, c.Connections.Postgres.ContainerRef.validate("connections.postgres"))
	}
	if c.Connections.Mongodb != nil {
		errs = appendIfErr(errs, c.Connections.Mongodb.ContainerRef.validate("connections.mongodb"))
	}
	if c.Connections.Redis != nil {
		errs = appendIfErr(errs, c.Connections.Redis.ContainerRef.validate("connections.redis"))
	}
	if c.Connections.Elasticsearch != nil {
		errs = appendIfErr(errs, c.Connections.Elasticsearch.ContainerRef.validate("connections.elasticsearch"))
	}

	for i := range c.Databases {
		errs = appendIfErr(errs, c.Databases[i].validate(fmt.Sprintf("databases[%d]", i)))
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

	allowedPorts := c.PortSlotNames()
	for name, spec := range c.Ports {
		errs = appendIfErr(errs, validatePortSlot(name, spec))
	}

	for i := range c.Patches {
		errs = appendIfErr(errs, validatePatch(c.Patches[i], fmt.Sprintf("patches[%d]", i), allowedPorts))
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

func validatePortSlot(name string, spec PortSpec) error {
	if name == "" {
		return fmt.Errorf("ports: slot name must be non-empty")
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
		errs = appendIfErr(errs, validateTemplate(
			path+".test_clones.name_template", o.TestClones.NameTemplate,
			template.Scope{AllowN: true}))
	}
	return errors.Join(errs...)
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
func (d DatabaseConfig) validate(path string) error {
	var errs []error
	if d.Engine == "" {
		return fmt.Errorf("%s: engine is required (one of: mysql, mariadb, tidb, postgres, postgresql, mongodb, redis, elasticsearch, opensearch)", path)
	}
	switch d.Engine {
	case "mysql", "mariadb", "tidb", "postgres", "postgresql", "mongodb":
		if d.NameTemplate == "" {
			return fmt.Errorf("%s: name_template is required for engine %q (used to compute the per-worktree database name)", path, d.Engine)
		}
	case "redis", "elasticsearch", "opensearch":
		// Prefix-scoped engines — name_template doesn't apply.
	default:
		return fmt.Errorf("%s: unknown engine %q (allowed: mysql, mariadb, tidb, postgres, postgresql, mongodb, redis, elasticsearch, opensearch)",
			path, d.Engine)
	}

	if d.NameTemplate != "" {
		errs = appendIfErr(errs, validateTemplate(path+".name_template", d.NameTemplate, template.Scope{}))
	}
	if d.KeyPrefix != "" {
		errs = appendIfErr(errs, validateTemplate(path+".key_prefix", d.KeyPrefix, template.Scope{}))
	}
	if d.Migrate != nil {
		for k, v := range d.Migrate.Env {
			errs = appendIfErr(errs, validateTemplate(
				fmt.Sprintf("%s.migrate.env[%s]", path, k), v,
				template.Scope{AllowTargetDB: true}))
		}
	}
	if d.Seed != nil {
		for k, v := range d.Seed.Env {
			errs = appendIfErr(errs, validateTemplate(
				fmt.Sprintf("%s.seed.env[%s]", path, k), v,
				template.Scope{AllowTargetDB: true}))
		}
	}
	if d.TestClones != nil && d.TestClones.NameTemplate != "" {
		errs = appendIfErr(errs, validateTemplate(
			path+".test_clones.name_template", d.TestClones.NameTemplate,
			template.Scope{AllowN: true}))
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
				"%s: branch_scoped and test_clones are mutually exclusive — a branch_scoped database is a stateful per-branch snapshot, not a reproducible test-clone source", path))
		}
		if d.Fanout > 0 {
			errs = appendIfErr(errs, fmt.Errorf(
				"%s: branch_scoped databases do not fan out — remove `fanout` (it only applies to the test-clone path)", path))
		}
	}
	return errors.Join(errs...)
}
