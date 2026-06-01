// Package es is the treeman Elasticsearch / OpenSearch driver.
// Pure HTTP via net/http — no fat client dependency.
package es

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// Driver wraps an http.Client + base URL.
type Driver struct {
	Base string
	HTTP *http.Client
}

// escSeg percent-escapes a single URL path segment (an index name or
// name prefix) so characters outside the index-name charset cannot
// alter the request path or inject query parameters. Valid ES index
// names (lowercase letters, digits, `-_.+`) pass through unchanged, so
// the common case stays byte-identical.
func escSeg(s string) string { return url.PathEscape(s) }

// Connect probes reachability + returns a Driver. Auth (api key /
// basic) is left for a future patch; the local-dev target uses
// unauthenticated Elasticsearch.
func Connect(ctx context.Context, cfg config.EsConn) (*Driver, error) {
	url := cfg.URL
	if cfg.Container != "" || cfg.ComposeService != "" {
		opts := containerip.Opts{
			Container:      cfg.Container,
			ComposeService: cfg.ComposeService,
			ComposeProject: cfg.ComposeProject,
			Engine:         cfg.ContainerEngine,
			Network:        cfg.Network,
			InternalPort:   containerip.URIPort(url, 9200),
		}
		addr, err := containerip.ResolveAddr(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("resolve container: %w", err)
		}
		if addr != nil {
			url = containerip.RewriteHostPortInURIWithPort(url, addr.Host, addr.Port)
		}
	}
	if err := reachability.ProbeURLCtx(ctx, "elasticsearch", url); err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	if cfg.PoolMax > 0 {
		// Cap simultaneous + idle HTTP connections to the cluster, the
		// HTTP analogue of the SQL engines' pool_max.
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("es connect: http.DefaultTransport is %T, not *http.Transport", http.DefaultTransport)
		}
		tr := base.Clone()
		tr.MaxConnsPerHost = int(cfg.PoolMax)
		tr.MaxIdleConnsPerHost = int(cfg.PoolMax)
		httpClient.Transport = tr
	}
	return &Driver{
		Base: strings.TrimRight(url, "/"),
		HTTP: httpClient,
	}, nil
}

// EngineVersion fetches the cluster root, parses the `version.number`
// field. Empty string + nil error if missing.
func (d *Driver) EngineVersion(ctx context.Context) (string, error) {
	body, err := d.get(ctx, "/")
	if err != nil {
		return "", err
	}
	var v struct {
		Version struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", err
	}
	return v.Version.Number, nil
}

// DropMatching deletes every index whose name starts with prefix.
// ES rejects wildcard DELETE when
// action.destructive_requires_name=true (the cluster default), so
// list-then-delete-by-name.
//
// Deletes fan out in parallel (limit 8) — DELETE /index is a single
// HTTP round trip with no contention between indices, and ES happily
// fields concurrent admin requests up to thread_pool.management's
// queue size.
func (d *Driver) DropMatching(ctx context.Context, prefix string) ([]string, error) {
	return d.DropMatchingFiltered(ctx, prefix, nil)
}

// DropMatchingFiltered is DropMatching with an optional `keep`
// predicate: only indices for which keep(name) returns true are
// dropped. A nil predicate is "keep everything matching", i.e. the
// classic DropMatching behaviour.
//
// Cold-build uses this so sibling worktrees' branch-scoped indices
// that share the current worktree's source prefix (e.g. main wt
// prefix `kho` is also a prefix of every other wt's `kho_<slug>_*`)
// survive the eager pre-build drop.
func (d *Driver) DropMatchingFiltered(ctx context.Context, prefix string, keep func(string) bool) ([]string, error) {
	all, err := d.ListMatching(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var names []string
	if keep == nil {
		names = all
	} else {
		names = make([]string, 0, len(all))
		for _, n := range all {
			if keep(n) {
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	g, gctx := errgroup.WithContext(ctx)
	limit := max(min(8, len(names)), 1)
	g.SetLimit(limit)
	for _, n := range names {
		g.Go(func() error {
			if _, err := d.delete(gctx, "/"+escSeg(n)); err != nil {
				return fmt.Errorf("DELETE %s: %w", n, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return names, nil
}

// ListMatching returns every index whose name starts with prefix.
func (d *Driver) ListMatching(ctx context.Context, prefix string) ([]string, error) {
	body, err := d.get(ctx, "/_cat/indices/"+escSeg(prefix)+"*?h=index&format=json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Index string `json:"index"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		// Fallback: text format (older ES versions return blank).
		var out []string
		for line := range strings.SplitSeq(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		return out, nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Index)
	}
	return out, nil
}

// StoreSizeBytes sums the on-disk store.size (in bytes) of every index
// matching `<prefix>*`, or 0 on error / no matching indices. Used by the
// MCP snapshot-inspect size estimate.
func (d *Driver) StoreSizeBytes(ctx context.Context, prefix string) (int64, error) {
	body, err := d.get(ctx, "/_cat/indices/"+escSeg(prefix)+"*?bytes=b&h=store.size&format=json")
	if err != nil {
		return 0, err
	}
	var rows []struct {
		Size string `json:"store.size"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return 0, err
	}
	var total int64
	for _, r := range rows {
		if n, perr := strconv.ParseInt(strings.TrimSpace(r.Size), 10, 64); perr == nil {
			total += n
		}
	}
	return total, nil
}

// WriteWatermark returns a sound, monotonic, per-prefix write-counter
// token: the summed primaries indexing.index_total + delete_total across
// every index matching `prefix*`, from the _stats/indexing API. Those
// counters are always tracked and only increase, so an unchanged token
// proves no document was indexed or deleted under the prefix in between.
// Any transport/parse error (including a not-yet-created prefix → HTTP
// 404) returns "" so the caller declines to skip work.
func (d *Driver) WriteWatermark(ctx context.Context, prefix string) (string, error) {
	return d.WriteWatermarkFiltered(ctx, prefix, nil)
}

// WriteWatermarkFiltered is WriteWatermark restricted to the indices for
// which keep(name) returns true. A nil predicate sums every index under
// `prefix*` (classic behaviour). The branch_scoped swap passes the
// adapter's sibling filter so a bare main-worktree active prefix yields a
// watermark counting only THIS worktree's writes — without it, a sibling
// worktree's writes would bump the counter and needlessly force a capture.
// Parses the per-index `level=indices` view so the filter can drop
// sibling-owned indices before summing.
func (d *Driver) WriteWatermarkFiltered(ctx context.Context, prefix string, keep func(string) bool) (string, error) {
	body, err := d.get(ctx, "/"+escSeg(prefix)+"*/_stats/indexing?level=indices&format=json")
	if err != nil {
		return "", err
	}
	var parsed struct {
		Indices map[string]struct {
			Primaries struct {
				Indexing struct {
					IndexTotal  int64 `json:"index_total"`
					DeleteTotal int64 `json:"delete_total"`
				} `json:"indexing"`
			} `json:"primaries"`
		} `json:"indices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	var total int64
	for name, idx := range parsed.Indices {
		if keep != nil && !keep(name) {
			continue
		}
		total += idx.Primaries.Indexing.IndexTotal + idx.Primaries.Indexing.DeleteTotal
	}
	return "es:" + strconv.FormatInt(total, 10), nil
}

func (d *Driver) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: read body: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("GET %s → HTTP %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

func (d *Driver) delete(ctx context.Context, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, d.Base+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		// Already gone — treat as success for idempotent teardown.
		return resp.StatusCode, nil
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("DELETE %s → HTTP %d", path, resp.StatusCode)
	}
	return resp.StatusCode, nil
}
