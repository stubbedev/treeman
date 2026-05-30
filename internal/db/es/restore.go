package es

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/stubbedev/treeman/internal/db/dumpload"
)

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
// Bulk POSTs are chunked at ~10 MiB so large dumps don't OOM the
// ES server. Each chunk is checked for the `errors: true` flag in
// the response and surfaces with a clear error.
func (d *Driver) Restore(ctx context.Context, sourcePrefix, dumpPath string) error {
	rc, _, err := dumpload.OpenDump(dumpPath)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	const maxBatch = 10 << 20 // 10 MiB

	var buf bytes.Buffer
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 1<<16), 64<<20) // up to 64 MB per line
	sub := []byte("{target_db}")
	pref := []byte(sourcePrefix)

	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Base+"/_bulk", bytes.NewReader(buf.Bytes()))
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
		buf.Reset()
		return nil
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.Contains(line, sub) {
			line = bytes.ReplaceAll(line, sub, pref)
		}
		buf.Write(line)
		buf.WriteByte('\n')
		if buf.Len() >= maxBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read dump: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	// Refresh affected indices so subsequent SnapshotCreate sees
	// the bulk-loaded docs immediately.
	if err := d.refresh(ctx, sourcePrefix+"*"); err != nil {
		return fmt.Errorf("es refresh %s*: %w", sourcePrefix, err)
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
