package es

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/db/dumpload"
)

// bulkFanout bounds how many _bulk POSTs are in flight at once. ES
// indexes bulk payloads server-side with its own thread pool, so 2–3
// concurrent chunks keep the pipe full (the client otherwise idles
// while the server indexes); more mostly grows server-side queueing.
const bulkFanout = 3

// bulkMaxBatch caps one _bulk chunk at ~10 MiB so large dumps don't
// OOM the ES server. Variable (not const) only so tests can shrink it
// to exercise multi-chunk pairing.
var bulkMaxBatch = 10 << 20

// Restore streams a `_bulk`-format NDJSON dump into Elasticsearch.
// Compression (gzip/zstd/bzip2/xz) is auto-detected from the dump
// file's magic bytes.
//
// The dump file is NDJSON in the canonical bulk format: pairs of
// action + doc lines, e.g.
//
//	{"index": {"_index": "{target_db}foo", "_id": "1"}}
//	{"field": "value"}
//
// Every literal `{target_db}` token in the file is replaced with
// sourcePrefix before posting, mirroring the substitution
// `runner.Run` does for migrate/seed env values. That's how the
// dump expresses "place this index under the current worktree's
// prefix" without baking a specific prefix in.
//
// Bulk POSTs are chunked at ~10 MiB so large dumps don't OOM the ES
// server, cut only on action-pair boundaries (an index/create/update
// action line and its doc line always travel in the same chunk), and
// posted with up to bulkFanout chunks in flight. Chunk concurrency
// assumes the usual dump shape — each document appears once — since
// two operations on the same _id in different chunks have no ordering
// guarantee. Each chunk's response is checked for the `errors: true`
// flag and surfaces with a clear error.
func (d *Driver) Restore(ctx context.Context, sourcePrefix, dumpPath string) error {
	rc, _, err := dumpload.OpenDump(dumpPath)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(bulkFanout)

	var buf bytes.Buffer
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 1<<16), 64<<20) // up to 64 MB per line
	sub := []byte("{target_db}")
	pref := []byte(sourcePrefix)

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		payload := make([]byte, buf.Len())
		copy(payload, buf.Bytes())
		buf.Reset()
		g.Go(func() error { return d.bulkPost(gctx, payload) })
	}

	// expectDoc tracks bulk-format pairing: after an index/create/update
	// action line the next line is its document and must stay in the
	// same chunk (a split pair makes ES read the doc line as an action).
	expectDoc := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.Contains(line, sub) {
			line = bytes.ReplaceAll(line, sub, pref)
		}
		if expectDoc {
			expectDoc = false
		} else {
			expectDoc = actionNeedsDoc(line)
		}
		buf.Write(line)
		buf.WriteByte('\n')
		if buf.Len() >= bulkMaxBatch && !expectDoc {
			flush()
		}
	}
	if err := scanner.Err(); err != nil {
		_ = g.Wait()
		return fmt.Errorf("read dump: %w", err)
	}
	flush()
	if err := g.Wait(); err != nil {
		return err
	}
	// Refresh affected indices so subsequent SnapshotCreate sees
	// the bulk-loaded docs immediately.
	if err := d.refresh(ctx, sourcePrefix+"*"); err != nil {
		return fmt.Errorf("es refresh %s*: %w", sourcePrefix, err)
	}
	return nil
}

// actionNeedsDoc reports whether a bulk action line is followed by a
// document line (index/create/update are; delete is not). Non-action
// garbage defaults to false so a malformed dump still surfaces as an
// ES error rather than desynced pairing here.
func actionNeedsDoc(line []byte) bool {
	var action struct {
		Index  json.RawMessage `json:"index"`
		Create json.RawMessage `json:"create"`
		Update json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(line, &action); err != nil {
		return false
	}
	return action.Index != nil || action.Create != nil || action.Update != nil
}

// bulkPost ships one NDJSON chunk to /_bulk and fails on transport
// errors, HTTP >= 400, or the response's per-item `errors` flag.
func (d *Driver) bulkPost(ctx context.Context, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Base+"/_bulk", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("es _bulk: read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("es _bulk %s: %s", resp.Status, string(body))
	}
	// Parse the top-level "errors" field rather than substring-
	// matching the body — a doc echoed back in `items` could
	// otherwise produce a false positive.
	var hdr struct {
		Errors bool `json:"errors"`
	}
	if err := json.Unmarshal(body, &hdr); err != nil {
		return fmt.Errorf("es _bulk: parse response: %w", err)
	}
	if hdr.Errors {
		return fmt.Errorf("es _bulk reported per-item errors: %s", truncate(string(body), 1024))
	}
	return nil
}

// refresh forces a refresh on the indices matching pattern so any
// just-bulk-loaded docs are searchable + visible to _clone.
func (d *Driver) refresh(ctx context.Context, pattern string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Base+"/"+escSeg(pattern)+"/_refresh", nil)
	if err != nil {
		return err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body) // best-effort body for the error message
		return fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
