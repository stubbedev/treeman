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
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// IndexExists reports whether `name` is a live index in the cluster.
// Uses HEAD /<name> which returns 200 / 404.
func (d *Driver) IndexExists(ctx context.Context, name string) (bool, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, d.Base+"/"+name, nil)
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
	if sourcePrefix == templatePrefix {
		return fmt.Errorf("snapshot create: source and template prefixes must differ")
	}
	srcIndices, err := d.ListMatching(ctx, sourcePrefix)
	if err != nil {
		return fmt.Errorf("list source indices %s*: %w", sourcePrefix, err)
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
	if templatePrefix == targetPrefix {
		return fmt.Errorf("snapshot restore: template and target prefixes must differ")
	}
	tplIndices, err := d.ListMatching(ctx, templatePrefix)
	if err != nil {
		return fmt.Errorf("list template indices %s*: %w", templatePrefix, err)
	}
	if _, err := d.DropMatching(ctx, targetPrefix); err != nil {
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
		src := src
		// Map `<srcPrefix><rest>` → `<dstPrefix><rest>`.
		rest := strings.TrimPrefix(src, srcPrefix)
		dst := dstPrefix + rest
		g.Go(func() error {
			return d.cloneOneIndex(gctx, src, dst)
		})
	}
	return g.Wait()
}

// cloneOneIndex does the read-only flip + _clone + writable flip
// dance for a single index. Idempotent against pre-existing dst —
// we don't drop it here (the caller's `DropMatching` already cleared
// the prefix).
func (d *Driver) cloneOneIndex(ctx context.Context, src, dst string) error {
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
	return nil
}

func (d *Driver) cloneAPICall(ctx context.Context, src, dst string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/%s/_clone/%s", d.Base, src, dst),
		bytes.NewReader([]byte("{}")))
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
	payload, _ := json.Marshal(map[string]any{
		"index": map[string]any{
			"blocks": map[string]any{
				"write": readOnly,
			},
		},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut,
		d.Base+"/"+name+"/_settings", bytes.NewReader(payload))
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
