package config

import (
	"errors"
	"fmt"
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

	return errors.Join(errs...)
}

func appendIfErr(errs []error, err error) []error {
	if err == nil {
		return errs
	}
	return append(errs, err)
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

// validate enforces that Engine is present and recognized, and that
// NameTemplate is set for engines that scope by database name.
func (d DatabaseConfig) validate(path string) error {
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
	return nil
}
