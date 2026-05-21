// Package es is the treeman Elasticsearch / OpenSearch driver.
// Pure HTTP via net/http — no fat client dependency.
package es

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// Connect probes reachability + returns a Driver. Auth (api key /
// basic) is left for a future patch; the local-dev target uses
// unauthenticated Elasticsearch.
func Connect(ctx context.Context, cfg config.EsConn) (*Driver, error) {
	url := cfg.URL
	if cfg.Container != "" {
		ip, err := containerip.Resolve(cfg.Container, cfg.ContainerEngine)
		if err != nil {
			return nil, fmt.Errorf("resolve container %q: %w", cfg.Container, err)
		}
		if ip != "" {
			url = containerip.RewriteHostPortInURI(url, ip)
		}
	}
	if err := reachability.ProbeURL("elasticsearch", url); err != nil {
		return nil, err
	}
	return &Driver{
		Base: strings.TrimRight(url, "/"),
		HTTP: &http.Client{Timeout: 30 * time.Second},
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
	names, err := d.ListMatching(ctx, prefix)
	if err != nil {
		return nil, err
	}
	g, gctx := errgroup.WithContext(ctx)
	limit := 8
	if limit > len(names) && len(names) > 0 {
		limit = len(names)
	}
	if limit < 1 {
		limit = 1
	}
	g.SetLimit(limit)
	for _, n := range names {
		n := n
		g.Go(func() error {
			if _, err := d.delete(gctx, "/"+n); err != nil {
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
	body, err := d.get(ctx, "/_cat/indices/"+prefix+"*?h=index&format=json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Index string `json:"index"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		// Fallback: text format (older ES versions return blank).
		var out []string
		for _, line := range strings.Split(string(body), "\n") {
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

func (d *Driver) get(ctx context.Context, path string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.Base+path, nil)
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("GET %s → HTTP %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

func (d *Driver) delete(ctx context.Context, path string) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, d.Base+path, nil)
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
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
