// Package engine centralises the supported DB engine identifiers
// treeman accepts as `databases[].engine`. Other packages canonicalise
// raw config strings through Canonical so alias additions (e.g.
// `tidb` → mysql family) land in one place instead of drifting across
// every per-engine switch statement. The drift was the root cause of
// the snapshot GC `eviction: unsupported engine "mongodb"` regression
// (gc.go enumerated mysql + postgres only) and the later opensearch
// follow-up; centralising the mapping makes future additions a single
// edit.
package engine

import "strings"

// Family is the canonical engine label (one driver per family —
// aliases collapse to it).
type Family string

const (
	FamilyMySQL    Family = "mysql"         // mysql / mariadb / tidb
	FamilyPostgres Family = "postgres"      // postgres / postgresql
	FamilyMongo    Family = "mongodb"       // mongodb
	FamilyRedis    Family = "redis"         // redis
	FamilyES       Family = "elasticsearch" // elasticsearch / opensearch
)

// Known is every engine string treeman accepts under
// `databases[].engine`. Single source of truth: validate.go's allow
// list, gc.go's switch, MCP schema enums all derive from this.
// Adding a new alias means appending here and (typically) updating
// Canonical to map it to the right Family.
var Known = []string{
	"mysql", "mariadb", "tidb",
	"postgres", "postgresql",
	"mongodb",
	"redis",
	"elasticsearch", "opensearch",
}

// KnownList returns the engine alias list joined as ", " — used in
// error messages so the wording lives in one place.
func KnownList() string {
	return strings.Join(Known, ", ")
}

// Canonical maps any engine alias to its Family. Returns ok=false
// for engines treeman does not recognise so callers can distinguish
// "unsupported" from "no-op for this family".
func Canonical(eng string) (Family, bool) {
	switch eng {
	case "mysql", "mariadb", "tidb":
		return FamilyMySQL, true
	case "postgres", "postgresql":
		return FamilyPostgres, true
	case "mongodb":
		return FamilyMongo, true
	case "redis":
		return FamilyRedis, true
	case "elasticsearch", "opensearch":
		return FamilyES, true
	default:
		return "", false
	}
}

// Scope reports whether the engine family scopes by full database
// name (mysql/postgres/mongo) or by key/index prefix (redis/es).
// Prefix-scoped engines don't accept a `name_template`.
type Scope int

const (
	ScopeName Scope = iota
	ScopePrefix
)

// Scope returns whether the family scopes by name or by prefix.
func (f Family) Scope() Scope {
	switch f {
	case FamilyRedis, FamilyES:
		return ScopePrefix
	default:
		return ScopeName
	}
}
