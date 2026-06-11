package prepare

import (
	"context"
	"sync"
)

// probeCacheKey carries the per-run probe cache through the context so
// the five prepareX handlers share it without widening their (already
// wide) parameter lists.
type probeCacheKey struct{}

// withProbeCache returns ctx carrying a fresh probe cache. Installed
// once at the top of RunFiltered so the cache's lifetime is exactly one
// prepare run — engine restarts/upgrades between runs are always seen.
func withProbeCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, probeCacheKey{}, &sync.Map{})
}

// cachedProbe memoizes fn's result under key for the duration of one
// prepare run. N databases on the same engine connection otherwise pay
// N identical EngineVersion/MaxConnections round-trips — one per
// parallel prepareX goroutine. Errors are not cached (the next caller
// retries), and concurrent first probes may race-duplicate the call;
// both are fine because every probe is an idempotent read. Without a
// cache in ctx (direct test calls) it degrades to calling fn.
func cachedProbe[T any](ctx context.Context, key string, fn func() (T, error)) (T, error) {
	m, _ := ctx.Value(probeCacheKey{}).(*sync.Map)
	if m == nil {
		return fn()
	}
	if v, ok := m.Load(key); ok {
		return v.(T), nil //nolint:forcetypeassert // only this file writes the map; values under one key share T
	}
	v, err := fn()
	if err != nil {
		var zero T
		return zero, err
	}
	actual, _ := m.LoadOrStore(key, v)
	return actual.(T), nil //nolint:forcetypeassert // same-key values share T
}
