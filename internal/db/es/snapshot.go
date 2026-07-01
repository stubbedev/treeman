// Snapshot primitives for the Elasticsearch / OpenSearch driver.
// Mirrors the MySQL + Postgres `SnapshotCreate` / `SnapshotRestore`
// shape so the prepare orchestrator can treat ES with the same
// cache-hit / cold-build flow.
//
// ES doesn't have "databases" — it has indices. A treeman
// "namespace" for ES is a name prefix; a worktree owns every index
// whose name starts with that prefix. Cloning is per-index using
// the native `_clone` API (server-side file-level copy, very fast).
// The source index must be read-only for the duration of the clone
// — we flip the `index.blocks.write` setting on, run _clone, flip
// it off again.
//
// Template name shape: the orchestrator picks a fingerprint-keyed
// prefix (e.g. `_tm_abcdef1234`). Every index under the source
// prefix becomes `<template-prefix><source-relative-name>`.

package es

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// srcBlockRegistry refcounts `index.blocks.write` per (cluster, index)
// process-wide. `_clone` requires its SOURCE index read-only, but treeman
// clones the SHARED live parent (`dev_*`) indices concurrently from multiple
// worktree seeds (and each Driver is a fresh instance, so an instance field
// wouldn't serialize them). Without coordination, one clone's deferred unblock
// clears the read-only flag another clone still depends on and ES rejects the
// in-flight resize with "must be read-only to resize index". Refcounting keeps
// the block on until the LAST concurrent clone of that source releases it.
//
// The entry is retired from the registry when its count returns to zero, so
// the map stays bounded by the number of IN-FLIGHT source blocks, not by every
// index name ever cloned. `dead` closes the retire/lookup race: a goroutine
// that fetched a ref just before it was retired sees the flag after locking and
// re-looks-up a fresh entry rather than resurrecting the retired one.
type srcBlockRef struct {
	mu    sync.Mutex // held across the block/unblock HTTP call for this index
	count int
	dead  bool // set under mu just before the entry leaves the registry
}

var (
	srcBlockRegMu sync.Mutex
	srcBlockReg   = map[string]*srcBlockRef{}
)

// errSourceNotReadOnly tags a _clone rejection caused by the source index
// losing its write-block mid-clone (a racing external writer, or slow
// cluster-state propagation). cloneOneIndex re-asserts the block and retries.
var errSourceNotReadOnly = errors.New("clone source lost its write-block")

func srcBlockKey(base, index string) string { return base + "\x00" + index }

// srcBlockRefFor returns the live ref for (base, index), creating it if absent.
func srcBlockRefFor(base, index string) *srcBlockRef {
	srcBlockRegMu.Lock()
	defer srcBlockRegMu.Unlock()
	key := srcBlockKey(base, index)
	ref := srcBlockReg[key]
	if ref == nil {
		ref = &srcBlockRef{}
		srcBlockReg[key] = ref
	}
	return ref
}

// acquireSourceReadOnly ensures `index` is read-only, refcounted process-wide.
// The first acquirer runs `set`; later concurrent acquirers just increment. On
// `set` failure the count is NOT incremented, so the caller must not release.
func acquireSourceReadOnly(ctx context.Context, base, index string, set func(context.Context) error) error {
	for {
		ref := srcBlockRefFor(base, index)
		ref.mu.Lock()
		if ref.dead {
			// Retired between lookup and lock — a fresh entry now owns the key.
			ref.mu.Unlock()
			continue
		}
		if ref.count == 0 {
			if err := set(ctx); err != nil {
				ref.mu.Unlock()
				return err
			}
		}
		ref.count++
		ref.mu.Unlock()
		return nil
	}
}

// releaseSourceReadOnly decrements the refcount; the last releaser runs `clear`
// to flip the source writable again and retires the registry entry. Each
// release pairs with a prior successful acquire whose +1 keeps the entry alive
// until now, so the lookup below always finds this exact ref (a lookup miss
// means an unpaired release — treated as a no-op).
func releaseSourceReadOnly(ctx context.Context, base, index string, clearFn func(context.Context) error) {
	srcBlockRegMu.Lock()
	key := srcBlockKey(base, index)
	ref := srcBlockReg[key]
	srcBlockRegMu.Unlock()
	if ref == nil {
		return
	}
	ref.mu.Lock()
	defer ref.mu.Unlock()
	if ref.count == 0 {
		return
	}
	ref.count--
	if ref.count == 0 {
		_ = clearFn(ctx)
		ref.dead = true
		srcBlockRegMu.Lock()
		if srcBlockReg[key] == ref {
			delete(srcBlockReg, key)
		}
		srcBlockRegMu.Unlock()
	}
}

// IndexExists reports whether `name` is a live index in the cluster.
// Uses HEAD /<name> which returns 200 / 404.
func (d *Driver) IndexExists(ctx context.Context, name string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, d.Base+"/"+escSeg(name), nil)
	if err != nil {
		return false, err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("HEAD /%s → HTTP %d", name, resp.StatusCode)
}

// SnapshotCreate clones every index under `sourcePrefix` into a
// matching index under `templatePrefix`. Mirrors MySQL's
// SnapshotCreate in shape: after this returns, the template prefix
// holds a complete copy of the populated state.
//
// `_clone` requires the source to be read-only. We flip
// `index.blocks.write: true` on each source index, clone, then
// flip back to false so the app keeps working.
func (d *Driver) SnapshotCreate(ctx context.Context, sourcePrefix, templatePrefix string) error {
	return d.SnapshotCreateFiltered(ctx, sourcePrefix, templatePrefix, nil)
}

// SnapshotCreateFiltered is SnapshotCreate with an optional `keep`
// predicate restricting which SOURCE indices are captured: only indices
// for which keep(name) returns true are cloned into the template prefix.
// A nil predicate captures everything under sourcePrefix (classic
// behaviour). The branch_scoped swap uses this so a bare main-worktree
// active prefix — which is also a prefix of every sibling worktree's
// `<prefix>_<slug>_*` — does not pull sibling-owned indices into this
// branch's durable copy. The template (durable) prefix is hash-derived
// and never collides, so its stale-cleanup drop stays unfiltered.
func (d *Driver) SnapshotCreateFiltered(ctx context.Context, sourcePrefix, templatePrefix string, keep func(string) bool) error {
	if sourcePrefix == templatePrefix {
		return errors.New("snapshot create: source and template prefixes must differ")
	}
	srcIndices, err := d.ListMatching(ctx, sourcePrefix)
	if err != nil {
		return fmt.Errorf("list source indices %s*: %w", sourcePrefix, err)
	}
	if keep != nil {
		kept := make([]string, 0, len(srcIndices))
		for _, n := range srcIndices {
			if keep(n) {
				kept = append(kept, n)
			}
		}
		srcIndices = kept
	}
	// Drop any pre-existing template indices so the clone has clean ground.
	if _, err := d.DropMatching(ctx, templatePrefix); err != nil {
		return fmt.Errorf("drop stale template %s*: %w", templatePrefix, err)
	}
	if len(srcIndices) == 0 {
		// Nothing to clone — treat as success (the user's seed step
		// produced no indices, which can happen on a fresh build).
		return nil
	}
	return d.cloneIndices(ctx, srcIndices, sourcePrefix, templatePrefix)
}

// SnapshotRestore clones every index under `templatePrefix` into a
// matching index under `targetPrefix`. Used by the parallel fanout
// to spin up paratest-style worker prefixes.
func (d *Driver) SnapshotRestore(ctx context.Context, templatePrefix, targetPrefix string) error {
	return d.SnapshotRestoreFiltered(ctx, templatePrefix, targetPrefix, nil)
}

// SnapshotRestoreFiltered is SnapshotRestore with an optional `keep`
// predicate restricting which TARGET indices the stale-target cleanup
// may drop: only target indices for which keep(name) returns true are
// dropped before the clone. A nil predicate drops everything under
// targetPrefix (classic behaviour). The branch_scoped swap uses this so
// restoring into a bare main-worktree active prefix does not wipe sibling
// worktrees' `<prefix>_<slug>_*` indices. The template (durable) source
// is hash-derived and never collides, so its listing stays unfiltered.
func (d *Driver) SnapshotRestoreFiltered(ctx context.Context, templatePrefix, targetPrefix string, keep func(string) bool) error {
	return d.SnapshotRestoreSrcFiltered(ctx, templatePrefix, targetPrefix, nil, keep)
}

// SnapshotRestoreSrcFiltered is SnapshotRestore with independent filters
// for the SOURCE (`srcKeep`, which template indices to copy) and the
// TARGET (`tgtKeep`, which target indices the stale-cleanup may drop).
// Either nil means "all". The branch_scoped parent-seed uses srcKeep to
// copy only the parent worktree's OWN indices when the parent prefix is a
// bare main-worktree prefix that nests sibling worktrees' indices; tgtKeep
// spares the current worktree's siblings exactly as SnapshotRestoreFiltered
// does. Durable resume passes srcKeep=nil (the durable prefix is
// hash-derived and never nests).
func (d *Driver) SnapshotRestoreSrcFiltered(
	ctx context.Context,
	templatePrefix, targetPrefix string,
	srcKeep, tgtKeep func(string) bool,
) error {
	if templatePrefix == targetPrefix {
		return errors.New("snapshot restore: template and target prefixes must differ")
	}
	tplIndices, err := d.ListMatching(ctx, templatePrefix)
	if err != nil {
		return fmt.Errorf("list template indices %s*: %w", templatePrefix, err)
	}
	if srcKeep != nil {
		kept := make([]string, 0, len(tplIndices))
		for _, n := range tplIndices {
			if srcKeep(n) {
				kept = append(kept, n)
			}
		}
		tplIndices = kept
	}
	if _, err := d.DropMatchingFiltered(ctx, targetPrefix, tgtKeep); err != nil {
		return fmt.Errorf("drop stale target %s*: %w", targetPrefix, err)
	}
	if len(tplIndices) == 0 {
		return nil
	}
	return d.cloneIndices(ctx, tplIndices, templatePrefix, targetPrefix)
}

// DropSnapshot deletes every index under `templatePrefix`. Used by
// the snapshot GC sweep when the SQLite cap evicts a template row.
func (d *Driver) DropSnapshot(ctx context.Context, templatePrefix string) error {
	_, err := d.DropMatching(ctx, templatePrefix)
	return err
}

// cloneIndices fans out the per-index clone, with parallelism
// capped at 8. Each clone is now an async dispatch
// (wait_for_active_shards=0) so each goroutine spends almost no time
// holding an HTTP connection — it fires the POST, then polls _recovery
// with short-lived 500ms probes. 8 lets a typical multi-index template
// clone in one wave without overwhelming the ES management thread pool.
func (d *Driver) cloneIndices(ctx context.Context, srcIndices []string, srcPrefix, dstPrefix string) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, src := range srcIndices {
		// Map `<srcPrefix><rest>` → `<dstPrefix><rest>`.
		rest := strings.TrimPrefix(src, srcPrefix)
		dst := dstPrefix + rest
		g.Go(func() error {
			return d.cloneOneIndex(gctx, src, dst, srcPrefix, dstPrefix)
		})
	}
	return g.Wait()
}

// cloneOneIndex does the read-only flip + _clone + writable flip
// dance for a single index. Idempotent against pre-existing dst —
// we don't drop it here (the caller's `DropMatching` already cleared
// the prefix).
func (d *Driver) cloneOneIndex(ctx context.Context, src, dst, srcPrefix, dstPrefix string) error {
	// 1. Mark src read-only — refcounted process-wide so concurrent clones of
	// the SAME shared source (sibling worktrees seeding off the live `dev_*`
	// parent) don't clear each other's block mid-clone. First acquirer flips
	// it on; last releaser flips it off.
	if err := acquireSourceReadOnly(ctx, d.Base, src, func(c context.Context) error {
		return d.setIndexBlock(c, src, true)
	}); err != nil {
		return fmt.Errorf("set read-only on %s: %w", src, err)
	}
	defer func() {
		// Always release — even on clone failure — or the app can't write to
		// its data after a transient ES hiccup. The last releaser flips back on
		// a fresh, short-lived context so the unblock runs even when ctx was
		// cancelled mid-clone; otherwise the source could stay read-only
		// indefinitely.
		bgctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		releaseSourceReadOnly(bgctx, d.Base, src, func(c context.Context) error {
			return d.setIndexBlock(c, src, false)
		})
	}()

	// 2. POST /<src>/_clone/<dst>. The source must stay read-only for the whole
	// resize. If the block is cleared out from under us mid-clone — a racing
	// external writer, or slow cluster-state propagation after our PUT — ES
	// 500s with "must be read-only to resize index". Re-assert the block and
	// retry a bounded number of times. (The refcount stops treeman itself from
	// clearing it; this covers everyone else.) A rejected _clone never creates
	// dst, so no cleanup is needed between attempts.
	const maxCloneRetries = 3
	for attempt := 0; ; attempt++ {
		err := d.cloneAPICall(ctx, src, dst)
		if err == nil {
			break
		}
		if attempt >= maxCloneRetries || !errors.Is(err, errSourceNotReadOnly) {
			return err
		}
		if berr := d.setIndexBlock(ctx, src, true); berr != nil {
			return fmt.Errorf("re-assert read-only on %s: %w", src, berr)
		}
	}

	// 3. Make the clone writable. ES inherits index.blocks.write
	// from the source, so the dst comes up read-only — flip it.
	if err := d.setIndexBlock(ctx, dst, false); err != nil {
		return fmt.Errorf("clear read-only on clone %s: %w", dst, err)
	}

	// 4. Replicate aliases. `_clone` does NOT carry the source index's
	// aliases, so an app that reads through `<index>_alias` 404s against
	// the clone unless we re-create them with the prefix rewritten into
	// the destination namespace.
	if err := d.copyAliases(ctx, src, dst, srcPrefix, dstPrefix); err != nil {
		return fmt.Errorf("copy aliases %s → %s: %w", src, dst, err)
	}
	return nil
}

// copyAliases re-creates every alias on src onto dst, rewriting a leading
// srcPrefix on the alias name to dstPrefix so the alias lands in the
// destination namespace (`dev_..._alias` → `kho_<slug>_..._alias`). Alias
// metadata (filter, routing, is_write_index) is carried through verbatim.
//
// ES `_clone` copies index data + settings but not aliases, so this is the
// missing half of a faithful per-index clone. Aliases whose name does not
// start with srcPrefix are skipped: a shared/un-prefixed alias replicated
// onto a branch-scoped clone would resolve across namespaces and break the
// per-worktree isolation the prefix exists to enforce.
func (d *Driver) copyAliases(ctx context.Context, src, dst, srcPrefix, dstPrefix string) error {
	body, err := d.get(ctx, "/"+escSeg(src)+"/_alias")
	if err != nil {
		return err
	}
	var parsed map[string]struct {
		Aliases map[string]map[string]json.RawMessage `json:"aliases"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("parse alias response: %w", err)
	}

	type action struct {
		Add map[string]json.RawMessage `json:"add"`
	}
	var actions []action
	for _, idx := range parsed {
		for name, meta := range idx.Aliases {
			if !strings.HasPrefix(name, srcPrefix) {
				continue
			}
			add := make(map[string]json.RawMessage, len(meta)+2)
			maps.Copy(add, meta)
			idxJSON, err := json.Marshal(dst)
			if err != nil {
				return fmt.Errorf("marshal index name: %w", err)
			}
			aliasJSON, err := json.Marshal(dstPrefix + strings.TrimPrefix(name, srcPrefix))
			if err != nil {
				return fmt.Errorf("marshal alias name: %w", err)
			}
			add["index"] = idxJSON
			add["alias"] = aliasJSON
			actions = append(actions, action{Add: add})
		}
	}
	if len(actions) == 0 {
		return nil
	}

	payload, err := json.Marshal(map[string]any{"actions": actions})
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.Base+"/_aliases",
		bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("POST /_aliases: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("POST /_aliases: read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST /_aliases → HTTP %d: %s", resp.StatusCode, rb)
	}
	return nil
}

func (d *Driver) cloneAPICall(ctx context.Context, src, dst string) error {
	// Force 0 replicas on the clone. treeman's ES indices are ephemeral
	// per-template / per-worktree copies, typically on a single-node dev
	// cluster where a replica can never be allocated. `_clone` otherwise
	// inherits the source's replica count (usually 1), which doubles the
	// shard count and leaves every replica perpetually unassigned —
	// inflating the cluster toward max_shards_per_node until clones start
	// failing with "this action would add [N] shards". 0 keeps each clone
	// to one shard per index and the cluster green.
	cloneBody, err := json.Marshal(map[string]any{
		"settings": map[string]any{
			"index.number_of_replicas": 0,
		},
	})
	if err != nil {
		return fmt.Errorf("POST /%s/_clone/%s: marshal body: %w", src, dst, err)
	}
	// wait_for_active_shards=0: dispatch the clone and return immediately
	// without blocking until shards are active. Large indices can take
	// longer than the HTTP client's read timeout to clone server-side; the
	// async dispatch keeps each HTTP round-trip short. We poll _recovery
	// below until all shards reach DONE, with the caller's context deadline
	// (finalizeTimeout) as the safety net.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/%s/_clone/%s?wait_for_active_shards=0", d.Base, src, dst),
		bytes.NewReader(cloneBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("POST /%s/_clone/%s: %w", src, dst, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("POST /%s/_clone/%s: read body: %w", src, dst, err)
	}
	if resp.StatusCode >= 400 {
		// ES rejects the resize when the source lost its write-block between
		// our PUT and this _clone — a racing external writer or slow
		// cluster-state propagation. Tag it so the caller can re-assert + retry.
		if bytes.Contains(body, []byte("must be read-only to resize")) {
			return fmt.Errorf("POST /%s/_clone/%s → HTTP %d: %s: %w",
				src, dst, resp.StatusCode, body, errSourceNotReadOnly)
		}
		return fmt.Errorf("POST /%s/_clone/%s → HTTP %d: %s", src, dst, resp.StatusCode, body)
	}
	return d.waitForRecovery(ctx, dst)
}

// waitForRecovery polls /_recovery for index until all shards report DONE,
// sleeping 500 ms between probes. Each probe is a short HTTP call that
// comfortably fits within the driver's client timeout; the caller's context
// deadline is the overall safety net for a clone that never completes.
func (d *Driver) waitForRecovery(ctx context.Context, index string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		done, err := d.recoveryDone(ctx, index)
		if err != nil {
			return fmt.Errorf("recovery poll %s: %w", index, err)
		}
		if done {
			return nil
		}
	}
}

// recoveryDone fetches /_recovery for index and reports whether every shard
// is in the DONE stage. Returns (false, nil) on HTTP 404 — the index may
// not be visible immediately after the async clone POST.
func (d *Driver) recoveryDone(ctx context.Context, index string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		d.Base+"/"+escSeg(index)+"/_recovery?human=false", nil)
	if err != nil {
		return false, err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil // index not yet visible; keep polling
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("GET /%s/_recovery → HTTP %d: %s", index, resp.StatusCode, body)
	}
	var parsed map[string]struct {
		Shards []struct {
			Stage string `json:"stage"`
		} `json:"shards"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, fmt.Errorf("parse recovery response: %w", err)
	}
	total := 0
	for _, idx := range parsed {
		for _, shard := range idx.Shards {
			total++
			if shard.Stage != "DONE" {
				return false, nil
			}
		}
	}
	return total > 0, nil // total == 0: recovery not yet started
}

// setIndexBlock toggles `index.blocks.write` on the given index.
// When true, the index is read-only (required for the clone source);
// when false, the app can write again.
func (d *Driver) setIndexBlock(ctx context.Context, name string, readOnly bool) error {
	payload, err := json.Marshal(map[string]any{
		"index": map[string]any{
			"blocks": map[string]any{
				"write": readOnly,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("PUT /%s/_settings: marshal payload: %w", name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		d.Base+"/"+escSeg(name)+"/_settings", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("PUT /%s/_settings: read body: %w", name, err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("PUT /%s/_settings → HTTP %d: %s", name, resp.StatusCode, body)
	}
	return nil
}
