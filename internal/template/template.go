// Package template renders the `{key}` placeholders used by
// `.treeman.yaml`'s env_scoping.patches and database name_templates.
// Ported from `crates/treeman-core/src/template.rs`.
//
// Known keys:
//   {slug}              — the slug value (e.g. proj_1234)
//   {slug_dash}         — underscores replaced with hyphens
//   {slug_redis_queue}  — 6..15 (cksum-derived)
//   {slug_redis_cache}  — 6..15 (cksum-derived, distinct from queue)
//   {n}                 — paratest replica index (only valid when
//                        `.WithN()` was used on the context)
//
// Unknown keys fail loudly (RenderError.UnknownKey) so a YAML typo
// doesn't silently render as empty and quietly break some downstream
// query.
package template

import (
	"fmt"
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

// RenderError reports a typo or unsupported key in a template.
type RenderError struct {
	UnknownKey   string
	UnmatchedAt  int
}

func (e *RenderError) Error() string {
	if e.UnknownKey != "" {
		return fmt.Sprintf("unknown template key: %s", e.UnknownKey)
	}
	return fmt.Sprintf("unmatched '{' at offset %d", e.UnmatchedAt)
}

// Render replaces every `{key}` in `tmpl` with the matching field
// from ctx. Errors when an unknown key is encountered or a `{` has
// no closing `}`.
func Render(tmpl string, ctx Context) (string, error) {
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
		val, ok := lookup(key, ctx)
		if !ok {
			return "", &RenderError{UnknownKey: key}
		}
		b.WriteString(val)
		i += 1 + end + 1
	}
	return b.String(), nil
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
	default:
		return "", false
	}
}
