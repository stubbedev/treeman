// Snapshot primitives for the Redis driver. Mirrors the MySQL +
// Postgres SnapshotCreate / SnapshotRestore shape so the prepare
// orchestrator can treat Redis with the same cache-hit / cold-build
// flow as every other engine.
//
// Isolation primitive: a key *prefix* (not a DB-index). All keys
// live in one Redis logical DB; per-worktree separation comes from
// every key being prefixed with the worktree's slug. This lifts the
// 16-DB cap and works on cluster mode (where multi-DB is disabled).
//
// Cloning uses the server-side `COPY` command (Redis 6.2+) so each
// per-key copy is one round-trip instead of the older DUMP→RESTORE
// dance. COPY preserves TTLs and is pipelined for batching.

package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// DBIndex is the single Redis logical DB treeman uses for prefix-
// based isolation. Default 0; can be overridden in the future via a
// config knob if needed.
const DBIndex = 0

// PrefixExists reports whether ANY key matching `prefix*` lives in
// Redis. Used by the cache-hit path as the equivalent of MySQL's
// `DatabaseExists`. SCAN with COUNT=1 is the cheapest probe — it
// stops as soon as the first matching key shows up.
func (d *Driver) PrefixExists(ctx context.Context, prefix string) (bool, error) {
	c := d.client()
	defer c.Close()
	iter := c.Scan(ctx, 0, prefix+"*", 1).Iterator()
	if iter.Next(ctx) {
		return true, nil
	}
	return false, iter.Err()
}

// DropPrefix deletes every key under `prefix`. Batches DEL calls in
// chunks of 1000 — DEL with thousands of keys can stall the
// single-threaded Redis event loop for tens of ms; the batch cap
// keeps the worst-case pause bounded.
func (d *Driver) DropPrefix(ctx context.Context, prefix string) (int, error) {
	if prefix == "" {
		return 0, fmt.Errorf("redis: refusing to drop empty prefix (would wipe every key)")
	}
	c := d.client()
	defer c.Close()
	iter := c.Scan(ctx, 0, prefix+"*", 1000).Iterator()
	var batch []string
	deleted := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := c.Del(ctx, batch...).Err(); err != nil {
			return fmt.Errorf("DEL batch: %w", err)
		}
		deleted += len(batch)
		batch = batch[:0]
		return nil
	}
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) >= 1000 {
			if err := flush(); err != nil {
				return deleted, err
			}
		}
	}
	if err := iter.Err(); err != nil {
		return deleted, err
	}
	if err := flush(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

// SnapshotCreate copies every key under `sourcePrefix` to a
// matching key under `templatePrefix`. Same wiring as MySQL: after
// this returns, both prefixes hold identical data. Drops any
// pre-existing keys under templatePrefix first so the clone has
// clean ground.
func (d *Driver) SnapshotCreate(ctx context.Context, sourcePrefix, templatePrefix string) error {
	if sourcePrefix == templatePrefix {
		return fmt.Errorf("snapshot create: source and template prefixes must differ")
	}
	if _, err := d.DropPrefix(ctx, templatePrefix); err != nil {
		return fmt.Errorf("drop stale template %s*: %w", templatePrefix, err)
	}
	return d.copyByPrefix(ctx, sourcePrefix, templatePrefix)
}

// SnapshotRestore copies template → target. Used by the fanout path
// so paratest workers each get an isolated copy.
func (d *Driver) SnapshotRestore(ctx context.Context, templatePrefix, targetPrefix string) error {
	if templatePrefix == targetPrefix {
		return fmt.Errorf("snapshot restore: template and target prefixes must differ")
	}
	if _, err := d.DropPrefix(ctx, targetPrefix); err != nil {
		return fmt.Errorf("drop stale target %s*: %w", targetPrefix, err)
	}
	return d.copyByPrefix(ctx, templatePrefix, targetPrefix)
}

// DropSnapshot deletes every key under the named template prefix.
// Used by the snapshot GC sweep when the SQLite cap evicts a row.
func (d *Driver) DropSnapshot(ctx context.Context, templatePrefix string) error {
	_, err := d.DropPrefix(ctx, templatePrefix)
	return err
}

// copyByPrefix is the inner loop shared by SnapshotCreate /
// SnapshotRestore. Dispatches between two strategies:
//
//   • Redis 6.2+ → server-side `COPY` (one round-trip per key,
//     pipelined ≥100 keys per Exec). TTLs preserved automatically.
//   • Redis < 6.2 → DUMP + RESTORE fallback (two round-trips per
//     batch: phase 1 DUMP+PTTL, phase 2 RESTORE REPLACE). RESTORE
//     REPLACE requires Redis 3.0+, which is universally satisfied.
//
// Version detection is lazy + cached on the Driver via sync.Once.
// On detection failure we assume legacy + use DUMP+RESTORE so a
// transient INFO error doesn't surface as "ERR unknown command".
func (d *Driver) copyByPrefix(ctx context.Context, srcPrefix, dstPrefix string) error {
	if srcPrefix == "" {
		return fmt.Errorf("redis: refusing to copy from empty prefix")
	}
	if d.supportsCopy(ctx) {
		return d.copyByPrefixCOPY(ctx, srcPrefix, dstPrefix)
	}
	return d.copyByPrefixDumpRestore(ctx, srcPrefix, dstPrefix)
}

// supportsCopy detects (once) whether the connected Redis is 6.2+
// and caches the result. On detection failure we conservatively
// return false — DUMP+RESTORE works everywhere.
func (d *Driver) supportsCopy(ctx context.Context) bool {
	d.copyOnce.Do(func() {
		v, err := d.EngineVersion(ctx)
		if err != nil || v == "" {
			d.copySupported = false
			return
		}
		d.copySupported = versionAtLeast(v, 6, 2)
	})
	return d.copySupported
}

// versionAtLeast parses dotted-decimal version strings like "6.2.0"
// or "7.0.5-rc1" and reports whether (major, minor) ≥ (wantMajor,
// wantMinor). Trailing non-numeric junk after the minor is ignored.
func versionAtLeast(v string, wantMajor, wantMinor int) bool {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	maj, err1 := strconv.Atoi(parts[0])
	// Strip any non-digit suffix from the minor part (e.g. "0-rc1" → "0").
	minor := parts[1]
	for i := 0; i < len(minor); i++ {
		if minor[i] < '0' || minor[i] > '9' {
			minor = minor[:i]
			break
		}
	}
	min, err2 := strconv.Atoi(minor)
	if err1 != nil || err2 != nil {
		return false
	}
	if maj > wantMajor {
		return true
	}
	if maj == wantMajor && min >= wantMinor {
		return true
	}
	return false
}

// copyByPrefixCOPY is the Redis 6.2+ fast path.
func (d *Driver) copyByPrefixCOPY(ctx context.Context, srcPrefix, dstPrefix string) error {
	c := d.client()
	defer c.Close()

	iter := c.Scan(ctx, 0, srcPrefix+"*", 1000).Iterator()
	pipe := c.Pipeline()
	pending := 0
	flush := func() error {
		if pending == 0 {
			return nil
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("COPY pipeline exec: %w", err)
		}
		pending = 0
		return nil
	}
	for iter.Next(ctx) {
		src := iter.Val()
		dst := dstPrefix + strings.TrimPrefix(src, srcPrefix)
		pipe.Copy(ctx, src, dst, DBIndex, true) // REPLACE so re-runs are idempotent
		pending++
		if pending >= 100 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := iter.Err(); err != nil {
		return err
	}
	return flush()
}

// copyByPrefixDumpRestore is the pre-6.2 fallback.
//
// Per batch of up to 100 keys:
//   Phase 1: pipeline DUMP + PTTL on every source key (1 round-trip).
//   Phase 2: pipeline RESTORE on every destination key (1 round-trip).
//
// Two round-trips per 100 keys is slower than COPY's one, but
// network-cost-wise it's still ~50 keys per network hop. TTLs are
// preserved by passing PTTL's result to RESTORE.
//
// Keys that vanish between SCAN and DUMP (race with a concurrent
// DEL or TTL expiry) surface as `redis.Nil` from DUMP and are
// silently skipped — the SCAN cursor only guarantees a snapshot of
// keys *eventually* visited, not that every visited key still exists.
func (d *Driver) copyByPrefixDumpRestore(ctx context.Context, srcPrefix, dstPrefix string) error {
	c := d.client()
	defer c.Close()

	iter := c.Scan(ctx, 0, srcPrefix+"*", 1000).Iterator()
	var batch []string

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		// Phase 1: DUMP + PTTL for every key in the batch.
		dumpPipe := c.Pipeline()
		dumpCmds := make([]*redis.StringCmd, len(batch))
		pttlCmds := make([]*redis.DurationCmd, len(batch))
		for i, k := range batch {
			dumpCmds[i] = dumpPipe.Dump(ctx, k)
			pttlCmds[i] = dumpPipe.PTTL(ctx, k)
		}
		// pipeline.Exec returns a single error even when individual
		// commands returned redis.Nil — we surface only fatal errors.
		if _, err := dumpPipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("DUMP+PTTL batch: %w", err)
		}

		// Phase 2: RESTORE each onto the dst key.
		restorePipe := c.Pipeline()
		restored := 0
		for i, src := range batch {
			dump, err := dumpCmds[i].Result()
			if err != nil {
				if errors.Is(err, redis.Nil) {
					continue // key vanished mid-snapshot — skip
				}
				return fmt.Errorf("DUMP %s: %w", src, err)
			}
			pttl, _ := pttlCmds[i].Result()
			var ttl time.Duration
			if pttl > 0 {
				ttl = pttl
			}
			dst := dstPrefix + strings.TrimPrefix(src, srcPrefix)
			restorePipe.RestoreReplace(ctx, dst, ttl, dump)
			restored++
		}
		if restored > 0 {
			if _, err := restorePipe.Exec(ctx); err != nil {
				return fmt.Errorf("RESTORE batch: %w", err)
			}
		}
		batch = batch[:0]
		return nil
	}

	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) >= 100 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := iter.Err(); err != nil {
		return err
	}
	return flush()
}

// client opens a fresh go-redis client pinned to the treeman DB
// index. Each helper opens its own client + defers Close — keeps
// the lifecycle simple at the cost of more connection churn than
// a long-lived pool. Fine for prepare-time operations which run
// per worktree-create, not per-request.
func (d *Driver) client() *redis.Client {
	opts, _ := redis.ParseURL(d.url)
	opts.DB = DBIndex
	return redis.NewClient(opts)
}
