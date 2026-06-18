// Package template renders the `{key}` placeholders used by
// `.treeman.yaml`'s patches `set:` map, database `name_template:`
// / `key_prefix:` strings, and `migrate.env:` / `seed.env:` value
// templates.
//
// Known keys:
//
//	{slug}              — the slug value (e.g. proj_1234)
//	{slug_dash}         — underscores replaced with hyphens
//	{slug_redis_queue}  — 6..15 (cksum-derived)
//	{slug_redis_cache}  — 6..15 (cksum-derived, distinct from queue)
//	{n}                 — paratest replica index (only valid when
//	                     `.WithN()` was used on the context)
//	{target_db}         — resolved per-run database name / key prefix
//	                     (only valid when `.WithTargetDB()` was used —
//	                     i.e. inside `migrate.env:` / `seed.env:` values
//	                     after `name_template` / `key_prefix` rendered)
//	{port_<name>}       — per-worktree port for the slot named `<name>`
//	                     declared in the top-level `ports:` block. Only
//	                     valid when `.WithPorts()` populated the context.
//
// Unknown keys fail loudly (RenderError.UnknownKey) so a YAML typo
// doesn't silently render as empty and quietly break some downstream
// query.
package template

import (
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/stubbedev/treeman/internal/slug"
)

// Context carries the substitution values.
type Context struct {
	Slug           string
	SlugDash       string
	SlugRedisQueue string
	SlugRedisCache string
	N              int // valid only when HasN is true
	HasN           bool
	TargetDB       string // valid only when HasTargetDB is true
	HasTargetDB    bool
	// Ports holds per-worktree port assignments keyed by the slot
	// name declared in the top-level `ports:` block. Tokens of the
	// form `{port_<name>}` resolve to ports[name]. Empty map means
	// "no ports allocated for this context"; rendering a `{port_*}`
	// token against an empty map returns RenderError.UnknownKey.
	Ports map[string]uint16
}

// FromSlug builds a Context from a Slug. The `N` field stays unset
// until WithN is called.
func FromSlug(s slug.Slug) Context {
	q, c := s.RedisIndices()
	return Context{
		Slug:           s.Value,
		SlugDash:       s.Dashed(),
		SlugRedisQueue: strconv.Itoa(int(q)),
		SlugRedisCache: strconv.Itoa(int(c)),
	}
}

// WithN returns a copy with the paratest replica index set.
func (c Context) WithN(n int) Context {
	c.N = n
	c.HasN = true
	return c
}

// WithTargetDB returns a copy with the resolved per-run database
// name / key prefix set. Used by the migration runner so
// `migrate.env:` / `seed.env:` values can reference `{target_db}`
// after `name_template` / `key_prefix` already rendered.
func (c Context) WithTargetDB(db string) Context {
	c.TargetDB = db
	c.HasTargetDB = true
	return c
}

// WithPorts returns a copy with the per-worktree port slots set.
// A nil/empty map clears any previously-set ports.
func (c Context) WithPorts(ports map[string]uint16) Context {
	if len(ports) == 0 {
		c.Ports = nil
		return c
	}
	dup := make(map[string]uint16, len(ports))
	maps.Copy(dup, ports)
	c.Ports = dup
	return c
}

// RenderError reports a typo or unsupported key in a template.
type RenderError struct {
	UnknownKey  string
	UnmatchedAt int
}

func (e *RenderError) Error() string {
	if e.UnknownKey != "" {
		return "unknown template key: " + e.UnknownKey
	}
	return fmt.Sprintf("unmatched '{' at offset %d", e.UnmatchedAt)
}

// Render replaces every `{key}` in `tmpl` with the matching field
// from ctx. Errors when an unknown key is encountered or a `{` has
// no closing `}`.
func Render(tmpl string, ctx Context) (string, error) {
	return renderWith(tmpl, func(key string) (string, bool) { return lookup(key, ctx) })
}

// RenderMap renders the same `{key}` placeholder syntax as Render but
// resolves keys against a flat map rather than the slug-centric
// Context. Used by `treeman status` so its format strings share one
// templating convention with the rest of `.treeman.yaml` instead of
// introducing a second engine. Unknown keys fail loudly, same as
// Render, so a typo in a status format surfaces immediately.
func RenderMap(tmpl string, vals map[string]string) (string, error) {
	return renderWith(tmpl, func(key string) (string, bool) {
		v, ok := vals[key]
		return v, ok
	})
}

// renderWith is the shared `{key}` scanner. lookup resolves a key to
// its replacement; returning ok=false triggers RenderError.UnknownKey.
func renderWith(tmpl string, lookup func(key string) (string, bool)) (string, error) {
	var b strings.Builder
	b.Grow(len(tmpl))
	i := 0
	for i < len(tmpl) {
		if tmpl[i] != '{' {
			b.WriteByte(tmpl[i])
			i++
			continue
		}
		rest := tmpl[i+1:]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			return "", &RenderError{UnmatchedAt: i}
		}
		key := rest[:end]
		val, ok := lookup(key)
		if !ok {
			return "", &RenderError{UnknownKey: key}
		}
		b.WriteString(val)
		i += 1 + end + 1
	}
	return b.String(), nil
}

// Scope enables optional keys at validation time. `name_template` /
// `key_prefix` strings render before the per-run DB name exists, so
// `{target_db}` is illegal there; `migrate.env:` / `seed.env:`
// values render after, so it's legal. `{n}` is only valid in
// templates the paratest fan-out renders per replica. AllowedPorts
// lists the slot names that `{port_<name>}` tokens may reference;
// nil means "no port tokens allowed in this scope".
type Scope struct {
	AllowN        bool
	AllowTargetDB bool
	AllowedPorts  []string
}

// Validate checks that `tmpl` only references known keys allowed by
// `sc`. Returns nil when the template would render cleanly with the
// matching `WithN` / `WithTargetDB` / `WithPorts` already applied.
// Use this at config-load time so a YAML typo (`{target-db}`,
// `{slag}`, `{port_typo}`) is caught before a worktree create /
// prepare run trips over it.
func Validate(tmpl string, sc Scope) error {
	ctx := Context{
		Slug:           "x",
		SlugDash:       "x",
		SlugRedisQueue: "0",
		SlugRedisCache: "0",
	}
	if sc.AllowN {
		ctx = ctx.WithN(0)
	}
	if sc.AllowTargetDB {
		ctx = ctx.WithTargetDB("x")
	}
	if len(sc.AllowedPorts) > 0 {
		ports := make(map[string]uint16, len(sc.AllowedPorts))
		for _, name := range sc.AllowedPorts {
			ports[name] = 1
		}
		ctx = ctx.WithPorts(ports)
	}
	_, err := Render(tmpl, ctx)
	return err
}

func lookup(key string, ctx Context) (string, bool) {
	switch key {
	case "slug":
		return ctx.Slug, true
	case "slug_dash":
		return ctx.SlugDash, true
	case "slug_redis_queue":
		return ctx.SlugRedisQueue, true
	case "slug_redis_cache":
		return ctx.SlugRedisCache, true
	case "n":
		if !ctx.HasN {
			return "", false
		}
		return strconv.Itoa(ctx.N), true
	case "target_db":
		if !ctx.HasTargetDB {
			return "", false
		}
		return ctx.TargetDB, true
	}
	if name, ok := strings.CutPrefix(key, "port_"); ok {
		if port, exists := ctx.Ports[name]; exists {
			return strconv.Itoa(int(port)), true
		}
		return "", false
	}
	return "", false
}
