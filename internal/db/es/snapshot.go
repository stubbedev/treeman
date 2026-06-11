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
	"time"

	"golang.org/x/sync/errgroup"
)

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
// capped at 4 — ES _clone is server-side fast but uses the
// destination shard's primary, so flooding the management thread
// pool with concurrent clones is wasteful.
func (d *Driver) cloneIndices(ctx context.Context, srcIndices []string, srcPrefix, dstPrefix string) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
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
	// 1. Mark src read-only.
	if err := d.setIndexBlock(ctx, src, true); err != nil {
		return fmt.Errorf("set read-only on %s: %w", src, err)
	}
	defer func() {
		// Always flip back — even on clone failure — or the app
		// can't write to its data after a transient ES hiccup.
		// Use a fresh, short-lived context so the unblock runs
		// even when ctx has been cancelled mid-clone; otherwise
		// the source index could stay read-only indefinitely.
		bgctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = d.setIndexBlock(bgctx, src, false)
	}()

	// 2. POST /<src>/_clone/<dst>
	if err := d.cloneAPICall(ctx, src, dst); err != nil {
		return err
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/%s/_clone/%s", d.Base, src, dst),
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
		return fmt.Errorf("POST /%s/_clone/%s → HTTP %d: %s", src, dst, resp.StatusCode, body)
	}
	return nil
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
