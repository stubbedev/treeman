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

// globEscape escapes the glob metacharacters Redis SCAN/KEYS MATCH
// patterns recognise (`*`, `?`, `[`, `]`, `\`) so a prefix is matched
// literally. Without this a prefix containing any of these would be
// interpreted as a wildcard and could match — and therefore drop or
// copy — unrelated keys. The trailing `*` callers append for the
// prefix wildcard is added AFTER escaping and so stays a wildcard.
func globEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		switch c := s[i]; c {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// PrefixExists reports whether ANY key matching `prefix*` lives in
// Redis. Used by the cache-hit path as the equivalent of MySQL's
// `DatabaseExists`. SCAN with COUNT=1 is the cheapest probe — it
// stops as soon as the first matching key shows up.
func (d *Driver) PrefixExists(ctx context.Context, prefix string) (bool, error) {
	return d.PrefixExistsFiltered(ctx, prefix, nil)
}

// PrefixExistsFiltered is PrefixExists with an optional `keep` predicate:
// only keys for which keep(key) returns true count toward existence. A
// nil predicate considers every key under `prefix` (classic behaviour).
// The branch_scoped swap uses this so a bare main-worktree active prefix
// — which is also a prefix of every sibling worktree's `<prefix>_<slug>_*`
// keys — does not report "exists" purely because a sibling has data.
//
// With a filter the cheap COUNT=1 early-out no longer applies: a matching
// key might be sibling-owned, so the scan must continue until it finds a
// kept key or exhausts the cursor. Use a larger COUNT to keep round-trips
// down on the common case where the worktree owns most keys.
func (d *Driver) PrefixExistsFiltered(ctx context.Context, prefix string, keep func(string) bool) (bool, error) {
	c := d.client()
	count := int64(1)
	if keep != nil {
		count = 100
	}
	iter := c.Scan(ctx, 0, globEscape(prefix)+"*", count).Iterator()
	for iter.Next(ctx) {
		if keep == nil || keep(iter.Val()) {
			return true, nil
		}
	}
	return false, iter.Err()
}

// DropPrefix deletes every key under `prefix`. Batches DEL calls in
// chunks of 1000 — DEL with thousands of keys can stall the
// single-threaded Redis event loop for tens of ms; the batch cap
// keeps the worst-case pause bounded.
func (d *Driver) DropPrefix(ctx context.Context, prefix string) (int, error) {
	return d.DropPrefixFiltered(ctx, prefix, nil)
}

// DropPrefixFiltered is DropPrefix with an optional `keep` predicate:
// only keys for which keep(key) returns true are deleted. A nil
// predicate is the classic DropPrefix behaviour.
//
// Cold-build uses this so sibling worktrees' branch-scoped keys that
// share the current worktree's source prefix (e.g. main wt prefix
// `app_` is also a prefix of every other wt's `app_<slug>_*`) survive
// the eager pre-build drop.
func (d *Driver) DropPrefixFiltered(ctx context.Context, prefix string, keep func(string) bool) (int, error) {
	if prefix == "" {
		return 0, errors.New("redis: refusing to drop empty prefix (would wipe every key)")
	}
	c := d.client()
	iter := c.Scan(ctx, 0, globEscape(prefix)+"*", 1000).Iterator()
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
		k := iter.Val()
		if keep != nil && !keep(k) {
			continue
		}
		batch = append(batch, k)
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
	return d.SnapshotCreateFiltered(ctx, sourcePrefix, templatePrefix, nil)
}

// SnapshotCreateFiltered is SnapshotCreate with an optional `keep`
// predicate restricting which SOURCE keys are captured: only keys for
// which keep(key) returns true are copied into the template prefix. A nil
// predicate captures everything under sourcePrefix (classic behaviour).
// The branch_scoped swap uses this so a bare main-worktree active prefix
// does not pull sibling-owned keys into this branch's durable copy. The
// template (durable) prefix is hash-derived and never collides, so its
// stale-cleanup drop stays unfiltered.
func (d *Driver) SnapshotCreateFiltered(ctx context.Context, sourcePrefix, templatePrefix string, keep func(string) bool) error {
	if sourcePrefix == templatePrefix {
		return errors.New("snapshot create: source and template prefixes must differ")
	}
	if _, err := d.DropPrefix(ctx, templatePrefix); err != nil {
		return fmt.Errorf("drop stale template %s*: %w", templatePrefix, err)
	}
	return d.copyByPrefix(ctx, sourcePrefix, templatePrefix, keep)
}

// SnapshotRestore copies template → target. Used by the fanout path
// so paratest workers each get an isolated copy.
func (d *Driver) SnapshotRestore(ctx context.Context, templatePrefix, targetPrefix string) error {
	return d.SnapshotRestoreFiltered(ctx, templatePrefix, targetPrefix, nil)
}

// SnapshotRestoreFiltered is SnapshotRestore with an optional `keep`
// predicate restricting which TARGET keys the stale-target cleanup may
// drop: only target keys for which keep(key) returns true are dropped
// before the copy. A nil predicate drops everything under targetPrefix
// (classic behaviour). The branch_scoped swap uses this so restoring into
// a bare main-worktree active prefix does not wipe sibling worktrees'
// `<prefix>_<slug>_*` keys. The template (durable) source is hash-derived
// and never collides, so the copy itself stays unfiltered.
func (d *Driver) SnapshotRestoreFiltered(ctx context.Context, templatePrefix, targetPrefix string, keep func(string) bool) error {
	return d.SnapshotRestoreSrcFiltered(ctx, templatePrefix, targetPrefix, nil, keep)
}

// SnapshotRestoreSrcFiltered is SnapshotRestore with independent filters
// for the SOURCE (`srcKeep`, which template keys to copy) and the TARGET
// (`tgtKeep`, which target keys the stale-cleanup may drop). Either nil
// means "all". The branch_scoped parent-seed uses srcKeep to copy only the
// parent worktree's OWN keys when the parent prefix is a bare main-worktree
// prefix nesting sibling worktrees' keys; tgtKeep spares the current
// worktree's siblings. Durable resume passes srcKeep=nil (durable prefix is
// hash-derived and never nests).
func (d *Driver) SnapshotRestoreSrcFiltered(
	ctx context.Context,
	templatePrefix, targetPrefix string,
	srcKeep, tgtKeep func(string) bool,
) error {
	if templatePrefix == targetPrefix {
		return errors.New("snapshot restore: template and target prefixes must differ")
	}
	if _, err := d.DropPrefixFiltered(ctx, targetPrefix, tgtKeep); err != nil {
		return fmt.Errorf("drop stale target %s*: %w", targetPrefix, err)
	}
	return d.copyByPrefix(ctx, templatePrefix, targetPrefix, srcKeep)
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
//   - Redis 6.2+ → server-side `COPY` (one round-trip per key,
//     pipelined ≥100 keys per Exec). TTLs preserved automatically.
//   - Redis < 6.2 → DUMP + RESTORE fallback (two round-trips per
//     batch: phase 1 DUMP+PTTL, phase 2 RESTORE REPLACE). RESTORE
//     REPLACE requires Redis 3.0+, which is universally satisfied.
//
// Version detection is lazy + cached on the Driver via sync.Once.
// On detection failure we assume legacy + use DUMP+RESTORE so a
// transient INFO error doesn't surface as "ERR unknown command".
// `keep`, when non-nil, restricts the copy to source keys for which
// keep(key) returns true — used by the branch_scoped capture so a bare
// main-worktree active prefix does not copy sibling-owned keys.
func (d *Driver) copyByPrefix(ctx context.Context, srcPrefix, dstPrefix string, keep func(string) bool) error {
	if srcPrefix == "" {
		return errors.New("redis: refusing to copy from empty prefix")
	}
	if d.supportsCopy(ctx) {
		return d.copyByPrefixCOPY(ctx, srcPrefix, dstPrefix, keep)
	}
	return d.copyByPrefixDumpRestore(ctx, srcPrefix, dstPrefix, keep)
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
	minorNum, err2 := strconv.Atoi(minor)
	if err1 != nil || err2 != nil {
		return false
	}
	if maj > wantMajor {
		return true
	}
	if maj == wantMajor && minorNum >= wantMinor {
		return true
	}
	return false
}

// copyByPrefixCOPY is the Redis 6.2+ fast path.
func (d *Driver) copyByPrefixCOPY(ctx context.Context, srcPrefix, dstPrefix string, keep func(string) bool) error {
	c := d.client()

	iter := c.Scan(ctx, 0, globEscape(srcPrefix)+"*", 1000).Iterator()
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
		if keep != nil && !keep(src) {
			continue
		}
		dst := dstPrefix + strings.TrimPrefix(src, srcPrefix)
		// Plain `COPY src dst REPLACE` — no `DB n` option. treeman only
		// ever operates in logical DB 0 (DBIndex), so the cross-DB
		// option is redundant, and DragonflyDB (RESP-compatible but
		// single-DB) rejects `COPY … DB 0` with a syntax error. The
		// bare form is accepted by Redis 6.2+, Valkey, and DragonflyDB.
		pipe.Do(ctx, "COPY", src, dst, "REPLACE") // REPLACE so re-runs are idempotent
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
//
//	Phase 1: pipeline DUMP + PTTL on every source key (1 round-trip).
//	Phase 2: pipeline RESTORE on every destination key (1 round-trip).
//
// Two round-trips per 100 keys is slower than COPY's one, but
// network-cost-wise it's still ~50 keys per network hop. TTLs are
// preserved by passing PTTL's result to RESTORE.
//
// Keys that vanish between SCAN and DUMP (race with a concurrent
// DEL or TTL expiry) surface as `redis.Nil` from DUMP and are
// silently skipped — the SCAN cursor only guarantees a snapshot of
// keys *eventually* visited, not that every visited key still exists.
func (d *Driver) copyByPrefixDumpRestore(ctx context.Context, srcPrefix, dstPrefix string, keep func(string) bool) error {
	c := d.client()

	iter := c.Scan(ctx, 0, globEscape(srcPrefix)+"*", 1000).Iterator()
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
		k := iter.Val()
		if keep != nil && !keep(k) {
			continue
		}
		batch = append(batch, k)
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

// client returns the pooled go-redis client pinned to the treeman
// DB index. Reused across snapshot operations — the underlying pool
// keeps connections warm.
func (d *Driver) client() *redis.Client {
	return d.clientFor(DBIndex)
}

// Client is the exported entry point used by callers outside the
// snapshot path (currently the MCP engine tools). Returns the
// pooled client; callers MUST NOT Close it — the Driver owns the
// lifecycle.
func (d *Driver) Client() *redis.Client { return d.client() }
